# Sandbox API 与配置参考

本页描述供应商无关的已实现接口。主线执行策略接口见 [sandbox](../../sandbox/README.md)，后续提案见 [路线图](../design/SANDBOX_ROADMAP.md)。仓库不交付远端供应商适配器。

| 导入路径 | 职责 |
| --- | --- |
| `github.com/kart-io/goagent/harness/env` | Executor、FileSystem、Command、Result、WriteIntent |
| `github.com/kart-io/goagent-harness/sandbox` | 本机/容器 Policy 与 Executor；Provider、Registry、Session |
| `github.com/kart-io/goagent-harness/permissions` | 调用审批、会话权限与父级硬上限 |
| `github.com/kart-io/goagent-harness/preset` | Host.Sandboxes 与声明式装配 |

## Provider 与资源

以 [types.go](../../sandbox/types.go) 为准：

```go
type Provider interface {
    Version() string
    Create(context.Context, Request) (Environment, error)
    Connect(context.Context, string, Request) (Environment, error)
}
type Request struct { Provider, Scope, Image string; Network bool }
type Binding struct { ID string; Retain bool }
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

Provider.Version 是宿主实现的身份；实际后端支持与策略验证另行核验。`ContextResolver` 是远端文件系统生命周期要求，避免无 context 的 Resolve 越过取消。`MutationProtector` 可在实际文件变更边界累加受保护名称，其限制与生命周期必须保留。

## 结构化命令策略

```go
type PolicyExecutor interface {
    CheckPolicy(context.Context, Policy) error
    RunWithPolicy(context.Context, env.Command, Policy) (env.Result, error)
}
```

这是 managed 命令装配必须实现的额外契约，不是用 Capabilities 布尔值推测能力。后端必须实际强制传入的模式、路径、子进程与网络限制；不能支持时拒绝。Session.CheckPolicy/RunWithPolicy 保留生命周期追踪，运行前重新校验策略，不能缓存审批为永久权限。当前 managed 不支持额外宿主目录或环境变量映射。

## Registry

[Registry](../../sandbox/registry.go) 配置包含 Namespace、Providers、Resolve。使用期间配置不可变。Open 先校验任务命名空间、Provider 版本、取消与关闭状态，再 Create/Connect；网络能力或返回环境不完整则失败。Open 可能同时返回 Session 与 error，宿主必须接管清理。Close 关闭准入、取消待分配工作和会话并等待返回；不强杀不合作的宿主回调。

## Session

[Session](../../sandbox/session.go) 提供 Info、FileSystem、Executor、BeginClose、Close。Info 包含实例 ID、Root、Provider、Version、Isolated、Owned、Retained 与 Capabilities。新建未保留实例先 Destroy，再 Close；借用/保留实例只 Close。失败可重试，关闭前已准入操作实际结束后才结算关闭。

Session.Executor/FileSystem 的直接访问提供资源生命周期，不自动创建权限控制器；手工装配者须自行接入授权与策略。Preset 的 managed 包装层负责把 Controller 与逐次 Policy 连接起来。

## 错误和配置

ErrClosed 拒绝关闭后的操作；ErrUnsupported 表示不能兑现要求；ErrUnconfirmed 与 UnconfirmedError 保留操作未知状态；ErrOutputLimit 代表输出界限。不要将这些状态统归为普通超时后直接重试。

WorkspaceConfig 的 provider 仅用于 executor=sandbox；地址、密钥、namespace、既有实例绑定属于 Host。当前主线保留 maxSandbox、additionalWritableRoots、envAllowlist 与 SandboxCeiling，不支持的 managed 映射必须拒绝。详细装配和限制见 [指南](../guides/SANDBOX.md)，合并测试见 [验收](../evaluation/2026-11-01-sandbox-main/REPORT.md)。


## 持久调用的未知结果

编码命令与文件修改工具为同一 journal/CallID 设置 MaxAttempts=1，权限/schema 装饰器保留该策略。首次 ErrUnconfirmed 保持错误；持久恢复遇到失败记录返回 ErrOperationAttention，不重新执行副作用。正常完成的结果仍可从日志回放。确需再次发起操作时，宿主先核对实际结果，再明确安排新调用；换 CallID 不自动证明重试安全。
