# Sandbox 使用与功能说明

本功能位于 `kart-io/goagent-harness`。GoAgent 提供 `harness/env` 接口和 Agent/workflow 执行引擎；Harness 负责装配和资源生命周期。OpenSandbox SDK 位于独立模块 `github.com/kart-io/goagent-harness/sandbox/opensandbox`。根模块的普通使用者不会下载供应商 SDK。

设计与接口契约见 [设计文档](../design/SANDBOX.md)，复跑证据见 [验证报告](../evaluation/2026-11-01-sandbox/REPORT.md)。

## 已实现的功能

| 功能 | 行为 |
| --- | --- |
| Provider 替换 | 宿主注入命名 Provider，复用 GoAgent 的 Executor / FileSystem |
| 会话生命周期 | Create、Connect、owned/borrowed、Retain、可重试关闭、迟到创建回收 |
| Preset | `executor: sandbox`，provider/image/network 校验，构造失败清理 |
| 子 Agent | 根据完整装配路径分配 Scope，默认独立环境；创建前拒绝权限和网络扩张 |
| 命令 | 原生 Argv、工作目录限定、合并输出、非零退出码、输出上限、异步中断及停止确认 |
| 工作区 | Resolve、Stat、ReadFile、WriteFile、Remove、Walk，二进制内容与 SHA256 版本 |
| 条件写 | CreateIfAbsent、IfVersion、辅助程序间 flock、原子替换与 fsync |
| 恢复 | 真实实例 ID 入 Manifest，宿主保留并重连同一个实例后核对 Manifest |
| 固定任务 | workflow 节点直接使用会话，不需要额外模型或子 Agent |

## OpenSandbox 环境要求

首期验证组合为 **Docker OpenSandbox server/execd release-1.2.0 + Go SDK v1.2.0**。SDK 模块声明仍为 `github.com/alibaba/OpenSandbox/sdks/sandbox/go`。供应商地址和密钥由宿主提供，不放入模型工具参数或 preset 配置。

沙盒镜像需要 Linux、POSIX 文件能力、`python3` 和 `sleep infinity`。创建时会实际运行文件辅助程序；缺少运行时会失败，Registry 保留已分配资源的清理责任。工作区默认 `/workspace/goagent`，可由宿主 `Options.Root` 修改。初始化不自动上传宿主项目；通过远端 FileSystem 写入输入、读取产物。

本适配器要求 `Connection.UseServerProxy=true`，数据端点必须与控制服务同源，HTTP 重定向关闭。服务必须由宿主按 Docker 后端部署；适配器不把一个地址自动证明为可信隔离环境。首期仅支持 `Network:true`，`false` 在分配前返回 `sandbox.ErrUnsupported`。不包含已经验证的出站阻断、Kubernetes 或微虚机后端。

## 本地构建与案例

当前分支尚未发布。可选模块引用本轮根模块提交，需要在克隆根目录建立临时 workspace；根模块独立运行不需要此步骤。

采用以下完整临时配置，不修改仓库文件：

```sh
TASK_WORK=$(mktemp -d)
TASK_REPO=$(pwd)
(cd "$TASK_WORK" && GOWORK=off go work init "$TASK_REPO" "$TASK_REPO/sandbox/opensandbox")
GOWORK="$TASK_WORK/go.work" go work edit -replace=github.com/kart-io/goagent-harness@v0.0.0-20261101021826-231f713ad219="$TASK_REPO"
export GOWORK="$TASK_WORK/go.work"
(cd sandbox/opensandbox && go test -race ./...)
```

宿主需 Go 1.25。存在旧的全局 GOROOT 时，命令使用 `env -u GOROOT GOTOOLCHAIN=go1.25.0` 前缀。

```sh
# 已有的可信 Docker 控制服务；示例使用 HTTP 本机/开发环境。
export HARNESS_SANDBOX_DOMAIN=127.0.0.1:8080
export HARNESS_SANDBOX_API_KEY='your-key'
export HARNESS_SANDBOX_IMAGE='your-linux-python3-image'
(cd sandbox/opensandbox && go run ./examples/coding)
(cd sandbox/opensandbox && go run ./examples/workflow)
```

`examples/coding` 通过现有 coding Workspace 和 Dispatcher 执行 write_file/read_file；同一组 Dispatcher.Tools 可传给 GoAgent Builder。`examples/workflow` 执行 prepare → execute 两个固定节点，上传输入、运行 Python、读取结果，不依赖模型密钥。示例默认创建独立实例并在结束时销毁，错误返回包含清理失败。

需要从零复跑本轮验证时：

```sh
python3 scripts/verify-sandbox.py --output /tmp/harness-sandbox-results
```

脚本建立仅监听本机的临时控制服务、固定镜像 digest、独立数据库和临时密钥，运行真实适配器测试与两个案例，最后只清理本轮服务及其沙盒。需要 Docker、Python3、Go 和镜像拉取权限；服务容器需要访问 Docker socket。脚本不修改宿主 Docker 配置或清理其他项目容器。

## 宿主接入

完整可编译代码见 [共享示例宿主](../../sandbox/opensandbox/examples/internal/demo/session.go)。核心装配为：

```go
provider, err := opensandbox.New(opensandbox.Options{
    Connection: sdk.ConnectionConfig{
        Domain: domain, Protocol: "https", APIKey: key,
        UseServerProxy: true,
    },
})
// 检查 err 后：
registry := &sandbox.Registry{
    Namespace: "business-task-123",
    Providers: map[string]sandbox.Provider{"opensandbox": provider},
}
// preset 宿主：Host{Root: localConfigRoot, Sandboxes: registry, ...}
// 或直接 registry.Open(ctx, sandbox.Request{Provider: "opensandbox",
//     Scope: "fixed-task", Image: image, Network: true})
```

Registry 配置在使用期间保持不可变。`Namespace` 是业务任务/恢复域，不能所有租户共用一个常量。Registry 会把 Namespace 和装配 Scope 组合后交给 Provider；Scope 包含 preset、组件和子 Agent 完整路径。

`Open` 可以同时返回非空 Session 和 error；必须先接管非空会话的清理，再处理错误。宿主无论装配成功与否，都应在任务结束时使用独立、限时 context 调用 Registry.Close。Close 超时可再次调用；创建返回迟到资源、销毁失败时，责任仍保留在 Registry 中。不要仅依赖 Assembly.Close：它沿用现有一次性调用约定，Registry 是失败清理与重试的兜底。

## Preset 配置

```yaml
presets:
  - id: remote-coder
    components:
      - id: workspace
        use: workspace
        config:
          executor: sandbox
          provider: opensandbox
          image: your-linux-python3-image
          sandbox: workspace-write
          network: true
          commandTimeout: 30s
          maxOutputBytes: 1048576
      - id: coding
        use: coding-tools
        config:
          tools: [read_file, write_file, edit_file, glob, grep, run_command]
      - id: permissions
        use: permissions
        config:
          base: coding
```

`base: coding` 放行文件读取和工作区编辑；命令仍需宿主 Approver 审批。漏配 permissions 时默认 Ask，不能把工具可见误当成已授权。

按现有 [Preset 信任规则](../../preset/README.md) 授权；executor 授权名称为 `sandbox:opensandbox`，镜像、网络仍受 Needs/Limits 约束。`Host.Root` 是本地配置与信任身份，实际 Workspace.Root 来自远端，二者没有隐式挂载关系。

`workspace-write` 需要真正隔离；`Remote=true` 本身不是隔离声明。`read-only` 包裹文件接口，同时本适配器没有 ReadOnlyRoot，因此移除命令工具；单纯文件只读不代表命令不能写。父子权限/网络限制在远端分配前检查，子 Agent 默认独立实例，不支持跨 Scope 隐式共享写工作区。

## 所有权和恢复

| Registry.Resolve 的 Binding | 关闭行为 |
| --- | --- |
| 默认空 Binding | Create，owned，销毁后断开 |
| `{Retain:true}` | Create，owned，但只断开，宿主接管后续销毁 |
| `{ID:existing}` | Connect，borrowed，只断开 |

恢复时保持 Namespace 和装配 Scope，宿主 Resolver 返回此前保存的 ID。OpenSandbox 会核对版本、Root/Scope/Image 摘要、实际镜像与网络标签，不允许把任意无关实例接进来。摘要采用无 padding 的 base32 SHA256，以满足服务端标签限制；不会在标签存放原始任务内容。Manifest 仍记录真实 SandboxID 和 Provider 版本。新建的空实例即使配置相同也有不同 Manifest；Retain → 同实例 Connect 才能保持相同执行身份。

TTL 默认 10 分钟。本适配器不自动续期；需要更长任务时在宿主设置 TTL（1 秒至 24 小时，服务端仍可收窄）。Retain 不等于永久保留。持久 workflow 需由宿主 PinManifest，且业务操作需要自己的幂等键；连接恢复不意味着任意命令可安全重放。

## 失败、取消与边界

| 结果 | 处理 |
| --- | --- |
| 命令正常退出，包括非零码 | `env.Result, nil`，业务判断 ExitCode |
| 调用取消或输出超限 | 独立清理 context 中断，再查状态确认停止 |
| 无法确认命令/创建结果 | `errors.Is(err, sandbox.ErrUnconfirmed)`；用 UnconfirmedError 中 ID/token 排查，不自动重试 |
| 新建实例已知 ID 但初始化失败 | 返回部分 Environment，Registry 负责销毁 |
| 创建响应丢失、没有 ID | 随机 owner token 用于 ListSandboxes 对账；空列表不能证明未分配，Close 保持可重试 |
| IfVersion/CreateIfAbsent 冲突 | `env.ErrStale` / `env.ErrExists`，读取后决定后续动作 |
| 文件/遍历超限 | `opensandbox.ErrFileLimit` / `ErrWalkLimit` |

默认上限：操作 30 秒、启动 120 秒、清理 5 秒、命令输出 1MiB、RPC 响应 8MiB、单文件 4MiB、Walk 10000 个条目（包含起点）。宿主可在 Options 设置；coding 工具还有自身输出/文件限制，实际采用较严格边界。超出命令输出上限会中断命令并返回 error，不是无限运行后截断。

FS 请求通过临时文件上传，避免 Linux 单个 argv 长度限制。辅助程序使用目录 fd/no-follow 限制路径，内部链接可解析，外部链接被拒绝，Walk 不跟随链接，Remove 删除链接本身。每次操作重新检查路径；Walk 在并发变化下不是快照。

条件写的锁只协调辅助程序写者。直接命令、外部编辑器或有权限的恶意进程可绕过锁；不承诺对这些写者的 CAS。写入原子替换后 fsync 失败或调用取消时，文件可能已经改变，需重新读取确认。命令的工作目录在根内，但命令可读写沙盒内其他路径；工作区文件接口边界不等于容器内命令文件权限。进程停止确认不承诺所有脱离进程组的子进程消失，owned 实例最终整体销毁。
