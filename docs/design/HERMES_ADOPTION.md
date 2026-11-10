# Hermes 采纳：Harness 增量设计

日期：2026-11-10。状态：设计提案，未实施。用户提到的 `goagent-hermess` 暂按本仓库 `goagent-harness` 对应；如是其他仓库需重新对照。

跨仓完整规格：[GoAgent / Harness 的 Hermes 采纳设计](../../../goagent/docs/superpowers/specs/2026-11-10-hermes-runtime-design.md)（相邻检出）；[仓库链接](https://github.com/kart-io/goagent/blob/master/docs/superpowers/specs/2026-11-10-hermes-runtime-design.md) 在文档推送后可用。本文件保存 Harness 的责任与迁移要求，不复制一套执行内核。

阅读来源：[微信原文](https://mp.weixin.qq.com/s/rbybfVn1Pxbup9pkfPwMWg) 全部 17 张图；[逐图对照](../../../goagent/docs/research/2026-11-10-hermes-design-review.md) 说明前 13 张架构、后 4 张资料展示及固定官方源码的核对范围。图解中的模块是职责，不要求一比一建立包。

## 1. 本仓库需要补齐的内容

| 阶段 | 当前实现 | 增量设计 |
| --- | --- | --- |
| H1 技能加载 | preset/skills.go 自行解析与读取；核心已提供通用 Catalog | 复用核心 skills.Catalog / tools/skill，Harness 保留目录、信任、可见性和执行策略 |
| H2 上下文 | persona/instructions/env-context/PromptSections/Contexts 分别贡献文本 | 来源报告统一记录来源、摘要、权限、预算、顺序与冻结范围，复用现有 Assembly/Manifest |
| H3 模型路由 | Host 选客户端，Loop.Request 可以替换 | 先做版本固定且确定性的路由；不宣称持久 RequestError 可跨 Provider 回退 |
| H4 会话搜索 | session 保存完整事件；compaction 只减小工作窗口 | 可选有界检索工具，消费宿主/contrib 的授权索引；不全库读入内存 |
| H5 技能沉淀 | 只读技能加载，无候选发布生命周期 | 从已验证轨迹提出候选，隔离验证和授权发布，新 Revision 仅供新 Session |
| H6/H7 网关与调度 | background 子工作、effects 投递已经存在 | 平台绑定和日历调度由宿主组合，复用 management Task；Harness 不新增任务库 |

这些阶段接续现有 [Pi 采纳](PI_AGENT_ADOPTION_PLAN.md)、[dsh 子工作与效果](DSH_SUPERAGENT_ADOPTION_PLAN.md)、[Sandbox 路线图](SANDBOX_ROADMAP.md)，不将其已实现部分再列作待开发。

## 2. H1 的关键迁移约束

1. 先更新并验证核心依赖：本地 go.mod 当前固定于 GoAgent `v0.49.3-0.20261107151110-dae60f4779d4`，不能因为相邻目录有新 API 就假定发布依赖已具备它。开发可用临时 workspace 验证，但发布需可解析的核心版本。
2. 来源授权先于 Discover；默认目录和覆盖优先级仍由 Harness 决定，核心不解释 `~`。
3. 核心 Catalog 不自动隐藏/授权；Harness 对模型索引、按名加载和实际工具许可分别处理。
4. 可借用宿主归档 Catalog，或者创建本地 Catalog；借用者不 Close 共享资源，创建者由 Assembly.Close 回收。构建失败也须释放自己持有的目录。
5. Manifest 的旧正文短摘要不能冒充新原文摘要/整包摘要。组件版本明确变化；旧运行用旧装配恢复或提前结束，不能静默迁移。
6. SKILL.md 的 Digest 不含引用文件。持久、可审计部署从 AgentRevision 的整包归档加载；本地可变目录需要明确标识资源可变。
7. 技能不因 `allowed-tools` 获得权限，隐藏技能不因未列出名称就自动受到读取保护。

## 3. H2 的来源报告和环境快照

报告随 Assembly 暴露为诊断资料，记录 SourceID、种类、来源位置、信任、适用范围、原文/渲染摘要、版本、大小、预算及入选/拒绝原因。默认不包含正文和秘密。

稳定内容进入系统前缀；git、待办、审批状态与召回事实走 PromptContext 或已定义的数据通道。当前 env-context 是装配时探测，重建 Assembly 会重新探测；持久恢复需由宿主保存并复供原运行快照，再另报实时变化，不能直接把新探测结果用于重建旧 Manifest。

PromptSection 的现有保证是单轮固定，不是整个 Session 自动冻结。跨轮依赖 AgentRevision 与固定装配；目录级指令的作用范围必须保留。内容扫描只能提供证据，不能授予工作区外路径、网络或工具权限。

进入 Manifest 的只有稳定来源身份、RevisionID/归档相对路径、静态内容与渲染摘要、固定版本/排序/预算配置。临时目录、宿主绝对路径、采集时间、动态内容和每步预算裁决只进诊断，不参与恢复摘要；环境快照沿用宿主持久保存的原运行资源摘要。

## 4. 路由、错误与检索的安全恢复

H3 首版固定路由配置/端点/策略版本和凭据范围引用，RequestHook 只应用确定性结果；实时健康、价格和随机选择不能在重放时改变模型目标。持久失败仍只允许原样重试。跨 Provider 的状态、部分输出和预算需要独立规格，不能在 Client 包装器里隐藏切换。

普通工具错误不在 durable Builder 中自动转成功反馈。能确认的业务失败由工具返回 Rejected；取消、存储错误、租约丢失和未知效果仍终止或 attention。

H4 的索引不是权威来源，搜索要在授权范围内分页，并有正文与扫描上限。对可以撤权或删除的会话，不直接把正文放进永久可重放结果：首版限制为非持久工具，或使用宿主承诺的不可变授权档案；一般持久检索需 refs + 每次恢复重鉴权的后续契约。

## 5. 技能候选与宿主集成

H5 的候选不进入活跃目录；来源轨迹、验证产物和完整包摘要可追溯。发布依赖不可用时保持待审，附属文件变更使旧审查失效。新的 Catalog 归入新 AgentRevision；旧 Session 保持原版本，紧急撤权仍由执行准入控制。

H6 的入口认证、跨平台身份、私聊/群聊隔离、账号凭据与目标绑定由宿主决定。H7 的 Schedule/Occurrence 同样在宿主，默认每次新 Session。background 不充当 cron，effects 不充当业务任务库。

首版复用 management 已有终态事务 outbox / OutboxDispatcher。宿主 DeliverySink.Accept 在 effects 使用的同一 durable.Store 内，以 TenantID + Delivery.ID 为键，原子提交不可变接纳收据与全部分片效果；重复同内容交接只确认收据，异内容冲突。Accept 成功表示持久接纳，实际送达由 effects 单独记录。effects 的稳定键不是永久去重，终态被清理后也必须由收据/墓碑阻止再次入队。ACK 未知进入核对，补发不能重跑 Agent。

日历调度还须协调 management 幂等保留期：每次跨库外呼前固定首次尝试时间；未知创建/提交最多自动恢复 24 小时，要求配置的幂等保留期更长并留时钟/运维余量。超期缺 SessionID/TaskID 时转 attention，不重新创建。已知 ID 仅查询；触发墓碑覆盖计划重驱期限。计划固定旧版本在新版本发布后仍受 CreateSession 的 agent.publish 权限检查，缺权限则 blocked，不能偷偷切最新版或给调度主体扩权。

## 6. 实施与验证顺序

先提交 H1 的核心版本矩阵与装配迁移，再做 H2 来源报告；H3/H4 独立安排，H5 依赖 H1/H2，H6/H7 以具体宿主案例驱动。首版不同时建设完整个人助手平台。

验收须覆盖：

- 新旧 Manifest、归档技能引用、目录替换、大小限制、隐藏/读取策略以及共享资源关闭。
- 多轮前缀稳定、动态状态变化、预算不足、来源解释、Assembly 重建的环境快照。
- 路由漂移、能力缺失、重试用量和持久回退被明确拒绝。
- 搜索授权分页、撤权、索引落后；候选隔离、验证失败与新旧会话版本。
- 任务完成到投递之间崩溃、未知 ACK、重复回调和定时触发去重。

本轮仅新增设计文档，未修改已有未提交的 preset/build.go、preset/builtin.go、preset/plan.go 或 integration/runtime_test.go，也未运行代码验收。
