# Sandbox 使用指南

本仓库提供 native/docker/local 执行策略、供应商无关的 `Provider → Registry → Session` 生命周期，以及 SSH Provider（[`sandbox/sshsandbox`](../../sandbox/sshsandbox)）。没有内置云厂商 SDK、部署脚本或远端服务案例。现有本机策略与单次审批演示见 [sandbox](../../sandbox/README.md)、[permissions](../../permissions/README.md) 和 [可运行案例](../../examples/sandbox-permissions/main.go)。

## 本机与容器

`executor: docker` 保持默认，网络默认关闭；`executor: native` 使用目标平台可用的本机隔离后端。不能兑现策略时拒绝执行，不自动降级为不受限 local。完整配置与平台限制见 [Preset](../../preset/README.md)。

```sh
GOWORK=off go test -race ./...
GOWORK=off go build ./...
go run ./examples/sandbox-permissions -approve
```

案例可能因宿主不具备所需隔离能力而拒绝；应记录平台与拒绝原因，不能以拒绝当作成功执行。文件隔离、文件保密、容器和虚拟机是不同保证。

Linux 上 `executor: native` 优先用 Bubblewrap，探测不通过时回退到 Landlock，恢复契约会记录回退。Landlock 比 Bubblewrap 弱：没有 PID 命名空间，脱离进程组的后代清理不到，也能看到宿主进程；内核低于 6.12 时还挡不住向外发信号，这时上报 `partial`。低于 6.2 时不可用。Windows 仍不可用。

## SSH 远端

`sshsandbox.Provider` 把一台 Linux 主机上的目录作为实例，命令由远端的 `sandbox-runner` 用远端的 Bubblewrap 或 Landlock 执行。

先在远端安装辅助程序，并记下摘要：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o sandbox-runner ./cmd/sandbox-runner
sha256sum sandbox-runner
```

```go
provider, err := sshsandbox.New(sshsandbox.Config{
    Addr: "host:22", User: "agent", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
    HostKeyCallback: hostKeys, // 必填，用 knownhosts.New 或 ssh.FixedHostKey
    BaseDir:         "/home/agent/sandboxes",
    RunnerPath:      "/usr/local/bin/sandbox-runner",
    RunnerSHA256:    digest,
})
registry := &sandbox.Registry{Namespace: taskID, Providers: map[string]sandbox.Provider{"ssh": provider}}
```

- **远端依赖：** GNU coreutils 和 findutils（`stat --printf`）、`setsid`、`flock`、`curl`（`network: deny` 的探测要用），以及 Bubblewrap 或内核 6.2 以上的 Landlock。
- **能力：** 实现了 `InstanceFinder`、`InstanceDestroyer` 和 `KeyedExecutor`。实例 ID 由 Scope 决定，所以 `Create` 响应丢失后也能查回。按键命令脱离 SSH 连接继续运行，断线和宿主重启后可以取回结果；普通命令在断线或取消时被终止。
- **不支持：** `danger-full-access` 和额外可写目录，这两种策略直接拒绝。
- **不防什么：** 隔离只限制写入、网络和进程，不限制读取，**同一台主机上的实例能读到彼此的文件**，也能读这个账户可读的任何文件。不要把不同租户放在同一账户下。远端主机和 `sandbox-runner` 属于可信基础设施，摘要校验防的是版本不符，不防恶意主机。
- **隔离程度：** 用 `Provider.Probe` 查看实际的 `Runner` 和 `Enforcement`。要求绝对边界的宿主应当拒绝 `partial`。
- **验证范围：** 契约和断线用例在真实 OpenSSH 上通过，覆盖 Ubuntu 22.04（5.15，bwrap）、24.04（6.8，Landlock `partial` 和 bwrap）和容器。设置 `GOAGENT_SSH_ADDR`、`GOAGENT_SSH_KEY`、`GOAGENT_SSH_HOST_KEY`、`GOAGENT_SSH_EXPECT_RUNNER` 后，`TestSSH*` 会连到你指定的主机（用户须叫 `agent`）。
- **Landlock 下的已知缺口：** `sandbox-runner` 自己被 SIGKILL 时（比如 OOM）漏掉命令的子进程。Bubblewrap 没有这个问题。
- **文件操作不进沙箱：** 文件工具走远端命令，路径边界由 `sshsandbox` 检查，运行中的命令可能在检查和操作的间隙替换路径。

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

上例仅演示接口装配，不是可独立运行的后端。可运行的参考见 [integration/docker_provider_test.go](../../integration/docker_provider_test.go)，它只用于测试，设置 `GOAGENT_DOCKER_TESTS=1` 后运行。`Registry.Close(cleanupCtx)` 由宿主在任务退出时调用，并处理失败、取消、清理重试。不能用已取消任务 context 作为唯一清理机会。借用实例和 Retain 实例不会被默认销毁。后台子 Agent 的实例归子工作，生命周期见 [background](../../background/README.md)。

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

编码命令与文件修改工具为同一 journal/CallID 设置 MaxAttempts=1，权限/schema 装饰器保留该策略。首次 ErrUnconfirmed 保持错误；持久恢复遇到失败记录返回 ErrOperationAttention，不重新执行副作用。例外是后端实现 `KeyedExecutor` 时的 `run_command`，见下。正常完成的结果仍可从日志回放。确需再次发起操作时，宿主先核对实际结果，再明确安排新调用；换 CallID 不自动证明重试安全。

`run_command` 在后端实现 `KeyedExecutor` 且运行带日志时按执行键运行：命令脱离连接继续跑，只有第一次尝试会启动，宿主重启后只附着并取回结果。查不到结果也不在运行时（`ErrCommandLost`，如容器重启或状态被删）不重跑，告诉模型状态未知，由它检查后决定。`git_commit` 和文件修改工具不受影响。需要审批的调用重启后要重新审批，被拒绝时命令被终止，模型会知道它已经启动过。

没有原生能力的后端可以用 `CommandSupervisor`：它在沙箱内用 `sh`、`setsid`、`flock`、`mkfifo` 和 `head` 实现按键执行，超时也在沙箱内执行。要求执行器启动的进程能在调用返回后存活，状态放在 `/tmp/.goagent-commands`，随实例销毁，所以长期保留的实例要由所有者清理。
