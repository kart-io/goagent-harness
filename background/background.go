// Package background 管理有稳定身份的子工作：记录先落盘，重启后找回；结果与通知原子提交，通知只补发不重新执行业务。
//
// 前台子工作随父调用取消。后台子工作由 Manager 持有，所有者只负责接收通知。权限、预算和沙箱由 Runner 使用的装配决定，
// Manager 不放宽任何上限。Run 停止时不改变运行中的记录，它们在下次启动时继续。
package background

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/core/durable"
)

const (
	stateVersion = "goagent-background-v1"
	notifyKind   = "notify"
	writeTimeout = 5 * time.Second
)

// ErrNotRunning 表示 Manager 没有在 Run 中，或已经停止。
var ErrNotRunning = errors.New("background manager is not running")

// Mode 决定子工作的所有权。
type Mode string

const (
	// ModeForeground 随 Spawn 的 ctx 取消，父调用须 Wait 它结束。
	ModeForeground Mode = "foreground"
	// ModeBackground 由 Manager 持有，完成后通知所有者。
	ModeBackground Mode = "background"
)

// Status 是子工作的状态。
type Status string

const (
	StatusRunning     Status = "running"
	StatusInterrupted Status = "interrupted" // 前台任务被进程停止打断，等父调用恢复后重新进入。
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled" // 父调用取消了前台任务。
	StatusKilled      Status = "killed"   // 超过等待上限，或被宿主终止。
	StatusStopped     Status = "stopped"  // 交接给人时停止。
)

func (s Status) terminal() bool {
	return s != StatusRunning && s != StatusInterrupted
}

// Task 描述一项子工作。ID 须稳定（如工具调用的 ExecutionKey）：相同 ID 再次提交不会产生第二份工作。
type Task struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Runner string `json:"runner"`
	Input  string `json:"input"`
	Mode   Mode   `json:"mode"`
	// SurviveHandoff 的工作（如供人查看的服务）在交接后继续，不占槽位、不受等待上限影响，完成时不唤醒所有者。
	SurviveHandoff bool `json:"survive_handoff,omitempty"`
}

// Record 是子工作的持久记录。错误文本不保存，只保存分类。
type Record struct {
	Task
	Status          Status    `json:"status"`
	Result          string    `json:"result,omitempty"`
	ResultTruncated bool      `json:"result_truncated,omitempty"`
	FailureKind     string    `json:"failure_kind,omitempty"`
	Quiet           bool      `json:"quiet,omitempty"`
	WaitUntil       time.Time `json:"wait_until"`
	CreatedAt       time.Time `json:"created_at"`
	FinishedAt      time.Time `json:"finished_at"`
}

// Notice 通知所有者子工作已结束。投递至少一次，所有者按 ChildID 去重。
type Notice struct {
	Owner           string
	ChildID         string
	Runner          string
	Status          Status
	Result          string
	ResultTruncated bool
	FailureKind     string
}

// Runner 执行子工作；ctx 带有该子工作的持久日志和稳定执行键，并在取消时要求尽快返回。
type Runner func(ctx context.Context, task Task) (string, error)

// Config 配置 Manager；零值的上限取默认值。
type Config struct {
	Store durable.Store
	Name  string
	// Notify 投递结束通知，返回 effects.ErrLater 时稍后重试。
	Notify func(context.Context, Notice) error
	// MaxWorkers 是普通槽位数，默认 4；另有一个槽位留给人的输入。
	MaxWorkers int
	// MaxWait 是轮次结束后后台工作的等待上限，默认 30m。
	MaxWait time.Duration
	// MaxResult 是保存的结果字节上限，默认 256 KiB，超出时截断并标记 ResultTruncated。
	MaxResult int
	// RetainFinished 是保留的已结束记录数，默认 1000，超出时删除最早的。被删除的 ID 再次提交会重新执行。
	RetainFinished int
	Poll           time.Duration // 检查等待期限的间隔，默认 1s。
	// Effects 配置通知队列，其中的 Store、Name、Kinds 由 Manager 设置。
	Effects effects.Config
	Now     func() time.Time
}

type liveChild struct {
	cancel  context.CancelCauseFunc
	done    chan struct{}
	release func()
}

// Manager 管理子工作。同一 Store 与 Name 同时只有一个 Run。
type Manager struct {
	cfg      Config
	docID    string
	runnerID string
	queue    *effects.Queue
	slots    *Slots

	mu      sync.Mutex
	runners map[string]Runner
	live    map[string]*liveChild
	runCtx  context.Context
	fail    context.CancelCauseFunc
	wg      sync.WaitGroup
	ready   chan struct{}
	opened  sync.Once
}

type managerState struct {
	Records []Record `json:"records"`
}

// KeyOf 返回子工作的持久执行键。
func KeyOf(name, id string) string { return durable.ChildKey("goagent/background/"+name, id) }

// New 校验配置并创建 Manager。
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil || cfg.Name == "" || cfg.Notify == nil {
		return nil, errors.New("background 需要 Store、Name 和 Notify")
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 30 * time.Minute
	}
	if cfg.MaxResult <= 0 {
		cfg.MaxResult = 256 << 10
	}
	if cfg.RetainFinished <= 0 {
		cfg.RetainFinished = 1000
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	id := "goagent/background/" + cfg.Name
	m := &Manager{cfg: cfg, docID: id, runnerID: id + "/runner", slots: NewSlots(cfg.MaxWorkers),
		runners: map[string]Runner{}, live: map[string]*liveChild{}, ready: make(chan struct{})}
	queueCfg := cfg.Effects
	queueCfg.Store, queueCfg.Name, queueCfg.Now = cfg.Store, "background/"+cfg.Name, cfg.Now
	queueCfg.Kinds = map[string]effects.Kind{notifyKind: {Run: m.deliver, Idempotent: true}}
	queue, err := effects.New(queueCfg)
	if err != nil {
		return nil, err
	}
	m.queue = queue
	return m, nil
}

// Register 登记 Runner。须在 Run 之前登记恢复时用到的全部 Runner。
func (m *Manager) Register(name string, runner Runner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runners[name] = runner
}

// Store 返回子工作日志所用的 Store。
func (m *Manager) Store() durable.Store { return m.cfg.Store }

// Slots 返回并发槽位，宿主用它为人的输入和自动输入排队。
func (m *Manager) Slots() *Slots { return m.slots }

// Ready 等待 Run 完成恢复。
func (m *Manager) Ready(ctx context.Context) error {
	select {
	case <-m.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) load(ctx context.Context) (*managerState, error) {
	snapshot, err := m.cfg.Store.Load(ctx, m.docID)
	if errors.Is(err, os.ErrNotExist) {
		return &managerState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var state managerState
	if snapshot == nil || snapshot.Version != stateVersion || snapshot.RunID != m.docID || json.Unmarshal(snapshot.Data, &state) != nil {
		return nil, errors.New("invalid background record")
	}
	return &state, nil
}

func findRecord(state *managerState, id string) *Record {
	for i := range state.Records {
		if state.Records[i].ID == id {
			return &state.Records[i]
		}
	}
	return nil
}

// update 在记录锁内修改状态，并与运行锁记录和通知一起原子提交。写入被拒绝（失去控制权）时停止 Manager。
func (m *Manager) update(ctx context.Context, change func(*managerState) ([]effects.Effect, error)) (err error) {
	unlock, err := m.cfg.Store.Lock(ctx, m.docID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	state, err := m.load(ctx)
	if err != nil {
		return err
	}
	notices, err := change(state)
	if err != nil {
		return err
	}
	m.trim(state)
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	err = m.queue.Commit(ctx, []durable.Write{
		{ID: m.docID, Snapshot: &durable.Snapshot{RunID: m.docID, Version: stateVersion, Data: data}},
		{ID: m.runnerID, Snapshot: &durable.Snapshot{RunID: m.runnerID, Version: stateVersion, Data: json.RawMessage(`{}`)}},
	}, notices...)
	if errors.Is(err, durable.ErrLeaseLost) {
		m.abort(err)
	}
	return err
}

// trim 只保留最近结束的记录，避免状态随运行时间无限增长。
func (m *Manager) trim(state *managerState) {
	finished := 0
	for i := len(state.Records) - 1; i >= 0; i-- {
		if state.Records[i].Status.terminal() {
			if finished++; finished > m.cfg.RetainFinished {
				state.Records = slices.Delete(state.Records, i, i+1)
			}
		}
	}
}

func (m *Manager) abort(err error) {
	m.mu.Lock()
	fail := m.fail
	m.mu.Unlock()
	if fail != nil {
		fail(err)
	}
}

// Run 恢复后台子工作并运行到 ctx 取消，返回前等待全部子工作退出。取消时运行中的记录保持不变，下次启动继续。
func (m *Manager) Run(ctx context.Context) (err error) {
	unlock, err := m.cfg.Store.Lock(ctx, m.runnerID)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	runCtx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	m.mu.Lock()
	m.runCtx, m.fail = runCtx, fail
	m.mu.Unlock()
	defer func() {
		// 先拒绝新的子工作，再等待已有的退出。
		m.mu.Lock()
		m.runCtx, m.fail = nil, nil
		m.mu.Unlock()
		fail(nil)
		m.wg.Wait()
	}()

	queueDone := make(chan error, 1)
	go func() {
		queueErr := m.queue.Run(runCtx)
		// 通知队列失去控制权时整个 Manager 一起停止。
		if errors.Is(queueErr, durable.ErrLeaseLost) {
			fail(queueErr)
		}
		queueDone <- queueErr
	}()
	defer func() {
		fail(nil)
		if queueErr := <-queueDone; queueErr != nil && !errors.Is(queueErr, context.Canceled) {
			err = errors.Join(err, queueErr)
		}
	}()

	if err := m.resume(runCtx); err != nil {
		return ignoreCanceled(err)
	}
	m.opened.Do(func() { close(m.ready) })

	ticker := time.NewTicker(m.cfg.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-runCtx.Done():
			return causeOf(ctx, runCtx)
		case <-ticker.C:
			if err := m.expire(runCtx); err != nil {
				if errors.Is(err, durable.ErrLeaseLost) {
					return err
				}
				if runCtx.Err() != nil {
					return causeOf(ctx, runCtx)
				}
			}
		}
	}
}

func causeOf(parent, run context.Context) error {
	if parent.Err() != nil {
		return nil
	}
	return ignoreCanceled(context.Cause(run))
}

func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// resume 启动上次未结束的后台工作；前台工作记为中断，等父调用恢复后用同一 ID 重新进入。
func (m *Manager) resume(ctx context.Context) error {
	var restart []Record
	err := m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		restart = nil
		for i := range state.Records {
			record := &state.Records[i]
			if record.Status != StatusRunning {
				continue
			}
			if record.Mode == ModeForeground {
				record.Status = StatusInterrupted
				continue
			}
			restart = append(restart, *record)
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	for _, record := range restart {
		m.launch(context.WithoutCancel(ctx), record.Task, record.Quiet)
	}
	return nil
}

// Spawn 记录并启动子工作，记录落盘后才开始执行。相同 ID 再次提交返回现有记录，内容不同时报错。
func (m *Manager) Spawn(ctx context.Context, task Task) (Record, error) {
	if task.ID == "" || task.Owner == "" || (task.Mode != ModeForeground && task.Mode != ModeBackground) {
		return Record{}, errors.New("background 任务需要 ID、Owner 和有效的 Mode")
	}
	m.mu.Lock()
	running := m.runCtx != nil
	_, known := m.runners[task.Runner]
	m.mu.Unlock()
	if !running {
		return Record{}, ErrNotRunning
	}
	if !known {
		return Record{}, fmt.Errorf("Runner %q 未注册", task.Runner)
	}
	var result Record
	start := false
	err := m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		start = false
		record := findRecord(state, task.ID)
		if record != nil {
			if record.Task != task {
				return nil, fmt.Errorf("子工作 %q 已存在且内容不同", task.ID)
			}
			if record.Status == StatusInterrupted {
				record.Status, start = StatusRunning, true
			} else if record.Status == StatusRunning && !m.isLive(task.ID) {
				start = true
			}
			result = *record
			return nil, nil
		}
		result = Record{Task: task, Status: StatusRunning, CreatedAt: m.cfg.Now()}
		state.Records = append(state.Records, result)
		start = true
		return nil, nil
	})
	if err != nil {
		return Record{}, err
	}
	if start {
		base := context.WithoutCancel(ctx)
		child := m.launch(base, task, result.Quiet)
		if task.Mode == ModeForeground && child != nil {
			stop := context.AfterFunc(ctx, func() { child.cancel(errParentCanceled) })
			go func() {
				<-child.done
				stop()
			}()
		}
	}
	return result, nil
}

func (m *Manager) isLive(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.live[id]
	return ok
}

var (
	errParentCanceled = errors.New("parent canceled")
	errKilled         = errors.New("killed")
)

// launch 在独立 goroutine 中运行子工作。base 提供 ctx 中的值（如预算），取消由 Manager 控制。
func (m *Manager) launch(base context.Context, task Task, quiet bool) *liveChild {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runCtx == nil {
		return nil
	}
	if child, ok := m.live[task.ID]; ok {
		return child
	}
	runner := m.runners[task.Runner]
	runCtx := m.runCtx
	ctx, cancel := context.WithCancelCause(durable.WithJournal(durable.WithExecutionKey(base, KeyOf(m.cfg.Name, task.ID)), m.cfg.Store))
	stopFollow := context.AfterFunc(runCtx, func() { cancel(context.Canceled) })
	child := &liveChild{cancel: cancel, done: make(chan struct{}), release: func() {}}
	m.live[task.ID] = child
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(child.done)
		defer stopFollow()
		defer cancel(nil)
		defer func() {
			m.mu.Lock()
			release := child.release
			child.release = func() {}
			delete(m.live, task.ID)
			m.mu.Unlock()
			release()
		}()
		if !quiet {
			release, err := m.slots.Acquire(ctx, PriorityLaunch)
			if err != nil {
				m.finishIfOwned(runCtx, ctx, task, "", err)
				return
			}
			m.mu.Lock()
			child.release = release
			m.mu.Unlock()
		}
		if runner == nil {
			m.finishIfOwned(runCtx, ctx, task, "", errors.New("Runner 未注册"))
			return
		}
		result, err := invoke(ctx, runner, task)
		m.finishIfOwned(runCtx, ctx, task, result, err)
	}()
	return child
}

func invoke(ctx context.Context, runner Runner, task Task) (result string, err error) {
	defer func() {
		if v := recover(); v != nil {
			result, err = "", errPanic
		}
	}()
	return runner(ctx, task)
}

var errPanic = errors.New("runner panic")

// finishIfOwned 写入结果。Manager 已停止时不写入：运行中的记录留给下次启动。
func (m *Manager) finishIfOwned(runCtx, ctx context.Context, task Task, result string, runErr error) {
	if runCtx.Err() != nil {
		return
	}
	status, kind := StatusCompleted, ""
	if cause := context.Cause(ctx); runErr != nil || errors.Is(cause, errParentCanceled) {
		switch {
		case errors.Is(cause, errParentCanceled):
			status = StatusCanceled
		case errors.Is(runErr, errPanic):
			status, kind = StatusFailed, "panic"
		case errors.Is(runErr, durable.ErrOperationAttention):
			status, kind = StatusFailed, "attention"
		case durable.Waits(runErr) != nil:
			// 后台没有人能续跑审批等待，交给宿主。
			status, kind = StatusFailed, "waiting"
		default:
			status, kind = StatusFailed, "execution"
		}
		result = ""
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	// 写入失败时记录仍是 running，下次启动重新进入，已完成的持久步骤回放。
	_ = m.finish(saveCtx, task.ID, status, result, kind)
}

// finish 把运行中的记录改为终态并提交通知；已是终态时不变，先落盘的结果为准。
func (m *Manager) finish(ctx context.Context, id string, status Status, result, kind string) error {
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		record := findRecord(state, id)
		if record == nil || record.Status.terminal() {
			return nil, nil
		}
		return m.settle(record, status, result, kind)
	})
}

func (m *Manager) settle(record *Record, status Status, result, kind string) ([]effects.Effect, error) {
	if len(result) > m.cfg.MaxResult {
		cut := m.cfg.MaxResult
		for cut > 0 && !utf8.RuneStart(result[cut]) {
			cut--
		}
		result, record.ResultTruncated = result[:cut], true
	}
	record.Status, record.Result, record.FailureKind, record.FinishedAt, record.WaitUntil = status, result, kind, m.cfg.Now(), time.Time{}
	if record.Mode != ModeBackground || record.Quiet {
		return nil, nil
	}
	payload, err := json.Marshal(record.ID)
	if err != nil {
		return nil, err
	}
	return []effects.Effect{{Kind: notifyKind, Key: record.ID, Target: record.Owner, Payload: payload}}, nil
}

// deliver 按记录投递通知。结果不放进待办内容，内容大小不受结果影响。
func (m *Manager) deliver(ctx context.Context, effect effects.Effect) error {
	var id string
	if err := json.Unmarshal(effect.Payload, &id); err != nil {
		return effects.Drop("corrupt_notice")
	}
	record, err := m.Get(ctx, id)
	if errors.Is(err, os.ErrNotExist) {
		return effects.Drop("record_removed")
	}
	if err != nil {
		return err
	}
	return m.cfg.Notify(ctx, Notice{Owner: record.Owner, ChildID: record.ID, Runner: record.Runner, Status: record.Status,
		Result: record.Result, ResultTruncated: record.ResultTruncated, FailureKind: record.FailureKind})
}

// Get 返回子工作的记录。
func (m *Manager) Get(ctx context.Context, id string) (Record, error) {
	state, err := m.load(ctx)
	if err != nil {
		return Record{}, err
	}
	record := findRecord(state, id)
	if record == nil {
		return Record{}, fmt.Errorf("子工作 %q: %w", id, os.ErrNotExist)
	}
	return *record, nil
}

// Records 返回所有者的全部记录；owner 为空时返回全部。
func (m *Manager) Records(ctx context.Context, owner string) ([]Record, error) {
	state, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, record := range state.Records {
		if owner == "" || record.Owner == owner {
			records = append(records, record)
		}
	}
	return records, nil
}

// Busy 报告所有者是否还有需要等待的工作。轮次结束但 Busy 为真时，所有者不算空闲。交接后的豁免工作不计入。
func (m *Manager) Busy(ctx context.Context, owner string) (bool, error) {
	records, err := m.Records(ctx, owner)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.Status == StatusRunning && !record.Quiet {
			return true, nil
		}
	}
	return false, nil
}

// Wait 等待子工作结束并返回记录。ctx 只控制等待。
func (m *Manager) Wait(ctx context.Context, id string) (Record, error) {
	ticker := time.NewTicker(m.cfg.Poll)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		child := m.live[id]
		m.mu.Unlock()
		if child != nil {
			select {
			case <-child.done:
			case <-ctx.Done():
				return Record{}, ctx.Err()
			}
		}
		record, err := m.Get(ctx, id)
		if err != nil {
			return Record{}, err
		}
		if record.Status.terminal() || record.Status == StatusInterrupted {
			return record, nil
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return Record{}, ctx.Err()
		}
	}
}

// Kill 终止子工作，记为 killed 并通知所有者。结果先落盘，随后的完成不再改变它。已结束时不做任何事。
func (m *Manager) Kill(ctx context.Context, id string) error {
	if err := m.finish(ctx, id, StatusKilled, "", ""); err != nil {
		return err
	}
	m.stopChild(id, errKilled)
	return nil
}

func (m *Manager) stopChild(id string, cause error) {
	m.mu.Lock()
	child := m.live[id]
	m.mu.Unlock()
	if child != nil {
		child.cancel(cause)
	}
}

// TurnEnded 在所有者的轮次结束时调用：开始计算后台工作的等待期限。期限持久保存，重启后不重新计时。
func (m *Manager) TurnEnded(ctx context.Context, owner string) error {
	until := m.cfg.Now().Add(m.cfg.MaxWait)
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		for i := range state.Records {
			record := &state.Records[i]
			if record.Owner == owner && record.Status == StatusRunning && record.Mode == ModeBackground && !record.Quiet && record.WaitUntil.IsZero() {
				record.WaitUntil = until
			}
		}
		return nil, nil
	})
}

// TurnStarted 在所有者开始新一轮时调用：清除等待期限。
func (m *Manager) TurnStarted(ctx context.Context, owner string) error {
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		for i := range state.Records {
			if state.Records[i].Owner == owner {
				state.Records[i].WaitUntil = time.Time{}
			}
		}
		return nil, nil
	})
}

// expire 终止超过等待期限的后台工作。
func (m *Manager) expire(ctx context.Context) error {
	state, err := m.load(ctx)
	if err != nil {
		return err
	}
	now := m.cfg.Now()
	var expired []string
	for _, record := range state.Records {
		if record.Status == StatusRunning && !record.WaitUntil.IsZero() && !record.WaitUntil.After(now) {
			expired = append(expired, record.ID)
		}
	}
	for _, id := range expired {
		if err := m.Kill(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Handoff 在交接给人时调用：普通子工作停止，豁免的工作继续但释放槽位且不再唤醒所有者。
func (m *Manager) Handoff(ctx context.Context, owner string) error {
	var stopped, exempt []string
	err := m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		stopped, exempt = nil, nil
		for i := range state.Records {
			record := &state.Records[i]
			if record.Owner != owner || record.Status != StatusRunning {
				continue
			}
			if record.SurviveHandoff {
				record.Quiet, record.WaitUntil = true, time.Time{}
				exempt = append(exempt, record.ID)
				continue
			}
			record.Quiet = true
			if _, err := m.settle(record, StatusStopped, "", ""); err != nil {
				return nil, err
			}
			stopped = append(stopped, record.ID)
		}
		return nil, nil
	})
	if err != nil {
		return err
	}
	for _, id := range stopped {
		m.releaseSlot(id)
		m.stopChild(id, errKilled)
	}
	for _, id := range exempt {
		m.releaseSlot(id)
	}
	return nil
}

func (m *Manager) releaseSlot(id string) {
	m.mu.Lock()
	child := m.live[id]
	release := func() {}
	if child != nil {
		release = child.release
		child.release = func() {}
	}
	m.mu.Unlock()
	release()
}
