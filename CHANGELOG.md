# 变更记录

## 未发布

- 新增 `verify-before-stop`：模型给出最终答复前在工作区运行检查命令，失败时把输出交还模型续跑，默认最多 3 次。本会话还没有经写入类工具改动文件时不检查。`coding.Workspace` 增加 `RunCheck`。
- `claude-code-hooks` 的 `Stop`：`stop_hook_active` 只在本轮已因 Stop 钩子续跑时为 true，用户 Steer 不再使它为 true。需要 GoAgent 的 `Stopping.Blocked`。
- 修复：调用方的超时或取消在探测期间到期时，Docker 后端和固定 `Executor.Runner` 的 native 后端返回 `ErrUnavailable`，现在返回上下文错误，与自动选择后端时一致。
- 依赖升级到 GoAgent v0.37.0。
- 新增 `effects`：持久的外部副作用队列，决定与待办原子提交，结果未知时不盲目重做。
- 新增 `background`：有稳定身份的子工作，重启后找回，完成后通知所有者，提供等待上限、交接和为人保留的槽位。
- `subagent` 组件增加 `mode: background`，由 `Host.Background` 持有子 Agent。默认仍是同步的前台模式。
- 后台子 Agent 需要审批时进入 `waiting`，宿主用 `Manager.Resume` 和 `ApprovalAllow`、`ApprovalDeny` 答复，不再调用 `Host.Approver`；提供 `<toolName>_status` 查询工具。
- 托管沙箱的责任跟随后台子工作：每个 child 有自己的实例，重启后连回，结束时才销毁；宿主借出或保留的实例不受影响。新增 `Registry.OpenWith`、`Registry.FindByScope`、`Registry.Destroy`、`Registry.Placeholder` 和可选的 `InstanceFinder`。
- 增加 `integration/postgres` 独立模块，CI 在真实 Postgres 上验证后台子工作；`integration` 增加基于 Docker 的测试 Provider，验证托管沙箱的生命周期。
- 后台子工作重启后恢复预算和会话身份，并固定装配清单，变化时记为失败（`manifest_changed`）。
- `BuilderRunner` 在后台链路中开启 `WithInterruptedModelResend`：停在模型请求中途的子工作重启后重发并继续，可能重复计费，每次计入预算。不可重跑的工具仍不重跑。
- `run_command` 在后端实现 `KeyedExecutor` 时，重启后附着到原命令并取回结果，不重跑；新增 `CommandSupervisor`，用沙箱内的 POSIX 工具实现这项能力。
- 新增 `sandboxtest`：Provider 契约测试和故障注入。Docker 和自托管 OpenSandbox 的测试 Provider 都已通过。
- `native` 后端在 Linux 上增加 Landlock：Bubblewrap 探测不通过时回退，新增 `Executor.Runner`、`Report.Runner`，恢复契约记录回退。内核低于 6.12 时上报 `partial`，低于 6.2 时不可用。
- 新增 `sandbox/sshsandbox` 和 `cmd/sandbox-runner`：把 Linux SSH 主机上的目录作为沙箱实例，命令由远端的本机隔离后端执行，支持断线后取回结果。
- 修复子工作被终止后资源释放可能延迟 5 秒：Runner 退出时不再写多余的结果。
- 修复托管沙箱泄漏和释放卡住：`Create` 响应丢失时保留分配意图并在释放时核对（新增 `ErrNotCreated`，Provider 确认没有实例时要返回它）；`Registry.Destroy` 可以删除已停止或已不存在的实例（新增 `InstanceDestroyer`、`ErrInstanceNotFound`）；实例在重启时已消失记为 `sandbox_gone`。
- 修复原生沙箱偶发 `ErrUnavailable`：`bwrap` 重挂宿主挂载点时与其他进程（如 Docker）的卸载竞争，现在重新启动；网络探测失败的错误信息带上退出码和输出。
- 修复技能说明与参考文件的符号链接越界读取，固定技能目录句柄并在关闭时释放。
- 修复文件分页整数溢出、整数参数范围校验和 10 MiB 单行搜索漏报；拒绝配置中的尾随 YAML 文档。
- 将边界审计用例纳入默认回归测试。
- 从 GoAgent 提取 Preset、编码工具、编码与计划模式示例及原有回归测试。
- 依赖 GoAgent v0.23.0 的公开接口，保留原有配置格式及行为。
- 增加公开 API 的持久图审批、工具执行与完成后恢复验证，以及临时 workspace 跨仓验证脚本。
- 源码发布到 `kart-io/goagent-harness` 的 `main` 分支，尚未发布版本标签；来源和导入迁移见 NOTICE 与 docs/MIGRATION.md。
