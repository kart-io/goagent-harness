# Sandbox 场景与组合

| 场景 | 当前能力 | 约束与验证入口 |
| --- | --- | --- |
| 本机工具执行与单次审批 | native/docker/local + Controller | [可运行案例](../../examples/sandbox-permissions/main.go)，宿主不支持隔离时拒绝 |
| 自定义远端工作区 | Provider + Registry + Session 接口 | 需宿主自行实现后端与策略强制，无内置供应商实例 |
| 子 Agent 独立环境 | Preset 按任务/组件派生 Scope | 父级 Policy、网络、环境与审批上限不能丢失 |
| 固定业务步骤 | GoAgent workflow 调用已授权工具 | workflow 不代替授权或资源清理 |
| 重连既有环境 | 宿主 Resolve 返回 Binding | 保留原实例身份并核对 Manifest，不把新实例冒充恢复 |
| 不确定的命令/资源关闭 | ErrUnconfirmed + 可重试 Close | 宿主先核对实际状态，不能宣称无副作用 |

一个任务同时使用多个 Agent 时，模型预算与资源配额分别治理；Provider 不负责意图识别，不另建 Agent 循环。共享文件写入还需任务/产物级提交契约，Session 本身不提供跨进程业务事务。

现有端到端案例只覆盖本机策略。Provider 契约测试使用受控环境，不等同于真实远端服务验收。接入步骤见 [指南](SANDBOX.md)，API 见 [参考](../reference/SANDBOX_API.md)。
