# Sandbox 使用指南

本仓库提供 native/docker/local 执行策略与供应商无关的 `Provider → Registry → Session` 生命周期。没有内置远端供应商 SDK、部署脚本或远端服务案例。现有本机策略与单次审批演示见 [sandbox](../../sandbox/README.md)、[permissions](../../permissions/README.md) 和 [可运行案例](../../examples/sandbox-permissions/main.go)。

## 本机与容器

`executor: docker` 保持默认，网络默认关闭；`executor: native` 使用目标平台可用的本机隔离后端。不能兑现策略时拒绝执行，不自动降级为不受限 local。完整配置与平台限制见 [Preset](../../preset/README.md)。

```sh
GOWORK=off go test -race ./...
GOWORK=off go build ./...
go run ./examples/sandbox-permissions -approve
```

案例可能因宿主不具备所需隔离能力而拒绝；应记录平台与拒绝原因，不能以拒绝当作成功执行。文件隔离、文件保密、容器和虚拟机是不同保证。

## 宿主自定义 Provider

宿主实现 [Provider/Environment](../../sandbox/types.go)，将实现注册到可信 `Registry.Providers`，用 `Registry.Namespace` 标识任务。`Provider.Version()` 用于装配身份，不是运行时安全证明。地址、凭据、既有实例 ID 与 Resolver 由宿主控制，不进入模型参数。

```go
registry := &sandbox.Registry{
    Namespace: taskID,
    Providers: map[string]sandbox.Provider{"managed": trustedProvider},
}
// trustedProvider is supplied by the host; this repository ships no vendor implementation.
session, err := registry.Open(ctx, sandbox.Request{
    Provider: "managed", Scope: "analysis", Image: image, Network: false,
})
if session != nil {
    // Close with an independent bounded cleanup context, including on err.
    defer session.Close(cleanupCtx)
}
if err != nil { return err }
```

上例仅演示接口装配，不是可独立运行的后端。`Registry.Close(cleanupCtx)` 由宿主在任务退出时调用，并处理失败、取消、清理重试。不能用已取消任务 context 作为唯一清理机会。借用实例和 Retain 实例不会被默认销毁。

## Preset managed 装配

`workspace.config.executor: sandbox` 配合 `provider`、`image` 和 `network`，由 `Host.Sandboxes` 选择宿主注册项。主线 `SandboxCeiling`、父级权限和审批控制器仍有效。实际根目录来自后端，Host.Root 仍是本地配置/授权标识，不隐式映射宿主文件。

命令支持矩阵：

| 配置 | 文件工具 | 命令与 linter |
| --- | --- | --- |
| sandbox=read-only 且 maxSandbox=read-only | 只读视图 | 不开放；配置 linter 直接拒绝 |
| maxSandbox 高于 read-only | 保留逐次权限和审批校验 | 实际 Executor 必须实现 sandbox.PolicyExecutor |
| additionalWritableRoots 或 envAllowlist 非空 | 分配前拒绝 | 当前没有宿主到远端的映射契约 |

PolicyExecutor 的 CheckPolicy 在装配时检查默认策略及最高可审批策略，RunWithPolicy 在每次调用接收 Controller 核验后的有效策略。Host.Root 是规范化的本地授权域，后端负责将该域绑定到本环境的实际 Root，不能按同名宿主路径访问其他资源。借用/保留生命周期不授予额外权限。

具体后端须兑现当前调用的结构化 Policy；只有容器或隔离布尔声明的执行器不能取得受限命令权限。不支持的网络、附加目录、环境变量映射应明确拒绝。只读文件视图不代表只读命令隔离，更不能因审批放宽绕过宿主硬上限。合并后的精确准入与回归结果见 [验收记录](../evaluation/2026-11-01-sandbox-main/REPORT.md)。

## 恢复与用量

恢复需要重连原实例并核对真实实例 ID、Provider 版本及 Manifest；同配置新实例不等于同一执行环境。Registry 是进程内资源管理，不是持久租约系统。模型 token 由 Agent/Builder 统计，远端资源账单由宿主独立记录。不要把未知操作或清理超时填成零消耗。


## 持久调用的未知结果

编码命令与文件修改工具为同一 journal/CallID 设置 MaxAttempts=1，权限/schema 装饰器保留该策略。首次 ErrUnconfirmed 保持错误；持久恢复遇到失败记录返回 ErrOperationAttention，不重新执行副作用。正常完成的结果仍可从日志回放。确需再次发起操作时，宿主先核对实际结果，再明确安排新调用；换 CallID 不自动证明重试安全。
