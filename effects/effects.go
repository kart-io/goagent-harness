// Package effects 是持久的外部副作用队列：决定与待办在同一次提交中写入，提交后才执行，崩溃后继续。
//
// 不承诺恰好一次。执行中断或结果未知时，只有声明了 Idempotent 或提供了 Reconcile 的种类才会继续，其余交给宿主。
// 目标系统应按 Effect.Key 去重。待办内容不要包含秘密，凭据由 Kind.Run 在执行时解析。
package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/core/durable"
)

const (
	stateVersion = "goagent-effects-v1"
	// settleTimeout 限制一次结果写入；执行结束后的结果不跟随 Run 的取消。
	settleTimeout = 5 * time.Second

	// DefaultHoldAfter 是待办连续失败多少次后不再阻塞同目标的后续待办。
	DefaultHoldAfter = 3
)

// ErrLater 表示暂时不能执行（如目标停用、没有空闲槽位），稍后重试且不计入失败。
var ErrLater = errors.New("effect deferred")

// ErrUnconfirmed 表示执行结果未知，与 sandbox.ErrUnconfirmed 是同一个错误。
var ErrUnconfirmed = sandbox.ErrUnconfirmed

// ErrLimit 表示超过队列长度或内容大小的上限，待办未写入。
var ErrLimit = errors.New("effect queue limit exceeded")

// DropError 表示目标永久失效，待办撤销并保留原因。
type DropError struct{ Reason string }

func (e *DropError) Error() string { return "effect dropped: " + e.Reason }

// Drop 返回撤销待办的错误。reason 应是固定的分类，会被持久保存。
func Drop(reason string) error { return &DropError{Reason: reason} }

// FailureError 为失败指定保存的分类；其他错误的分类都是 execution，错误文本不保存，避免持久化第三方错误中的凭据。
type FailureError struct {
	Kind string
	Err  error
}

func (e *FailureError) Error() string { return e.Kind + ": " + e.Err.Error() }
func (e *FailureError) Unwrap() error { return e.Err }

// State 是待办的状态。
type State string

const (
	StateWaiting   State = "waiting"
	StateRunning   State = "running"
	StateAttention State = "attention"
	StateDone      State = "done"
	StateDropped   State = "dropped"
)

// Effect 是一项待执行的外部副作用。同一 Kind 与 Key 的等待项会合并；Target 相同的待办保持顺序，空 Target 不参与排序。
type Effect struct {
	Kind    string          `json:"kind"`
	Key     string          `json:"key"`
	Target  string          `json:"target,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// Record 是待办的持久记录。
type Record struct {
	Effect
	Seq         uint64    `json:"seq"`
	State       State     `json:"state"`
	Attempts    int       `json:"attempts"`
	Failures    int       `json:"failures"`
	FailureKind string    `json:"failure_kind,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	NextAt      time.Time `json:"next_at"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
	SettledAt   time.Time `json:"settled_at"`
}

func (r *Record) settled() bool { return r.State == StateDone || r.State == StateDropped }

// Kind 定义一类待办的执行方式。
type Kind struct {
	// Run 执行待办，返回 ErrLater、ErrUnconfirmed、Drop 或普通错误（退避重试）。ctx 在 Run 停止时取消，此时结果按未知处理。
	Run func(context.Context, Effect) error
	// Authorize 在每次执行前按当前策略校验；返回错误时撤销待办，原因记为 unauthorized。
	Authorize func(context.Context, Effect) error
	// Reconcile 在结果未知时查询是否已生效：是则完成，否则重新执行；出错时交给宿主。
	Reconcile func(context.Context, Effect) (applied bool, err error)
	// Idempotent 声明目标按 Key 去重，结果未知时可直接重新执行。提供了 Reconcile 时以核对结果为准。
	Idempotent bool
}

// Config 配置队列；零值的时长与上限取默认值。
type Config struct {
	Store durable.Store
	// Name 区分同一 Store 中的队列。
	Name  string
	Kinds map[string]Kind

	MaxPending    int // 未结束待办的上限，默认 1000。
	MaxPayload    int // 单条内容的字节上限，默认 64 KiB。
	RetainSettled int // 保留的已结束记录数，默认 100。
	Concurrency   int // 同时执行的待办数，默认 4。
	HoldAfter     int // 默认 DefaultHoldAfter。

	MinBackoff time.Duration // 默认 5s，每次失败翻倍。
	MaxBackoff time.Duration // 默认 5m。
	LaterDelay time.Duration // ErrLater 之后的等待，默认 5s。
	Poll       time.Duration // 检查到期待办的间隔，默认 2s。
	Now        func() time.Time
}

// Queue 是持久队列。同一 Store 与 Name 同时只有一个 Run 执行待办，Commit 可以从任意实例调用。
type Queue struct {
	cfg      Config
	docID    string
	runnerID string
	wake     chan struct{}
}

type queueState struct {
	Next    uint64   `json:"next"`
	Records []Record `json:"records"`
}

// New 校验配置并创建队列。
func New(cfg Config) (*Queue, error) {
	if cfg.Store == nil || cfg.Name == "" {
		return nil, errors.New("effects 需要 Store 和 Name")
	}
	for name, kind := range cfg.Kinds {
		if kind.Run == nil {
			return nil, fmt.Errorf("effects 种类 %q 需要 Run", name)
		}
	}
	setDefault(&cfg.MaxPending, 1000)
	setDefault(&cfg.MaxPayload, 64<<10)
	setDefault(&cfg.RetainSettled, 100)
	setDefault(&cfg.Concurrency, 4)
	setDefault(&cfg.HoldAfter, DefaultHoldAfter)
	setDefault(&cfg.MinBackoff, 5*time.Second)
	setDefault(&cfg.MaxBackoff, 5*time.Minute)
	setDefault(&cfg.LaterDelay, 5*time.Second)
	setDefault(&cfg.Poll, 2*time.Second)
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	id := "goagent/effects/" + cfg.Name
	return &Queue{cfg: cfg, docID: id, runnerID: id + "/runner", wake: make(chan struct{}, 1)}, nil
}

func setDefault[T int | time.Duration](v *T, value T) {
	if *v <= 0 {
		*v = value
	}
}

func (q *Queue) load(ctx context.Context) (*queueState, error) {
	snapshot, err := q.cfg.Store.Load(ctx, q.docID)
	if errors.Is(err, os.ErrNotExist) {
		return &queueState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state queueState
	if snapshot == nil || snapshot.Version != stateVersion || snapshot.RunID != q.docID || json.Unmarshal(snapshot.Data, &state) != nil {
		return nil, errors.New("invalid effects queue record")
	}
	return &state, nil
}

func (q *Queue) docWrite(state *queueState) (durable.Write, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return durable.Write{}, err
	}
	return durable.Write{ID: q.docID, Snapshot: &durable.Snapshot{RunID: q.docID, Version: stateVersion, Data: data}}, nil
}

// update 在队列锁内修改状态并与 extra 一起提交。
func (q *Queue) update(ctx context.Context, extra []durable.Write, change func(*queueState) error) (err error) {
	unlock, err := q.cfg.Store.Lock(ctx, q.docID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	state, err := q.load(ctx)
	if err != nil {
		return err
	}
	if err := change(state); err != nil {
		return err
	}
	doc, err := q.docWrite(state)
	if err != nil {
		return err
	}
	return q.cfg.Store.SaveAll(ctx, append(slices.Clone(extra), doc)...)
}

// Enqueue 提交待办，没有同时写入的决定时使用。
func (q *Queue) Enqueue(ctx context.Context, effects ...Effect) error {
	return q.Commit(ctx, nil, effects...)
}

// Commit 把决定与待办作为一次原子提交：要么都写入，要么都不写入。调用方须持有 decisions 中每个 ID 的锁。
func (q *Queue) Commit(ctx context.Context, decisions []durable.Write, effects ...Effect) error {
	for _, effect := range effects {
		if _, ok := q.cfg.Kinds[effect.Kind]; !ok {
			return fmt.Errorf("effects 种类 %q 未注册", effect.Kind)
		}
		if effect.Key == "" {
			return errors.New("effects 需要 Key")
		}
		if len(effect.Payload) > q.cfg.MaxPayload {
			return fmt.Errorf("%w：内容 %d 字节，上限 %d", ErrLimit, len(effect.Payload), q.cfg.MaxPayload)
		}
		if len(effect.Payload) > 0 && !json.Valid(effect.Payload) {
			return errors.New("effects 的内容须是 JSON")
		}
	}
	err := q.update(ctx, decisions, func(state *queueState) error {
		for _, effect := range effects {
			if err := q.add(state, effect); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		q.notify()
	}
	return err
}

func (q *Queue) add(state *queueState, effect Effect) error {
	open := 0
	for i := range state.Records {
		record := &state.Records[i]
		if record.settled() {
			continue
		}
		open++
		if record.Kind != effect.Kind || record.Key != effect.Key {
			continue
		}
		switch record.State {
		case StateWaiting:
			// 合并：替换内容，不改变排队位置。
			record.Payload, record.Target = effect.Payload, effect.Target
			return nil
		case StateAttention:
			return fmt.Errorf("待办 %s/%s 结果未知，须先由宿主处理", effect.Kind, effect.Key)
		}
		// 执行中的内容不替换，新内容排在其后。
	}
	if open >= q.cfg.MaxPending {
		return fmt.Errorf("%w：未结束待办已达 %d", ErrLimit, q.cfg.MaxPending)
	}
	now := q.cfg.Now()
	state.Next++
	state.Records = append(state.Records, Record{Effect: effect, Seq: state.Next, State: StateWaiting, NextAt: now, EnqueuedAt: now})
	return nil
}

func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Records 返回全部记录，按入队顺序。
func (q *Queue) Records(ctx context.Context) ([]Record, error) {
	state, err := q.load(ctx)
	if err != nil {
		return nil, err
	}
	return state.Records, nil
}

// Problems 返回需要宿主关注的记录：结果未知的，以及连续失败达到阈值的。
func (q *Queue) Problems(ctx context.Context) ([]Record, error) {
	records, err := q.Records(ctx)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(r Record) bool {
		return r.settled() || (r.State != StateAttention && r.Failures < q.cfg.HoldAfter)
	}), nil
}

// Resolve 由宿主裁决结果未知的待办：applied 为真时记为完成，否则重新排队。
func (q *Queue) Resolve(ctx context.Context, seq uint64, applied bool) error {
	err := q.update(ctx, nil, func(state *queueState) error {
		record := findRecord(state, seq)
		if record == nil || record.State != StateAttention {
			return fmt.Errorf("待办 %d 不在等待裁决的状态", seq)
		}
		q.applyResolution(record, applied, "host")
		q.trim(state)
		return nil
	})
	if err == nil {
		q.notify()
	}
	return err
}

func (q *Queue) applyResolution(record *Record, applied bool, by string) {
	record.FailureKind = ""
	if applied {
		record.State, record.Reason, record.SettledAt = StateDone, "reconciled_by_"+by, q.cfg.Now()
		return
	}
	record.State, record.NextAt = StateWaiting, q.cfg.Now()
}

func findRecord(state *queueState, seq uint64) *Record {
	for i := range state.Records {
		if state.Records[i].Seq == seq {
			return &state.Records[i]
		}
	}
	return nil
}

// trim 只保留最近的已结束记录。
func (q *Queue) trim(state *queueState) {
	settled := 0
	for i := len(state.Records) - 1; i >= 0; i-- {
		if state.Records[i].settled() {
			if settled++; settled > q.cfg.RetainSettled {
				state.Records = slices.Delete(state.Records, i, i+1)
			}
		}
	}
}

// Run 执行待办直到 ctx 取消，返回前等待进行中的执行结束；未结束的待办保留到下次启动。
// 取消时返回 nil。失去运行锁（durable.ErrLeaseLost）时停止并返回错误，不再继续执行。
func (q *Queue) Run(ctx context.Context) (err error) {
	unlock, err := q.cfg.Store.Lock(ctx, q.runnerID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type finished struct {
		seq uint64
		err error
	}
	results := make(chan finished)
	inflight := 0
	// 运行结束前收回全部执行：它们使用 runCtx，也要把结果写回。
	defer func() {
		cancel()
		for ; inflight > 0; inflight-- {
			done := <-results
			err = errors.Join(err, ignoreCanceled(q.settle(ctx, done.seq, done.err)))
		}
	}()

	if err := q.recoverInterrupted(runCtx); err != nil {
		return ignoreCanceled(err)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		claimed, wait, err := q.claim(runCtx, q.cfg.Concurrency-inflight)
		if err != nil {
			return ignoreCanceled(err)
		}
		for _, record := range claimed {
			inflight++
			go func() { results <- finished{record.Seq, q.execute(runCtx, record)} }()
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return nil
		case done := <-results:
			inflight--
			if err := q.settle(ctx, done.seq, done.err); err != nil {
				return ignoreCanceled(err)
			}
		case <-q.wake:
		case <-timer.C:
		}
	}
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// fence 随状态一起写入运行锁对应的记录。以租约实现锁的 Store 据此拒绝已失去运行权的写入。
func (q *Queue) fence() []durable.Write {
	return []durable.Write{{ID: q.runnerID, Snapshot: &durable.Snapshot{RunID: q.runnerID, Version: stateVersion, Data: json.RawMessage(`{}`)}}}
}

// recoverInterrupted 处理上次运行留下的 running 记录：它们的结果未知。
func (q *Queue) recoverInterrupted(ctx context.Context) error {
	state, err := q.load(ctx)
	if err != nil {
		return err
	}
	for i := range state.Records {
		if state.Records[i].State == StateRunning {
			if err := q.judgeUnknown(ctx, state.Records[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// judgeUnknown 判定结果未知的待办。
func (q *Queue) judgeUnknown(ctx context.Context, record Record) error {
	kind, known := q.cfg.Kinds[record.Kind]
	applied, retry := false, false
	if known && kind.Reconcile != nil {
		var err error
		if applied, err = reconcile(ctx, kind, record.Effect); err == nil {
			retry = !applied
		}
	} else if known && kind.Idempotent {
		retry = true
	}
	return q.update(ctx, q.fence(), func(state *queueState) error {
		current := findRecord(state, record.Seq)
		if current == nil {
			return nil
		}
		switch {
		case applied:
			q.applyResolution(current, true, "reconcile")
		case retry:
			// 按失败退避：目标持续返回结果未知时不能空转。
			current.Failures++
			q.applyResolution(current, false, "reconcile")
			current.NextAt = q.cfg.Now().Add(q.backoff(current.Failures))
		default:
			current.State, current.FailureKind = StateAttention, "unconfirmed"
		}
		q.trim(state)
		return nil
	})
}

func reconcile(ctx context.Context, kind Kind, effect Effect) (applied bool, err error) {
	defer func() {
		if v := recover(); v != nil {
			applied, err = false, fmt.Errorf("核对时 panic: %v", v)
		}
	}()
	return kind.Reconcile(ctx, effect)
}

// claim 认领到期的待办，认领写入后才执行；同时返回距离下一次检查的时长。
func (q *Queue) claim(ctx context.Context, slots int) (claimed []Record, wait time.Duration, err error) {
	wait = q.cfg.Poll
	if slots <= 0 {
		return nil, wait, nil
	}
	state, err := q.load(ctx)
	if err != nil {
		return nil, wait, err
	}
	now := q.cfg.Now()
	due := q.due(state, now, slots, &wait)
	if len(due) == 0 {
		return nil, wait, nil
	}
	// 校验在锁外进行，可能调用宿主代码。
	denied := map[uint64]bool{}
	for _, seq := range due {
		record := findRecord(state, seq)
		if authorize := q.cfg.Kinds[record.Kind].Authorize; authorize != nil {
			if err := safeAuthorize(ctx, authorize, record.Effect); err != nil {
				denied[seq] = true
			}
		}
	}
	err = q.update(ctx, q.fence(), func(state *queueState) error {
		claimed = claimed[:0]
		for _, seq := range due {
			record := findRecord(state, seq)
			// 锁外期间记录可能被合并或裁决，只认领仍在等待的。
			if record == nil || record.State != StateWaiting {
				continue
			}
			if denied[seq] {
				record.State, record.Reason, record.SettledAt = StateDropped, "unauthorized", q.cfg.Now()
				continue
			}
			record.State = StateRunning
			record.Attempts++
			claimed = append(claimed, *record)
		}
		q.trim(state)
		return nil
	})
	if err != nil {
		return nil, wait, err
	}
	if len(claimed) < len(due) {
		wait = 0
	}
	return claimed, wait, nil
}

func safeAuthorize(ctx context.Context, authorize func(context.Context, Effect) error, effect Effect) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("校验时 panic: %v", v)
		}
	}()
	return authorize(ctx, effect)
}

// due 选出可执行的待办。同目标中，前面的待办未结束且连续失败未达阈值时，后面的等待。
func (q *Queue) due(state *queueState, now time.Time, slots int, wait *time.Duration) []uint64 {
	var due []uint64
	blocked := map[string]bool{}
	for i := range state.Records {
		record := &state.Records[i]
		if record.settled() || record.State == StateAttention {
			continue
		}
		held := record.Target != "" && blocked[record.Target]
		if record.Target != "" && (record.State == StateRunning || record.Failures < q.cfg.HoldAfter) {
			blocked[record.Target] = true
		}
		if record.State != StateWaiting || held {
			continue
		}
		if until := record.NextAt.Sub(now); until > 0 {
			if until < *wait {
				*wait = until
			}
			continue
		}
		if len(due) < slots {
			due = append(due, record.Seq)
		}
	}
	return due
}

func (q *Queue) execute(ctx context.Context, record Record) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = &FailureError{Kind: "panic", Err: fmt.Errorf("%v", v)}
		}
	}()
	return q.cfg.Kinds[record.Kind].Run(ctx, record.Effect)
}

// settle 写入执行结果。Run 的 ctx 已取消时，被中断的执行留在 running，由下次启动判定。
func (q *Queue) settle(ctx context.Context, seq uint64, runErr error) error {
	if ctx.Err() != nil && runErr != nil {
		var drop *DropError
		if !errors.As(runErr, &drop) && !errors.Is(runErr, ErrLater) {
			return nil
		}
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if errors.Is(runErr, ErrUnconfirmed) {
		state, err := q.load(saveCtx)
		if err != nil {
			return err
		}
		record := findRecord(state, seq)
		if record == nil {
			return nil
		}
		return q.judgeUnknown(saveCtx, *record)
	}
	return q.update(saveCtx, q.fence(), func(state *queueState) error {
		record := findRecord(state, seq)
		if record == nil {
			return nil
		}
		now := q.cfg.Now()
		var drop *DropError
		switch {
		case runErr == nil:
			record.State, record.FailureKind, record.SettledAt = StateDone, "", now
		case errors.As(runErr, &drop):
			record.State, record.Reason, record.SettledAt = StateDropped, drop.Reason, now
		case errors.Is(runErr, ErrLater):
			record.State, record.NextAt = StateWaiting, now.Add(q.cfg.LaterDelay)
		default:
			record.Failures++
			record.State, record.FailureKind, record.NextAt = StateWaiting, failureKind(runErr), now.Add(q.backoff(record.Failures))
		}
		q.trim(state)
		return nil
	})
}

func failureKind(err error) string {
	var failure *FailureError
	if errors.As(err, &failure) && failure.Kind != "" {
		return failure.Kind
	}
	return "execution"
}

func (q *Queue) backoff(failures int) time.Duration {
	delay := q.cfg.MinBackoff
	for i := 1; i < failures && delay < q.cfg.MaxBackoff; i++ {
		delay *= 2
	}
	return min(delay, q.cfg.MaxBackoff)
}
