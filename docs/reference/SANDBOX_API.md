# Sandbox API 与配置参考

核对日期：2026-11-01。代码基线：`e8c7ebf`。本页描述已实现 API；[后续路线图](../design/SANDBOX_ROADMAP.md)中的接口均为提案，不能按现有 API 调用。首次接入见[使用指南](../guides/SANDBOX.md)，业务组合见[场景指南](../guides/SANDBOX_SCENARIOS.md)。

## 模块边界

| 导入路径 | 用途 | 是否直接依赖供应商 SDK |
| --- | --- | --- |
| `github.com/kart-io/goagent/harness/env` | Executor、FileSystem、Command、Result、WriteIntent | 否 |
| `github.com/kart-io/goagent-harness/sandbox` | Provider、Registry、Session、生命周期 | 否 |
| `github.com/kart-io/goagent-harness/preset` | Host.Sandboxes 与声明式装配 | 否 |
| `github.com/kart-io/goagent-harness/sandbox/opensandbox` | OpenSandbox 可选实现 | 是，独立 Go 模块 |

来源：[类型](../../sandbox/types.go)、[Registry](../../sandbox/registry.go)、[Session](../../sandbox/session.go)、[Provider](../../sandbox/opensandbox/provider.go)。

## Provider 与 Environment

```go
type Provider interface {
    Version() string
    Create(context.Context, Request) (Environment, error)
    Connect(context.Context, string, Request) (Environment, error)
}
type Request struct {
    Provider, Scope, Image string
    Network bool
}
type Binding struct {
    ID string
    Retain bool
}
type Environment interface {
    ID() string
    Root() string
    FileSystem() env.FileSystem
    Executor() env.Executor
    Isolated() bool
    Close(context.Context) error
    Destroy(context.Context) error
}
```

Provider.Version 是适配器身份，不是远端部署的运行时证明。当前 OpenSandbox 返回 `opensandbox/1.2.0+harness-fs1`；SDK 常量和 server `/version` 另行检查，execd 使用宿主显式部署声明。

Environment.Close 断开本地视图；Destroy 删除远端实例。直接使用 Provider 时，调用者自行管理这两种动作及活动操作。需要取消、等待、所有权管理时使用 Registry/Session。Provider 可以返回部分 Environment 和 error；该资源仅用于接管清理，不代表已可执行。

## Registry

```go
type Registry struct {
    Namespace string
    Providers map[string]Provider
    Resolve func(Request) (Binding, error)
    // 其余为内部状态。
}
func (*Registry) Open(context.Context, Request) (*Session, error)
func (*Registry) Close(context.Context) error
```

| 字段/操作 | 契约 |
| --- | --- |
| Namespace | 非空，业务任务/恢复域；不替代租户认证与授权 |
| Providers | 宿主命名注册表；初始化完成后不可并发修改 |
| Resolve | 宿主回调；收到已加 Namespace 的 Scope；当前签名没有 context，必须及时返回 |
| Open | 无 ID 时创建，有 ID 时连接；同 Scope 的重复 Open 不会自动去重或共享实例 |
| Close | 关闭准入、取消 pending/活动操作，等待真实返回并回收；失败可带新 context 重试 |

最终 Scope 为 `len(namespace):namespace:scope`，长度是 Go 字符串字节数。Preset 的原始 Scope 是包含 preset、委派组件和 workspace 组件路径的 JSON 数组。Resolver 应以完整作用域查找宿主持久记录，不通过拆分冒号恢复业务身份。

Open 返回非空 Session 时，必须先接管该会话的清理再处理 error。Registry 仍保留兜底责任。Close 后不能重开 Registry，下一业务任务应新建。Registry 是内存中的资源管理器，不是持久任务库、去重服务、租户限流器或跨进程租约服务。

## Session 与所有权

```go
func (*Session) Info() Info
func (*Session) Executor() env.Executor
func (*Session) FileSystem() env.FileSystem
func (*Session) BeginClose()
func (*Session) Close(context.Context) error
```

`Info` 包含 `ID, Root, Provider, Version`、`Isolated, Owned, Retained`、`Capabilities env.Capabilities`。

| Binding | Owned | 正常关闭动作 |
| --- | --- | --- |
| 空 | true | 等待活动操作 → Destroy → Close |
| Retain=true，ID为空 | true | 等待活动操作 → Close，宿主另行删除实例 |
| ID非空 | false | 等待活动操作 → Close，不删除借用实例 |

Retain 是所有权选择，不是“失败时也自动回收”的承诺；恢复失败、保留实例最终回收均需宿主处理。BeginClose 只取消和关闭准入，不等待，也不保证资源已删除。Session.Close 超时之后仍可重试。Assembly.Close 沿用一次性结果缓存；宿主必须保留 Registry.Close 的重试路径。

## 可选 FileSystem 能力

```go
type ContextResolver interface {
    ResolveContext(context.Context, string) (string, error)
}
type MutationProtector interface {
    WithProtectedNames(names ...string) env.FileSystem
}
```

远端 FS 必须实现 ContextResolver，Registry 才会接受其 Remote 能力。Session 在底层支持时保留 MutationProtector；不支持时不虚报。规则按路径组成项匹配，例如 `.git` 同时匹配根目录和子仓库中的 `.git`，只限制 FileSystem 变更，不限制命令。

内部链接可先 Resolve 得到规范路径；WriteFile/Remove 在实际变更前拒绝重新定向的别名。保护规则需要到达最终后端，不能只在工具入参上检查。OpenSandbox 的 CAS 仅协调辅助程序写者；普通命令和外部编辑器不参与锁协议。

## OpenSandbox Options

`opensandbox.New(Options) (sandbox.Provider, error)`：

| 字段 | 零值/默认 | 校验与实际含义 |
| --- | --- | --- |
| ExecdImage | 无默认，必填 | 必须等于 SupportedExecdImage；宿主保证控制服务实际采用该 digest |
| Connection | 官方 SDK ConnectionConfig | UseServerProxy 必须 true；Retry 必须 nil；控制服务同源代理，禁止重定向 |
| Root | `/workspace/goagent` | 绝对规范路径，不能为 `/`；不是宿主挂载路径 |
| Python | `python3` | Linux 客体镜像中的可执行文件 |
| TTL | 10分钟 | **本地接受1秒～24小时，固定服务端创建最少60秒**；服务端仍可进一步限制最大值 |
| CleanupTimeout | 5秒 | 独立的中断/清理期限；不是服务端销毁的固定耗时保证 |
| OperationTimeout | 30秒 | 每次 Executor/辅助文件操作的期限，不是整个任务期限 |
| StartupTimeout | 120秒 | 创建/连接的版本检查、状态、命令服务就绪与文件初始化期限 |
| PollInterval | 50ms | 状态/日志稳定等待的间隔；调低会增加请求数 |
| MaxOutputBytes | 1MiB | 普通命令返回输出上限；超限中断并返回错误 |
| MaxResponseBytes | 8MiB | 单次 HTTP 响应限制；最大128MiB，需覆盖编码后的文件与元数据 |
| MaxFileBytes | 4MiB | 文件读写限制；允许1字节～64MiB，不是磁盘总配额 |
| MaxWalkEntries | 10000 | 包含起点的遍历项数；不是工作区文件总数限制 |
| ResourceLimits | cpu=500m，memory=256Mi | 传给供应商；服务端和实际容器负责落实。没有宿主业务任务总配额管理 |

`MaxResponseBytes >= MaxFileBytes*4/3+65536`，`1 <= MaxOutputBytes <= MaxResponseBytes`；负 duration 拒绝，零值使用默认。coding 工具还有自己的超时和输出限制，应同时检查。CPU/内存不是供应商请求之外的“独立保证”；本轮 inspect 证据见[调研报告](../evaluation/2026-11-01-sandbox-research/REPORT.md)。

### TTL 已发现的兼容性差距

真实服务验证：TTL=30s 时 New 成功，Create 返回 HTTP422、Environment=nil，没有本轮分配。固定上游 `CreateSandboxRequest.timeout` 要求 `ge=60`。因此目前应配置至少60秒；“New 成功”不代表服务端接纳所有选项。修正本地最小值属于路线图 S0，不在本次文档调研中伪报为已修复。

Connection.Headers、APIKey、HTTPClient 仅由宿主提供。自定义 RoundTripper 必须遵守 context。当前没有每请求选择网络策略、VM运行时、Volume、SnapshotID 或注入秘密的 Request 字段。

## 错误与后续动作

| 错误 | 何时出现 | 后续动作 |
| --- | --- | --- |
| sandbox.ErrClosed | 会话准入已关闭 | 不再在原 Session 发起工作 |
| sandbox.ErrUnsupported | 能力、网络、版本或部署声明不符 | 调整宿主配置，不能静默降级 |
| sandbox.ErrUnconfirmed | 创建结果、命令启动/中断结果未确认 | 持久记录身份并对账，不自动重放业务命令 |
| sandbox.ErrOutputLimit | 命令输出超限 | 检查停止结果；若同时 Unconfirmed 按未确认处理 |
| opensandbox.ErrFileLimit / ErrWalkLimit | 文件/遍历限额 | 缩小输入，或由宿主审查后调整上限 |
| env.ErrExists / ErrStale | 条件写冲突 | 重新读取，决定合并或重试 |
| env.ErrOutsideWorkspace | 路径越界 | 修正相对路径 |
| fs.ErrPermission | 受保护路径或权限不足 | 不绕过保护；Git操作走已授权工具 |

UnconfirmedError 含 SandboxID、CommandID、Cause；无命令ID的未知创建使用 `create:<owner-token>`。`errors.Is`/`errors.As` 可能同时识别 DeadlineExceeded 和 Unconfirmed：**先处理 Unconfirmed**。当前 Environment 遇到未确认命令会拒绝后续操作；没有公开的“清除污染”或任意命令重连恢复接口。
