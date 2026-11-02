# background

有稳定身份的子工作。记录先落盘，重启后用同一执行键重新进入，已完成的持久步骤从日志回放。结果和通知原子提交，通知丢失时只补发，不重新执行业务。

```go
manager, err := background.New(background.Config{Store: store, Name: "agents",
	Notify: background.NotifyHandle(handleOf, startNextTurn)})
asm, err := preset.Build(ctx, resolved, preset.Host{Background: manager /* ... */}) // Run 之前登记 Runner。
go manager.Run(ctx)
```

| 宿主调用 | 作用 |
| --- | --- |
| `TurnStarted`、`TurnEnded` | 开始或清除后台工作的等待期限（默认 30 分钟）。期限持久保存，到期时终止并通知 |
| `Busy` | 轮次结束但仍有后台工作时为真，此时所有者不算空闲 |
| `Handoff` | 交接给人：普通子工作停止并释放槽位；`SurviveHandoff` 的工作继续，不占槽位，完成时不唤醒所有者 |
| `Slots().Acquire` | 普通槽位外保留一个给人的输入；等待时人的输入优先于自动输入，再优先于新任务 |
| `Resume` | 用宿主的输入续跑 `waiting` 的子工作，键取 `Record.Waits` 中路径最后一项的 `Key` |
| `RegisterResource`、`Attach`、`Detach` | 把外部资源交给子工作：进程停止时保留，结束时释放。可以先记意图，分配后再更新 |
| `Problems`、`ResolveProblem` | 查看和裁决无法确认状态的通知与资源释放 |
| `Get`、`Records`、`Wait`、`Kill` | 查询、等待、终止。`Wait` 遇到 `waiting` 也返回 |

| 情况 | 结果 |
| --- | --- |
| 前台任务的父调用取消 | `canceled` |
| 进程停止时的前台任务 | 重启后为 `interrupted`，父调用恢复后用同一 ID 重新进入 |
| 进程停止时的后台任务 | 重启后重新进入。停在已提交的边界（如审批等待）时继续；用 `BuilderRunner` 且停在模型请求中途时重发这次请求（同一回合最多 3 次，可能重复计费，每次计入预算）；后端支持 `KeyedExecutor` 时，`run_command` 重启后附着到原命令并取回结果；其他不可重跑的工具停在中途时结果未知，不重跑，记为 `failed`（`attention`）；实例已不存在时记为 `failed`（`sandbox_gone`） |
| 完成与终止同时发生 | 以先落盘的结果为准，只通知一次 |
| 等待续跑（如审批） | `waiting`，通知所有者，不占槽位，受等待上限约束，重启后不自动重跑 |
| 续跑后崩溃 | 输入不落盘，重新进入后回到 `waiting`，已消费的批准不会被沿用 |
| 失败 | `failed`，只保存分类：`execution`、`panic`、`attention`、`waiting`（等待没有定位信息，无法续跑），或 `effects.FailureError` 指定的分类。错误文本不保存 |
| 重启后的预算 | 从记录恢复，继续计入同一份账本。根预算须是持久的，且与 Manager 使用同一个 Store，否则 `Spawn` 报错 |
| 子工作结束 | 资源释放与结果原子提交，等 Runner 退出后才执行，失败时退避重试，不重新执行业务 |
| 写入时失去运行锁 | 取消全部子工作，`Run` 返回 `durable.ErrLeaseLost` |

通知至少投递一次，每次状态变化有新的 `Generation`，已被取代的通知不再投递。`NotifyHandle` 用 `Handle.SteerOnce` 写入：对同一执行键的持久 Handle，崩溃后补发不会重复进入对话；非持久 Handle 只在进程内去重。`idle` 回调可能被重复调用，宿主用 `Notice.Key` 派生新运行的执行键，重投就会进入同一运行。

释放方法无法确认资源是否存在时返回 `ErrResourceUnknown`，这条释放进入 `Problems`，不再重试，也不重新执行业务；宿主处理后用 `ResolveProblem` 结束它。失去运行锁的旧宿主写不进结果，也就不会释放资源。

Manager 不放宽权限、预算和沙箱，它们由 Runner 使用的装配决定；宿主须在关闭 Assembly 之前停止 Manager。
