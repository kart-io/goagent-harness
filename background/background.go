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
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
)

const (
	stateVersion = "goagent-background-v1"
	notifyKind   = "notify"
	releaseKind  = "release"
	writeTimeout = 5 * time.Second
)

// ErrNotRunning 表示 Manager 没有在 Run 中，或已经停止。
var ErrNotRunning = errors.New("background manager is not running")

// ErrNotWaiting 表示子工作不在等待续跑。
var ErrNotWaiting = errors.New("background child is not waiting")

// ErrResourceUnknown 由资源的释放方法返回，表示无法确认资源是否存在。释放交给宿主裁决，见 Manager.Problems。
var ErrResourceUnknown = errors.New("background resource state is unknown")

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
	StatusWaiting     Status = "waiting"     // 等待宿主续跑（如审批），不占槽位，重启后不自动重跑。
	StatusInterrupted Status = "interrupted" // 前台任务被进程停止打断，等父调用恢复后重新进入。
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
	StatusCanceled    Status = "canceled" // 父调用取消了前台任务。
	StatusKilled      Status = "killed"   // 超过等待上限，或被宿主终止。
	StatusStopped     Status = "stopped"  // 交接给人时停止。
)

func (s Status) terminal() bool {
	return s != StatusRunning && s != StatusWaiting && s != StatusInterrupted
}

// Task 描述一项子工作。ID 须稳定（如工具调用的 ExecutionKey）：相同 ID 再次提交不会产生第二份工作。
type Task struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Runner string `json:"runner"`
	Input  string `json:"input"`
	Mode   Mode   `json:"mode"`
	// Session 是发起时的会话 ID，由 Runner 还原：重启后没有父调用的 ctx。
	Session string `json:"session,omitempty"`
	// SurviveHandoff 的工作（如供人查看的服务）在交接后继续，不占槽位、不受等待上限影响，完成时不唤醒所有者。
	SurviveHandoff bool `json:"survive_handoff,omitempty"`
}

// Record 是子工作的持久记录。错误文本不保存，只保存分类。
type Record struct {
	Task
	Status          Status `json:"status"`
	Result          string `json:"result,omitempty"`
	ResultTruncated bool   `json:"result_truncated,omitempty"`
	FailureKind     string `json:"failure_kind,omitempty"`
	// Waits 是等待中的位置，只含定位信息；Resume 的键取 Path 最后一项的 Key。
	Waits []*durable.WaitError `json:"waits,omitempty"`
	// Generation 在每次通知时递增。
	Generation int `json:"generation,omitempty"`
	// Budget 是发起时的根预算配置，重启后据此连回同一份账本。
	Budget *budget.Config `json:"budget,omitempty"`
	// Resources 是子工作持有的外部资源，结束时释放。
	Resources  []Resource `json:"resources,omitempty"`
	Quiet      bool       `json:"quiet,omitempty"`
	WaitUntil  time.Time  `json:"wait_until"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt time.Time  `json:"finished_at"`
}

// Resource 是子工作持有的外部资源，如托管沙箱实例。Data 是释放时需要的定位信息，不保存凭据。
type Resource struct {
	Kind string          `json:"kind"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Notice 通知所有者子工作已结束或等待续跑。投递至少一次，所有者按 ChildID 和 Generation 去重。
type Notice struct {
	Owner           string
	ChildID         string
	Generation      int
	Runner          string
	Status          Status
	Result          string
	ResultTruncated bool
	FailureKind     string
	Waits           []*durable.WaitError
}

// Key 是这条通知的稳定标识。宿主开启新运行时用它派生执行键，重投就会进入同一运行而不是再开一次。
func (n Notice) Key() string { return fmt.Sprintf("background/%s#%d", n.ChildID, n.Generation) }

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

	mu       sync.Mutex
	runners  map[string]Runner
	releases map[string]func(context.Context, Resource) error
	live     map[string]*liveChild
	runCtx   context.Context
	fail     context.CancelCauseFunc
	wg       sync.WaitGroup
	ready    chan struct{}
	opened   sync.Once
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
		runners: map[string]Runner{}, releases: map[string]func(context.Context, Resource) error{}, live: map[string]*liveChild{}, ready: make(chan struct{})}
	queueCfg := cfg.Effects
	queueCfg.Store, queueCfg.Name, queueCfg.Now = cfg.Store, "background/"+cfg.Name, cfg.Now
	queueCfg.Kinds = map[string]effects.Kind{notifyKind: {Run: m.deliver, Idempotent: true}, releaseKind: {Run: m.release, Reconcile: m.reconcileRelease}}
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

// RegisterResource 登记一类资源的释放方法，须幂等，无法确认资源是否存在时返回 ErrResourceUnknown。须在 Run 之前登记，否则上次遗留的释放要等到登记后才执行。
func (m *Manager) RegisterResource(kind string, release func(context.Context, Resource) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.releases[kind] = release
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
		// 恢复不了预算时不能无上限地运行：Run 失败，记录保持 running，下次启动再试。
		base, err := m.restoreBudget(context.WithoutCancel(ctx), record)
		if err != nil {
			return err
		}
		m.launch(base, record.Task, record.Quiet)
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
	limit, err := m.budgetOf(ctx)
	if err != nil {
		return Record{}, err
	}
	var result Record
	start := false
	err = m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
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
		result = Record{Task: task, Status: StatusRunning, Budget: limit, CreatedAt: m.cfg.Now()}
		state.Records = append(state.Records, result)
		start = true
		return nil, nil
	})
	if err != nil {
		return Record{}, err
	}
	if start {
		base, err := m.restoreBudget(context.WithoutCancel(ctx), result)
		if err != nil {
			return Record{}, err
		}
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

// budgetOf 取 ctx 中的根预算配置。重启后只能用 Manager 的 Store 连回账本，账本不在其中时上限会被重置，因此在提交时拒绝。
func (m *Manager) budgetOf(ctx context.Context) (*budget.Config, error) {
	controller, ok := budget.FromContext(ctx)
	if !ok {
		return nil, nil
	}
	if !controller.IsDurable() {
		return nil, errors.New("后台子工作需要持久根预算")
	}
	config := controller.Configuration()
	restored, err := budget.NewDurable(config, m.cfg.Store)
	if err != nil {
		return nil, err
	}
	current, err := controller.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	seen, err := restored.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if seen.ReservedRequests < current.ReservedRequests {
		return nil, errors.New("根预算须与 Manager 使用同一个 Store")
	}
	return &config, nil
}

func (m *Manager) restoreBudget(ctx context.Context, record Record) (context.Context, error) {
	if record.Budget == nil {
		return ctx, nil
	}
	controller, err := budget.NewDurable(*record.Budget, m.cfg.Store)
	if err != nil {
		return nil, err
	}
	return budget.WithController(ctx, controller), nil
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
	cause := context.Cause(ctx)
	// 终止时结果已经落盘。这里再写一次只会拖慢资源释放：释放要等这个 goroutine 退出。
	if errors.Is(cause, errKilled) {
		return
	}
	status, kind := StatusCompleted, ""
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if waits := durable.Waits(runErr); waits != nil && resumable(waits) && !errors.Is(cause, errParentCanceled) {
		// 写入失败时记录仍是 running，下次启动重新进入后回到同一等待。
		_ = m.park(saveCtx, task.ID, waits)
		return
	}
	if runErr != nil || errors.Is(cause, errParentCanceled) {
		var failure *effects.FailureError
		switch {
		case errors.Is(cause, errParentCanceled):
			status = StatusCanceled
		case errors.Is(runErr, errPanic):
			status, kind = StatusFailed, "panic"
		case errors.Is(runErr, durable.ErrOperationAttention):
			status, kind = StatusFailed, "attention"
		case durable.Waits(runErr) != nil:
			// 等待没有定位信息，宿主无法提供续跑输入。
			status, kind = StatusFailed, "waiting"
		case errors.As(runErr, &failure):
			status, kind = StatusFailed, failure.Kind
		default:
			status, kind = StatusFailed, "execution"
		}
		result = ""
	}
	// 写入失败时记录仍是 running，下次启动重新进入，已完成的持久步骤回放。
	_ = m.finish(saveCtx, task.ID, status, result, kind)
}

func resumable(waits []*durable.WaitError) bool {
	for _, wait := range waits {
		if len(wait.Path) == 0 {
			return false
		}
	}
	return len(waits) > 0
}

// park 把运行中的记录改为等待并通知所有者。已被终止时不变。
func (m *Manager) park(ctx context.Context, id string, waits []*durable.WaitError) error {
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		record := findRecord(state, id)
		if record == nil || record.Status != StatusRunning {
			return nil, nil
		}
		record.Status, record.Waits = StatusWaiting, waits
		return m.noticeOf(record)
	})
}

// Resume 用宿主提供的输入续跑等待中的子工作，键取 Record.Waits 中 Path 最后一项的 Key；未提供输入的等待会再次暂停。
// 输入是否被授权由宿主负责。输入不落盘：续跑后崩溃时子工作回到等待，已消费的批准不会被沿用。
func (m *Manager) Resume(ctx context.Context, id string, inputs map[string]string) error {
	if len(inputs) == 0 {
		return errors.New("Resume 需要续跑输入")
	}
	m.mu.Lock()
	running := m.runCtx != nil
	child := m.live[id]
	m.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	// 记录已是 waiting 时，上一次运行的 goroutine 可能还在退出；不等它结束，launch 会把它当作已在运行。
	if record, err := m.Get(ctx, id); child != nil && err == nil && record.Status == StatusWaiting {
		select {
		case <-child.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var resumed Record
	err := m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		record := findRecord(state, id)
		if record == nil {
			return nil, fmt.Errorf("子工作 %q: %w", id, os.ErrNotExist)
		}
		if record.Status != StatusWaiting {
			return nil, fmt.Errorf("子工作 %q 的状态是 %s: %w", id, record.Status, ErrNotWaiting)
		}
		for key := range inputs {
			if !slices.ContainsFunc(record.Waits, func(wait *durable.WaitError) bool { return wait.Path[len(wait.Path)-1].Key == key }) {
				return nil, fmt.Errorf("子工作 %q 没有等待键 %q", id, key)
			}
		}
		record.Status, record.Waits = StatusRunning, nil
		resumed = *record
		return nil, nil
	})
	if err != nil {
		return err
	}
	base, err := m.restoreBudget(context.WithoutCancel(ctx), resumed)
	if err != nil {
		return err
	}
	for key, input := range inputs {
		base = durable.WithResume(base, key, input)
	}
	m.launch(base, resumed.Task, resumed.Quiet)
	return nil
}

// Attach 把资源交给子工作：进程停止时保留，子工作结束时释放。相同 Kind 和 ID 的记录被更新，
// 所以可以在分配前先记下意图，分配后再补上实例。子工作已结束时报错，由调用方释放。
func (m *Manager) Attach(ctx context.Context, id string, resource Resource) error {
	m.mu.Lock()
	_, known := m.releases[resource.Kind]
	m.mu.Unlock()
	if !known {
		return fmt.Errorf("资源种类 %q 未注册", resource.Kind)
	}
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		record := findRecord(state, id)
		if record == nil {
			return nil, fmt.Errorf("子工作 %q: %w", id, os.ErrNotExist)
		}
		if record.Status.terminal() {
			return nil, fmt.Errorf("子工作 %q 已结束，不能再持有资源", id)
		}
		index := slices.IndexFunc(record.Resources, func(r Resource) bool { return r.Kind == resource.Kind && r.ID == resource.ID })
		if index >= 0 {
			record.Resources[index] = resource
		} else {
			record.Resources = append(record.Resources, resource)
		}
		return nil, nil
	})
}

// Detach 撤销子工作持有的资源记录，用于资源已确认不存在时。子工作不存在或已结束时不做任何事。
func (m *Manager) Detach(ctx context.Context, id, kind, resourceID string) error {
	return m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		record := findRecord(state, id)
		if record != nil && !record.Status.terminal() {
			record.Resources = slices.DeleteFunc(record.Resources, func(r Resource) bool { return r.Kind == kind && r.ID == resourceID })
		}
		return nil, nil
	})
}

// Problems 返回需要宿主处理的通知和资源释放：无法确认状态的，以及连续失败达到阈值的。
func (m *Manager) Problems(ctx context.Context) ([]effects.Record, error) {
	return m.queue.Problems(ctx)
}

// ResolveProblem 裁决无法确认状态的记录：applied 为真表示宿主已处理完，否则重新执行。
func (m *Manager) ResolveProblem(ctx context.Context, seq uint64, applied bool) error {
	return m.queue.Resolve(ctx, seq, applied)
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
	record.Status, record.Result, record.FailureKind, record.FinishedAt, record.WaitUntil, record.Waits = status, result, kind, m.cfg.Now(), time.Time{}, nil
	notices, err := m.noticeOf(record)
	if err != nil {
		return nil, err
	}
	for _, resource := range record.Resources {
		payload, err := json.Marshal(releasePayload{ChildID: record.ID, Resource: resource})
		if err != nil {
			return nil, err
		}
		notices = append(notices, effects.Effect{Kind: releaseKind, Key: record.ID + "/" + resource.Kind + "/" + resource.ID, Target: resource.Kind, Payload: payload})
	}
	return notices, nil
}

// noticeOf 为记录的当前状态生成通知。
func (m *Manager) noticeOf(record *Record) ([]effects.Effect, error) {
	if record.Mode != ModeBackground || record.Quiet {
		return nil, nil
	}
	record.Generation++
	payload, err := json.Marshal(noticePayload{ChildID: record.ID, Generation: record.Generation})
	if err != nil {
		return nil, err
	}
	return []effects.Effect{{Kind: notifyKind, Key: fmt.Sprintf("%s#%d", record.ID, record.Generation), Target: record.Owner, Payload: payload}}, nil
}

type noticePayload struct {
	ChildID    string `json:"child_id"`
	Generation int    `json:"generation"`
}

type releasePayload struct {
	ChildID  string   `json:"child_id"`
	Resource Resource `json:"resource"`
}

// deliver 按记录投递通知。结果不放进待办内容，内容大小不受结果影响。已被新状态取代的通知不再投递。
func (m *Manager) deliver(ctx context.Context, effect effects.Effect) error {
	var payload noticePayload
	if err := json.Unmarshal(effect.Payload, &payload); err != nil {
		return effects.Drop("corrupt_notice")
	}
	record, err := m.Get(ctx, payload.ChildID)
	if errors.Is(err, os.ErrNotExist) {
		return effects.Drop("record_removed")
	}
	if err != nil {
		return err
	}
	if record.Generation != payload.Generation {
		return effects.Drop("superseded")
	}
	return m.cfg.Notify(ctx, Notice{Owner: record.Owner, ChildID: record.ID, Generation: record.Generation, Runner: record.Runner, Status: record.Status,
		Result: record.Result, ResultTruncated: record.ResultTruncated, FailureKind: record.FailureKind, Waits: record.Waits})
}

// release 释放已结束子工作的资源。Runner 还在退出时稍后再试，资源不在使用中被销毁。
func (m *Manager) release(ctx context.Context, effect effects.Effect) error {
	var payload releasePayload
	if err := json.Unmarshal(effect.Payload, &payload); err != nil {
		return effects.Drop("corrupt_resource")
	}
	m.mu.Lock()
	release := m.releases[payload.Resource.Kind]
	_, live := m.live[payload.ChildID]
	m.mu.Unlock()
	if release == nil || live {
		return effects.ErrLater
	}
	err := release(ctx, payload.Resource)
	if errors.Is(err, ErrResourceUnknown) {
		return fmt.Errorf("%w: %w", effects.ErrUnconfirmed, err)
	}
	return err
}

// reconcileRelease 用再次释放来核对：释放是幂等的。仍无法确认时交给宿主。
func (m *Manager) reconcileRelease(ctx context.Context, effect effects.Effect) (bool, error) {
	err := m.release(ctx, effect)
	if errors.Is(err, effects.ErrUnconfirmed) {
		return false, err
	}
	return err == nil, nil
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
		if (record.Status == StatusRunning || record.Status == StatusWaiting) && !record.Quiet {
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
		if record.Status.terminal() || record.Status == StatusInterrupted || record.Status == StatusWaiting {
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
			if record.Owner == owner && (record.Status == StatusRunning || record.Status == StatusWaiting) && record.Mode == ModeBackground && !record.Quiet && record.WaitUntil.IsZero() {
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
		if (record.Status == StatusRunning || record.Status == StatusWaiting) && !record.WaitUntil.IsZero() && !record.WaitUntil.After(now) {
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
		var releases []effects.Effect
		for i := range state.Records {
			record := &state.Records[i]
			if record.Owner != owner || (record.Status != StatusRunning && record.Status != StatusWaiting) {
				continue
			}
			if record.SurviveHandoff {
				record.Quiet, record.WaitUntil = true, time.Time{}
				exempt = append(exempt, record.ID)
				continue
			}
			record.Quiet = true
			effects, err := m.settle(record, StatusStopped, "", "")
			if err != nil {
				return nil, err
			}
			releases = append(releases, effects...)
			stopped = append(stopped, record.ID)
		}
		return releases, nil
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
