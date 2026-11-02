# effects

持久的外部副作用队列，用于发消息、写评论这类不能丢也不能随意重做的动作。

```go
queue, err := effects.New(effects.Config{Store: store, Name: "outbox", Kinds: map[string]effects.Kind{
	"comment": {Run: postComment, Reconcile: commentExists},
}})
go queue.Run(ctx)

// 调用方持有 decision 的锁：决定与待办要么都写入，要么都不写入。
err = queue.Commit(ctx, []durable.Write{decision}, effects.Effect{Kind: "comment", Key: "pr-1/ack", Target: "pr-1", Payload: payload})
```

| 情况 | 行为 |
| --- | --- |
| 同一 Kind 与 Key 在等待 | 替换内容，不改变排队位置 |
| 同一 Kind 与 Key 在执行 | 新内容排在其后 |
| 同一 Kind 与 Key 结果未知 | 入队失败，先用 `Resolve` 裁决 |
| 同一 Target | 保持顺序；前面的待办连续失败 3 次后不再阻塞，由 `Problems` 暴露 |
| 返回 `ErrLater` | 稍后重试，不计入失败 |
| 返回 `Drop(reason)` | 撤销并保留原因 |
| `Authorize` 返回错误 | 不执行，撤销，原因记为 `unauthorized` |
| 返回普通错误 | 按失败退避重试，只保存分类，不保存错误文本 |
| 执行中进程退出或返回 `ErrUnconfirmed` | 有 `Reconcile` 时先核对；声明 `Idempotent` 时重做；否则进入 `attention` 等宿主裁决 |
| 超过长度或大小上限 | 入队失败（`ErrLimit`），不截断 |
| 写入时失去运行锁 | `Run` 停止并返回 `durable.ErrLeaseLost` |

不承诺恰好一次，目标系统应按 `Effect.Key` 去重。待办内容会持久保存，不要放入秘密，凭据在 `Run` 中解析。同一 `Store` 与 `Name` 同时只有一个 `Run`。
