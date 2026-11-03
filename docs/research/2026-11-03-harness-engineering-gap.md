# Harness Engineering 对照与差距

对照对象：LangChain《Improving Deep Agents with harness engineering》（2026-02）及截至 2026-11 的业界方案。代码以 GoAgent v0.38.1 与当时的主线为准，「进展」一节记录之后的修复。标「已核实」的条目经人工读码确认，其余为读码结论未运行验证；外部资料的日期与数字以原文为准，未核实处单独注明。

## 结论

分层、沙箱与权限、技能渐进加载、会话外置持久化符合主流做法。主要差距是容错：工具名或参数错误、工具执行失败、瞬时模型错误、子 Agent 失败、审批超时都会中止整次运行，而主流做法是把它们作为工具结果交还模型。其次缺少验证闭环、循环检测、环境自举和 Harness 级回归评测。

P0 与 P1 已在 GoAgent v0.39.0 与本仓库主线完成，见「进展」；压缩、Request 通道与回归评测仍待处理。

## 进展（2026-11-03）

| 事项 | 结果 | 位置 |
| --- | --- | --- |
| 工具错误交还模型 | GoAgent `WithToolErrorFeedback`：未提供的工具、参数不是 JSON 对象、工具返回 error 改为业务拒绝；`Configure` 默认开启，并追加 `RetryTransient(3)` | GoAgent v0.39.0；`preset/loop.go` |
| 子 Agent 失败、审批超时 | 前台子 Agent 失败作为拒绝交还父 Agent；单次提权审批超时按拒绝处理 | `preset/subagent.go`、`permissions/controller.go` |
| 命令策略 fail-closed | Deny/Ask 规则按首词文件名比较，命令含元字符时至少裁决为 Ask；Allow 仍精确匹配 | GoAgent v0.39.0 `tools/policy` |
| 环境快照 | `env-context` 组件：OS、git 状态、工具链版本、根目录列表进系统提示词 | `preset/envcontext.go` |
| 循环检测 | `loop-detect` 组件：相同调用且结果相同（默认 3 次）、同一文件编辑（默认 8 次）时提醒，可选结束本轮 | `preset/loopdetect.go` |
| Stop 语义 | `Stopping.Blocked` 不含 Steer；续跑后须剩一步带工具的请求，否则不执行钩子并记 `turn_stopping_skipped`；`stop_hook_active` 改用 `Blocked` | GoAgent v0.39.0；`preset/hooks.go` |
| 结束前验证 | `verify-before-stop` 组件：有写入改动时运行检查命令，失败交还模型续跑，默认最多 3 次 | `preset/verifystop.go` |

尚未解决：

- 工具返回 error 的交还只在非持久运行生效；持久运行中失败结果不入日志，仍中止。
- `loop-detect` 没有做连续拒绝熔断。
- `Stopping` 仍拿不到消息与剩余额度，`transcript_path` 仍为空；`turn_stopping_skipped` 只在结果元数据中，Harness 没有展示。
- `verify-before-stop` 只看写入类工具的改动，`run_command` 改的文件不触发检查。

真实模型实测（DeepSeek `deepseek-chat`，`examples/coding-agent`）：命令子进程只继承 `PATH`、`LANG`、`LC_ALL`、`TZ` 与 `envAllowlist`，本地执行器没有 `HOME` 时 `go test` 报 `build cache is required`。模型能在每条命令前自设 `GOCACHE` 绕过，但 `verify-before-stop` 的固定命令一直失败，3 次续跑全部浪费，模型请求从 6 次增至 22 次。示例已放行 `HOME`，文档注明检查命令受 `envAllowlist` 约束。修复后检查一次通过，三个新组件装配正常；`loop-detect` 与工具错误交还在实测中未被触发。

## 符合度

| 业界共识 | 现状 | 判断 |
| --- | --- | --- |
| 引擎与 Harness 分层；会话状态在上下文窗口外持久化 | `core/durable` 日志、`core/session`；Harness 不新增执行循环 | 符合 |
| OS 沙箱、默认禁网、硬上限、凭据不进沙箱 | docker/native/local，`maxSandbox`，父子权限交集，不可用时拒绝 | 符合 |
| 静态前缀稳定以命中缓存，动态内容后置 | 静态 system + 每步 `PromptContext` | 符合 |
| 技能渐进披露 | `load_skill` 按需读取 | 符合；`file` 参数无大小上限（`preset/skills.go:246`） |
| AGENTS.md 作为目录、分层合并 | 只读根目录，超 32KB 截尾（`preset/builtin.go:79`） | 部分符合 |
| 首次调用前注入环境快照 | 只有项目指令与沙箱策略 | 缺失（已补 `env-context`） |
| 生成与验证分离，判据机械化 | 仅有 `TurnStopping` 机制，无内置验证组件 | 缺失（已补 `verify-before-stop`） |
| 循环检测与连续拒绝熔断 | 只有 `MaxIterations` | 缺失（已补 `loop-detect`，未做拒绝熔断） |
| 工具错误交还模型 | 未知工具、非法参数直接中止（`goagent/builder/tool_events.go:261,268`，已核实） | 不符合（已修复，持久运行中工具 error 除外） |
| 压缩保留关键信息 | 任务原文、计划、todo 不原样保留 | 风险 |
| 推理强度按阶段分配 | 引擎 `RequestHook` 可改 `Reasoning`，`preset.Loop` 无 Request 通道 | 能力未接出 |
| Harness 回归评测 | agentbench 为单工具问答 | 缺失 |

## 已确认缺陷

### 容错

- 未知工具名、非法参数 JSON 直接返回错误（`goagent/builder/tool_events.go:261,268`，已核实）。
- 除 Rejected 与参数错误外的 ExecutionError 中止运行（`goagent/builder/configurable_agent.go:559-568`、`goagent/tools/dispatcher.go:593-617`）。
- 前台子 Agent 失败中止父 Agent（`preset/subagent.go:263-266`）。
- 沙箱升权审批超时返回 error 而非拒绝（`permissions/controller.go:248-249`）。
- 引擎提供 `RetryTransient`，Harness 未装配，429/5xx 直接失败。

### Stop 钩子

- `stop_hook_active` 取自 `continued_turns`，而 Steer 唤醒也会累加（`goagent/builder/tool_events.go:231-236`，已核实），遵守该标志的验证钩子在用户 Steer 后被跳过。
- 续跑只受 `MaxIterations` 约束；最后一步不执行 Stop，未验证答复按成功返回且无标记。
- 倒数第二步续跑后下一步无工具，模型无法据反馈修改。
- 钩子拿不到对话记录，`transcript_path` 为空。

### 权限

- 带 Command 的规则在命令含 shell 元字符时一律不匹配（`goagent/tools/policy/policy.go:170`，已核实）。存在整工具 allow 时，`git push && true` 可绕过 `deny git push`；`/usr/bin/git push` 也不匹配前缀。应改为无法判断时 fail-closed。
- 命令钩子继承完整 `os.Environ()`（`preset/hooks.go:316`），与命令工具的环境白名单不一致。

### 上下文与压缩

- 按字节/3 估算 token（`preset/compaction.go:172`），未用响应中的真实 usage。
- 压缩调用不经预算控制器，也不计入 usage（`preset/compaction.go:128`）。
- 每条消息先截断到 4000 字符再摘要（`preset/compaction.go:148`），旧摘要也被截断，多次压缩逐级失真。
- 计划模式依赖对话中的 enter/exit 结果恢复（`preset/plan.go:111`），压缩后在持久运行中重启可能把已批准的 Agent 重新锁回计划模式（推测，高置信）。
- todo 只在 Workspace 内存中，不回注提示词。
- 压缩失败直接中止（`preset/compaction.go:79-81`），没有丢弃旧工具结果等降级路径。
- Harness 未接入引擎 `WithOffload`；引擎 Offload 在最后一步还原全部引用（`goagent/builder/contextpolicy/offload.go:172-183`），可能撑爆上下文。

### 工具与循环

- 编辑只做精确匹配，未命中只返回 not found，不处理 CRLF（`tools/coding/file_tools.go:219-230`）。
- 读文件结果没有总字节上限（`tools/coding/file_tools.go:96-101`）。
- 最后一步撤掉工具但不告知模型；`PromptScope` 无 Final 字段（`goagent/builder/loop_hooks.go:144`）。耗尽时只返回错误，没有部分答复。
- 预算耗尽直接失败，没有收尾轮。

## 扩展点是否足够

| 组件 | 能否不改引擎 | 缺口 |
| --- | --- | --- |
| 退出前验证 | 基本可以，用 `TurnStopping` | `Stopping` 缺消息、剩余额度、不含 Steer 的续跑计数；需预留带工具的一步 |
| 循环检测 | 可以，用 PreStep 或工具后置钩子 | 无 |
| 时间预算提示 | 部分 | `PromptScope` 缺 ctx、Deadline、Remaining、Final |
| 分阶段推理强度 | 引擎可以 | `preset.Loop` 与 `Configure` 缺 RequestHook 通道 |

## 2026 年值得跟进的方案

| 方案 | 要点 | 出处 |
| --- | --- | --- |
| Harness 做减法 | Claude 5 代系统提示删减 80% 以上；规则交给模型，工具用法写进工具定义，验证移入 skill | [Anthropic，2026-07-24](https://claude.dev/blog/the-new-rules-of-context-engineering-for-claude-5-generation-models/) |
| deepagents v0.7 | 基础输入约 6k 到 2k；todo 工具改为可选；85% 时触发摘要；各模型 reward 置信区间均跨 0 | [LangChain，2026-07-29](https://www.langchain.com/blog/deep-agents-v0-7) |
| 按模型定制 harness | Codex 系用 `apply_patch`，Opus 只调提示词；tau2 难题子集 GPT 5.3 Codex 33% 到 53% | [LangChain，2026-04-29](https://www.langchain.com/blog/tuning-deep-agents-different-models) |
| 环境自举 | 首次调用前注入沙箱快照，约 80 行；TB2 上 Opus 4.6 76.4%。搜索与评测同用 89 题，有过拟合风险 | [Meta-Harness，2026-03-30](https://arxiv.org/abs/2603.28052) |
| 失败轨迹驱动的 harness 修补 | 诊断失败、生成补丁、用验证集选择；TB2.0 +10.0 | [AutoSaddler，2026-08-24](https://arxiv.org/abs/2608.23041) |
| 生成与评估分离 | planner/generator/evaluator；模型变强后 evaluator 只在最后评一次 | [Anthropic，2026-03-24](https://www.anthropic.com/engineering/harness-design-long-running-apps) |
| critic 判定完成 | `FinishAction` 后打分，低于 0.6 续跑，最多 3 轮 | [OpenHands critic](https://docs.openhands.dev/sdk/guides/critic)、[论文 2026-03-04](https://arxiv.org/abs/2603.03800) |
| 循环检测阈值 | 同一动作同一结果 4 次、同一错误 3 次、独白 3 次、交替 6 周期 | [OpenHands stuck detector](https://docs.openhands.dev/sdk/guides/agent-stuck-detector) |
| 压缩诱发循环 | 压缩后循环增多；用 DPO 训练抑制 | [DeLoop，2026-10-14](https://arxiv.org/abs/2610.19376) |
| 模型审查员代替人工审批 | 连续 3 次或同一轮最近 50 次中 10 次被拒即中断 | [Codex auto-review](https://learn.chatgpt.com/docs/sandboxing/auto-review) |
| 分类器审批 | 两段式分类器；连续 3 次或累计 20 次拒绝转人工；真实越权漏放 17% | [Claude Code auto mode，2026-03-25](https://www.anthropic.com/engineering/claude-code-auto-mode) |
| 大脑与双手解耦 | 会话日志、无状态 harness、可替换沙箱；沙箱故障作为工具失败返回 | [Managed Agents，2026-04-08](https://www.anthropic.com/engineering/managed-agents) |
| 提示构造与压缩 | 静态内容前置；运行中变化追加新消息；AGENTS.md 逐级合并，默认 32KiB | [Unrolling the Codex agent loop](https://openai.com/index/unrolling-the-codex-agent-loop/)（日期未核实） |
| lint 报错附修复方法 | 机械化约束分层；后台 agent 扫描偏离并开 PR | [OpenAI harness engineering](https://openai.com/index/harness-engineering/)（日期未核实） |
| 按步推理路由 | 小 router 每步选强度，推理 token 最多少 52.7% | [ARES，2026-03-09](https://arxiv.org/abs/2603.07915) |
| 降推理强度的代价 | 默认强度从 high 降到 medium 后质量投诉，回滚 | [Anthropic 复盘，2026-04-23](https://www.anthropic.com/engineering/april-23-postmortem) |
| 评测基础设施噪声 | 仅资源配置不同 TB2.0 差 6 分；3 分以内差距不可信 | [Anthropic，2026-02-05](https://www.anthropic.com/engineering/infrastructure-noise) |

仍有分歧：单 Agent 与多 Agent、是否保留显式规划工具、权限模型强度、压缩方式、推理预算分配、自动优化 harness 的可信度、MCP 与 CLI/skill 的取舍、榜单可复现性。

## 改进优先级

| 优先级 | 事项 | 验证方式 |
| --- | --- | --- |
| P0 ✅ | 未知工具、非法参数、ExecutionError、子 Agent 失败、审批超时改为工具结果；默认装配 `RetryTransient` | 每类错误一个用例，断言运行继续且模型收到错误文本 |
| P0 ✅ | 命令策略含元字符或非裸命令时 fail-closed | `git push && true`、`/usr/bin/git push` 在 deny 规则下不放行 |
| P1 ✅ | `env-context` 组件：目录摘要、OS、git 分支、工具链 | 提示词快照测试 |
| P1 ✅ | 修正 Stop 语义：单独续跑计数与上限、预留一步工具额度、未验证标记；新增 `verify-before-stop` | Steer 后仍触发验证；最后一步未通过有标记 |
| P1 ✅ | `loop-detect` 组件，参照 OpenHands 阈值并加拒绝熔断（拒绝熔断未做） | 构造重复调用序列断言注入提示或中断 |
| P2 | 压缩保留任务原文、最新计划、todo；失败降级；计入预算 | 多次压缩后关键信息仍在 |
| P2 | `preset.Loop` 增加 Request；`PromptScope` 增加 Final、Deadline | 组件可按步改推理强度、最后一步收到提示 |
| P2 | SWE 式回归集：真实仓库加隐藏测试，从会话日志失败案例抽 20 到 50 题，固定资源并重复运行 | 作为以上改动的量化依据 |
