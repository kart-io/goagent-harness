# 从 GoAgent 迁入独立 Harness

## 包路径

| 原路径 | 新路径 |
|---|---|
| `github.com/kart-io/goagent/harness/preset` | `github.com/kart-io/goagent-harness/preset` |
| `github.com/kart-io/goagent/tools/coding` | `github.com/kart-io/goagent-harness/tools/coding` |

`preset.Configure`、`Load`、`Build`、`Assembly` 和编码工具 API 保持原有行为，迁移时改写 import。
配置文件仍为 `~/.goagent/agents.yml`、`.goagent/agents.yml` 和 `.goagent/agents.local.yml`，本次不更改配置语法与存储位置。

## 保留在 GoAgent

Builder、Runnable、Graph、durable、budget、observe、Dispatcher/Pipeline、Handle、循环钩子、`harness/env`、`tools/policy`、`tools/spill` 继续由 GoAgent 提供。已有业务编排用户不需要引入本项目。

## 依赖与验证

当前模块依赖已发布的 GoAgent v0.32.2，包含 Preset 所需循环扩展点与当前执行环境接口；最初拆分基线为 v0.23.0。后续独立版本发布须更新明确的 GoAgent 版本并分别运行 `GOWORK=off go test ./...` 和本地跨仓验证脚本。
源码已迁至 `github.com/kart-io/goagent-harness` 的 `main` 分支，尚未发布版本标签。不在 go.mod 中写入本地 replace；需要稳定版本的使用方应等待正式标签。

发布依赖与本地 checkout 均需分别验证，文件系统保证以各自具体代码为准。Sandbox 供应商适配器为独立可选模块，其临时 workspace 和单独验收命令见 [Sandbox 指南](guides/SANDBOX.md)。

## 示例与设计

`examples/tools/09-coding-agent` 迁至 `examples/coding-agent`；`examples/tools/10-plan-agent` 迁至 `examples/plan-agent`。
历史设计见 [Harness 核心化设计](design/HARNESS_CORE_DESIGN.md)，迁移来源见 [NOTICE](../NOTICE)。

## DSH 风格沙盒与权限迁移

原有 YAML 的默认值保持：`executor: docker`、`sandbox: workspace-write`、`network: false`。
`executor: local` 仍表示不隔离；只读 local 仍隐藏命令工具，不会自动切到 native。
原有 GoAgent env 接口与依赖版本不变。

新增选项：`executor: native`、`maxSandbox`、`additionalWritableRoots`、`envAllowlist`。
额外目录必须填写物理绝对目录；文件工具使用 `@root1/path`、`@root2/path` 访问，按配置顺序对应，任意宿主绝对路径不会因此开放。
这些选项来自项目配置时会进入扩权授权检查；`maxSandbox` 不得超过宿主 `SandboxCeiling` 或父 Agent 上限。
命令子进程默认不继承模型 API token 等环境变量；需要的变量必须在可信配置的 `envAllowlist` 中明确列出。
Docker 默认保留镜像自己的 PATH；native 的可写缓存应放在工作区内，临时目录由执行器提供。

可选 `permissions.role` 提供角色模板。reader/reviewer/coordinator 禁止编辑与执行命令；coder 的命令仍需审批；publisher 不自动获准推送或发送消息。
自定义远程工具仍须由宿主按具体资源配置授权，不因角色名或文件沙盒模式而获得权限。

宿主如需恢复会话模式，实现 `permissions.SessionStore` 并提供稳定的 `Host.AgentID`。
同一 Agent 的状态变更应由单个 Controller 管理；多实例共享存储时宿主须提供跨实例协调。
未配置 SessionStore 时仅保留当前 Controller 的进程内状态，不能声称重启后继续保留。
审批从不持久化；恢复未完成调用必须重新检查权限并重新批准。静态 manifest 不包含会话递增版本。

历史 manifest 与新版本的工具 schema / 装配契约不同，恢复会拒绝不匹配；宿主须以新的执行范围启动，不要覆盖旧日志。

Linux native 与受限 Docker 为阻止通过宿主 Unix socket 绕过权限，会拒绝 Unix socket/socketpair 与 io_uring_setup。依赖匿名 Unix IPC 的程序可能需要调整执行策略；不能用关闭这些限制的方式继续声称相同隔离保证。macOS 按 Unix 路径阻断外部连接，保留匿名 socketpair。
Docker 的系统调用基线固定为仓库内注明来源与许可证的 Moby profile，并只收紧权限，不会改成 allow-all。自定义 daemon profile 的宿主应核对这个固定契约。
