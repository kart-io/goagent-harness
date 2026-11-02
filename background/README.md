# background

有稳定身份的子工作。记录先落盘，重启后用同一执行键继续，已完成的持久步骤从日志回放。结果和通知原子提交，通知丢失时只补发，不重新执行业务。

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
| `Get`、`Records`、`Wait`、`Kill` | 查询、等待、终止 |

| 情况 | 结果 |
| --- | --- |
| 前台任务的父调用取消 | `canceled` |
| 进程停止时的前台任务 | 重启后为 `interrupted`，父调用恢复后用同一 ID 重新进入 |
| 进程停止时的后台任务 | 重启后继续 |
| 完成与终止同时发生 | 以先落盘的结果为准，只通知一次 |
| 失败 | `failed`，只保存分类：`execution`、`panic`、`attention`、`waiting`。错误文本不保存 |
| 写入时失去运行锁 | 取消全部子工作，`Run` 返回 `durable.ErrLeaseLost` |

通知至少投递一次，所有者按 `ChildID` 去重。后台没有人能续跑审批等待，这类子工作记为失败（`waiting`）。Manager 不放宽权限、预算和沙箱，它们由 Runner 使用的装配决定；宿主须在关闭 Assembly 之前停止 Manager。
