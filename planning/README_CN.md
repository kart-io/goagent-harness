# 持久任务规划

[English](README.md) · [离线示例](../examples/planning-agent/README_CN.md)

`planning` 是基于GoAgent现有持久图的实验性规划闭环，由宿主掌握权限。固定图依次解析目标、验证候选DAG、冻结分配、执行就绪任务、复核结果并验收最终目标。模型生成的角色名称不能授予权限。

## 宿主接入

1. 建立受信 `Config.Members` 目录，提供稳定ID、语义版本、Manifest摘要、效果类别和资源声明。这些值由宿主提供，不能从Planner采纳。
2. 实现 `Planner`、`Executor`、`Reviewer`，按需实现 `GoalResolver`；在 `Dependencies` 中提供组件版本。`NewAgentGoalResolver`、`NewAgentPlanner`、`NewAgentReviewer` 可适配独立日志化的Builder Agent。Factory必须提供独立且可关闭的会话、稳定Agent身份/scope，并为规划及复核配置只读工具。这些属于宿主契约。
3. 调用 `NormalizeConfig`、`NewGraph`，通过相同持久存储取得 `DurableConfig`，固定状态版本、组件版本、成员目录和宿主策略。使用preset的 `PlanningExecutor` 时，父Assembly须另行Pin；子Manifest不包含整个父装配。
4. 绑定具有稳定RunID、journal和持久根预算的 `execution.Scope`。规划、执行、复核共享该根，重建宿主不能重置预算。
5. 首次 `Run` 前调用 `PrepareInitialState`。底层图在normalize前保存初始检查点，这一步防止超长未提交输入进入初始检查点；已有权威历史遇容量错误时保留。
6. 等待时返回并保存叶子interrupt的键和schema。重新打开相同存储，校验答案后恢复该键；不能回答父图键、另建attempt或清理未知效果记录。

已获授权的结构化目标放在 `State.Goal`；自然语言需求通过 `State.Request` 与Resolver处理。必需参数、澄清轮次及候选确认发生在执行前。宿主来源材料冻结于 `Goal.Inputs`，模型不能自建授权来源。

## 契约与上限

`Parallelism` 默认1，多槽须显式启用，运行中槽数固定。默认上限为当前任务64个、Wave 256次、重规划3次、每任务尝试3次、目标解析4轮、单产物64 KiB、单次TaskResult内产物正文合计1 MiB、整个序列化State（含历史与重复引用）8 MiB。Goal.Inputs来源正文合计另用同一MaxArtifactTotalBytes配置值限制。这些规划字段为零时采用默认，负数拒绝；产物还受具体Output声明的字节上限约束。它们不能替代共享的请求/token预算。

只有已验收上游产物满足依赖。重规划仅在语义指纹不变时复用产物；输入变化使下游传递失效。等待续跑保持业务尝试身份。写任务冻结契约与祖先，效果未知或已发生时不能换任务ID或Generation绕过。

Executor取得完整冻结 `GoalContext`，以及明确选中的 `GoalInputs` 和验证后的上游产物。执行结果仍是不可信候选：宿主检查身份、大小、输出声明和来源，分配产物身份/摘要，再验证任务和最终复核。JSON pointer证据须定位到本次分配授权的来源或产物。缺少摘要由宿主补齐，非空错误摘要拒绝。引用合法只表示可追溯，事实正确仍需复核与独立评估。

`Progress(State)` 按计划顺序投影展示状态，可同时出现多个running任务，不能作为调度、审批或恢复依据。

## preset成员与效果

`planning-members` 使用 `Host.PlanningMembers`、`Host.PlanningAgentFactory`，由 `Assembly.PlanningExecutor()` 暴露执行器。`PlanningBuilderFactory` 保留journal，默认不重发中断模型请求。成员继承父级pre-hook、guard、当前权限、沙箱/网络上限及环境约束。

本轮要求工作区成员使用宿主持久native目录，拒绝嵌套subagent，并在审批或执行前拒绝规划成员的 `PreSubstitute`。普通非planning subagent保持原有行为；未知工具直接报错。需要的工具应预先注册并授权，不能依赖运行时替换。

审批绑定目标摘要、计划修订、attempt与精确动作。新写入前再次检查撤权。未声明幂等性的写操作只有一次物理尝试，回执丢失后须按业务键核对或转宿主attention。模型重试与外部效果重试是不同契约；自定义Factory不能悄悄开启中断重发，或在无持久策略时重试写入。

## 记忆与恢复

自动召回是可撤回上下文，不是授权业务来源。不能复制进 `Goal.Inputs` 或任务指令来绕过删除。Forget从后续物理模型请求中移除符合条件的召回，不删除宿主已保存的业务输入、产物、审批或预算；这些存储须另行执行宿主的数据保留策略。

恢复使用相同图/状态版本、配置、成员Manifest与预算根。Manifest变化或检查点不兼容时拒绝恢复。升级前备份存储和删除日志；只替换二进制不能降级记忆 `/4`。旧plain聊天轮次继续保持原始请求意图。

示例默认不调用模型。其确定性Reviewer演示控制契约，不能代替生产质量判分。评估CLI预登记直接Agent、单槽和四槽试验，使用相同上限并把失败计入分母；只有显式配置live才访问模型。离线通过不能证明回答更好、成本更低或速度更快。
