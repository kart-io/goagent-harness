package background

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/stretchr/testify/require"
)

const crashExit = 91

type notices struct {
	mu   sync.Mutex
	list []Notice
}

func (n *notices) add(_ context.Context, notice Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, notice)
	return nil
}

func (n *notices) all() []Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]Notice(nil), n.list...)
}

func testConfig(store durable.Store, notify func(context.Context, Notice) error) Config {
	return Config{Store: store, Name: "m", Notify: notify, MaxWorkers: 4, MaxWait: time.Minute, Poll: 5 * time.Millisecond,
		Effects: effects.Config{Poll: 5 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, LaterDelay: 5 * time.Millisecond}}
}

func newStore(t *testing.T, dir string) *durable.FileStore {
	t.Helper()
	store, err := durable.NewFileStore(dir)
	require.NoError(t, err)
	return store
}

func newManager(t *testing.T, cfg Config, runner Runner) *Manager {
	t.Helper()
	m, err := New(cfg)
	require.NoError(t, err)
	m.Register("r", runner)
	return m
}

// runManager 启动 Manager，等它就绪，测试结束时停止。
func runManager(t *testing.T, m *Manager) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	require.NoError(t, m.Ready(context.Background()))
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Error("Manager 没有退出")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func bg(id string) Task {
	return Task{ID: id, Owner: "o", Runner: "r", Input: "task", Mode: ModeBackground}
}

func waitStatus(t *testing.T, m *Manager, id string, status Status) Record {
	t.Helper()
	var last Record
	require.Eventually(t, func() bool {
		record, err := m.Get(context.Background(), id)
		if err != nil {
			return false
		}
		last = record
		return record.Status == status
	}, 30*time.Second, 2*time.Millisecond, "等待 %s 进入 %s", id, status)
	return last
}

func TestBackgroundCompletesAndNotifiesOnce(t *testing.T) {
	var n notices
	runs := 0
	var mu sync.Mutex
	m := newManager(t, testConfig(newStore(t, t.TempDir()), n.add), func(ctx context.Context, task Task) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		runs++
		require.Equal(t, KeyOf("m", task.ID), durable.ExecutionKey(ctx), "执行键稳定")
		require.True(t, durable.HasJournal(ctx))
		return "answer:" + task.Input, nil
	})
	runManager(t, m)
	ctx := context.Background()
	record, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	require.Equal(t, StatusRunning, record.Status)
	done := waitStatus(t, m, "c1", StatusCompleted)
	require.Equal(t, "answer:task", done.Result)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, Notice{Owner: "o", ChildID: "c1", Generation: 1, Runner: "r", Status: StatusCompleted, Result: "answer:task"}, n.all()[0])

	// 相同 ID 再次提交返回同一结果，不重新执行。
	again, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, again.Status)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, runs)
}

func TestSpawnRejectsChangedTask(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		close(entered)
		<-release
		return "", ctx.Err()
	})
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	<-entered
	changed := bg("c1")
	changed.Input = "other"
	_, err = m.Spawn(ctx, changed)
	require.ErrorContains(t, err, "内容不同")
	_, err = m.Spawn(ctx, Task{ID: "x", Owner: "o", Runner: "missing", Mode: ModeBackground})
	require.ErrorContains(t, err, "未注册")
	_, err = m.Spawn(ctx, Task{Owner: "o", Runner: "r", Mode: ModeBackground})
	require.Error(t, err, "缺少 ID")
}

func TestSpawnRequiresRunningManager(t *testing.T) {
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(context.Context, Task) (string, error) { return "", nil })
	_, err := m.Spawn(context.Background(), bg("c1"))
	require.ErrorIs(t, err, ErrNotRunning)
}

func runChild(t *testing.T, name, env string) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), env+"="+dir)
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited, string(output))
	require.Equal(t, crashExit, exited.ExitCode(), string(output))
	return dir
}

// 执行中进程退出：重启找回同一 child 和执行键，已完成的持久步骤不重复。
func TestBackgroundResumesAfterProcessExit(t *testing.T) {
	const env = "GOAGENT_BACKGROUND_RESUME_DIR"
	step := func(ctx context.Context, id string, run func()) error {
		_, err := durable.Do(ctx, id, json.RawMessage(`"v1"`), durable.OperationPolicy{}, func(context.Context) (json.RawMessage, error) {
			run()
			return json.RawMessage(`null`), nil
		})
		return err
	}
	if dir := os.Getenv(env); dir != "" {
		m := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
			if err := step(ctx, "first", func() {}); err != nil {
				return "", err
			}
			os.Exit(crashExit)
			return "", nil
		})
		runManager(t, m)
		_, err := m.Spawn(context.Background(), bg("c1"))
		require.NoError(t, err)
		select {}
	}
	dir := runChild(t, "TestBackgroundResumesAfterProcessExit", env)
	var n notices
	first, second := 0, 0
	m := newManager(t, testConfig(newStore(t, dir), n.add), func(ctx context.Context, _ Task) (string, error) {
		if err := step(ctx, "first", func() { first++ }); err != nil {
			return "", err
		}
		if err := step(ctx, "second", func() { second++ }); err != nil {
			return "", err
		}
		return "resumed", nil
	})
	runManager(t, m)
	require.Equal(t, "resumed", waitStatus(t, m, "c1", StatusCompleted).Result)
	require.Zero(t, first, "已完成的步骤从日志回放")
	require.Equal(t, 1, second)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
}

// 结果已记录、通知未送达时进程退出：重启后只补发通知，不重新执行业务。
func TestNoticeRedeliveredWithoutRerunAfterProcessExit(t *testing.T) {
	const env = "GOAGENT_BACKGROUND_NOTICE_DIR"
	if dir := os.Getenv(env); dir != "" {
		m := newManager(t, testConfig(newStore(t, dir), func(context.Context, Notice) error {
			os.Exit(crashExit)
			return nil
		}), func(context.Context, Task) (string, error) { return "done", nil })
		runManager(t, m)
		_, err := m.Spawn(context.Background(), bg("c1"))
		require.NoError(t, err)
		select {}
	}
	dir := runChild(t, "TestNoticeRedeliveredWithoutRerunAfterProcessExit", env)
	var n notices
	runs := 0
	m := newManager(t, testConfig(newStore(t, dir), n.add), func(context.Context, Task) (string, error) {
		runs++
		return "again", nil
	})
	runManager(t, m)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, "done", n.all()[0].Result)
	require.Zero(t, runs)
}

func TestForegroundFollowsParentCancellation(t *testing.T) {
	store := newStore(t, t.TempDir())
	entered := make(chan struct{})
	m := newManager(t, testConfig(store, (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	runManager(t, m)
	parent, cancel := context.WithCancel(context.Background())
	task := bg("f1")
	task.Mode = ModeForeground
	_, err := m.Spawn(parent, task)
	require.NoError(t, err)
	<-entered
	cancel()
	record, err := m.Wait(context.Background(), "f1")
	require.NoError(t, err)
	require.Equal(t, StatusCanceled, record.Status)
}

// 进程停止时前台任务记为中断：父调用恢复后用同一 ID 重新进入，执行键不变；进入之前不算忙碌。
func TestForegroundInterruptedByShutdownCanBeRespawned(t *testing.T) {
	store := newStore(t, t.TempDir())
	entered := make(chan struct{})
	m := newManager(t, testConfig(store, (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	stop := runManager(t, m)
	task := bg("f1")
	task.Mode = ModeForeground
	_, err := m.Spawn(context.Background(), task)
	require.NoError(t, err)
	<-entered
	require.NoError(t, stop())

	restarted := newManager(t, testConfig(store, (&notices{}).add), func(context.Context, Task) (string, error) { return "finished", nil })
	runManager(t, restarted)
	record, err := restarted.Get(context.Background(), "f1")
	require.NoError(t, err)
	require.Equal(t, StatusInterrupted, record.Status)
	busy, err := restarted.Busy(context.Background(), "o")
	require.NoError(t, err)
	require.False(t, busy)
	_, err = restarted.Spawn(context.Background(), task)
	require.NoError(t, err)
	record, err = restarted.Wait(context.Background(), "f1")
	require.NoError(t, err)
	require.Equal(t, StatusCompleted, record.Status)
	require.Equal(t, "finished", record.Result)
}

func blocking(entered chan<- struct{}) Runner {
	return func(ctx context.Context, _ Task) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
}

func TestWaitLimitKillsAndReports(t *testing.T) {
	var n notices
	entered := make(chan struct{}, 1)
	cfg := testConfig(newStore(t, t.TempDir()), n.add)
	cfg.MaxWait = 30 * time.Millisecond
	m := newManager(t, cfg, blocking(entered))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	<-entered
	busy, err := m.Busy(ctx, "o")
	require.NoError(t, err)
	require.True(t, busy, "有后台工作时不算空闲")
	time.Sleep(60 * time.Millisecond)
	require.Equal(t, StatusRunning, mustGet(t, m, "c1").Status, "轮次未结束时不计时")

	require.NoError(t, m.TurnEnded(ctx, "o"))
	waitStatus(t, m, "c1", StatusKilled)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, StatusKilled, n.all()[0].Status)
	busy, err = m.Busy(ctx, "o")
	require.NoError(t, err)
	require.False(t, busy)
}

func mustGet(t *testing.T, m *Manager, id string) Record {
	t.Helper()
	record, err := m.Get(context.Background(), id)
	require.NoError(t, err)
	return record
}

func TestTurnStartedClearsWaitLimit(t *testing.T) {
	entered := make(chan struct{}, 1)
	cfg := testConfig(newStore(t, t.TempDir()), (&notices{}).add)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	cfg.Now = func() time.Time { return time.Unix(0, clock.Load()) }
	cfg.MaxWait = time.Minute
	m := newManager(t, cfg, blocking(entered))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	<-entered
	require.NoError(t, m.TurnEnded(ctx, "o"))
	require.False(t, mustGet(t, m, "c1").WaitUntil.IsZero())
	require.NoError(t, m.TurnStarted(ctx, "o"))
	require.True(t, mustGet(t, m, "c1").WaitUntil.IsZero())
	clock.Add(int64(2 * time.Minute))
	require.NoError(t, m.expire(ctx))
	require.Equal(t, StatusRunning, mustGet(t, m, "c1").Status)
}

// 等待期限持久化：重启后不会重新计时。
func TestWaitLimitSurvivesRestart(t *testing.T) {
	store := newStore(t, t.TempDir())
	entered := make(chan struct{}, 2)
	cfg := testConfig(store, (&notices{}).add)
	cfg.MaxWait = 50 * time.Millisecond
	m := newManager(t, cfg, blocking(entered))
	stop := runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	<-entered
	require.NoError(t, m.TurnEnded(ctx, "o"))
	require.NoError(t, stop())
	require.Equal(t, StatusRunning, mustGet(t, m, "c1").Status)

	cfg.MaxWait = time.Hour
	restarted := newManager(t, cfg, blocking(entered))
	runManager(t, restarted)
	waitStatus(t, restarted, "c1", StatusKilled)
}

// 完成与终止竞争时以先落盘的为准：已完成的任务不会再被记为终止，只通知一次。
func TestFirstPersistedOutcomeWins(t *testing.T) {
	var n notices
	finish := make(chan struct{})
	entered := make(chan struct{})
	m := newManager(t, testConfig(newStore(t, t.TempDir()), n.add), func(context.Context, Task) (string, error) {
		close(entered)
		<-finish
		return "done", nil
	})
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("c1"))
	require.NoError(t, err)
	<-entered
	require.NoError(t, m.Kill(ctx, "c1"))
	close(finish)
	record, err := m.Wait(ctx, "c1")
	require.NoError(t, err)
	require.Equal(t, StatusKilled, record.Status)
	require.NoError(t, m.Kill(ctx, "c1"), "重复终止被忽略")
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	require.Len(t, n.all(), 1)
	require.Empty(t, mustGet(t, m, "c1").Result)
}

func TestHandoffStopsChildrenAndKeepsExemptJobsQuiet(t *testing.T) {
	var n notices
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	cfg := testConfig(newStore(t, t.TempDir()), n.add)
	cfg.MaxWorkers = 2
	cfg.MaxWait = 10 * time.Millisecond
	m, err := New(cfg)
	require.NoError(t, err)
	m.Register("r", blocking(entered))
	m.Register("job", func(context.Context, Task) (string, error) {
		entered <- struct{}{}
		<-release
		return "job result", nil
	})
	runManager(t, m)
	ctx := context.Background()
	_, err = m.Spawn(ctx, bg("child"))
	require.NoError(t, err)
	_, err = m.Spawn(ctx, Task{ID: "job", Owner: "o", Runner: "job", Input: "serve", Mode: ModeBackground, SurviveHandoff: true})
	require.NoError(t, err)
	<-entered
	<-entered
	require.NoError(t, m.Handoff(ctx, "o"))
	waitStatus(t, m, "child", StatusStopped)
	require.Equal(t, 0, m.Slots().Used(), "交接后立即释放槽位")
	require.NoError(t, m.TurnEnded(ctx, "o"))
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, StatusRunning, mustGet(t, m, "job").Status, "豁免的工作不受等待上限影响")
	close(release)
	require.Equal(t, "job result", waitStatus(t, m, "job", StatusCompleted).Result)
	time.Sleep(30 * time.Millisecond)
	require.Empty(t, n.all(), "交接后的完成不唤醒所有者，下一轮读取结果")
	busy, err := m.Busy(ctx, "o")
	require.NoError(t, err)
	require.False(t, busy)
}

func TestNoticeRetriesUntilDelivered(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	var n notices
	m := newManager(t, testConfig(newStore(t, t.TempDir()), func(ctx context.Context, notice Notice) error {
		mu.Lock()
		attempts++
		current := attempts
		mu.Unlock()
		if current < 3 {
			return effects.ErrLater
		}
		return n.add(ctx, notice)
	}), func(context.Context, Task) (string, error) { return "done", nil })
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("c1"))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
}

func TestRunnerFailureIsCategorized(t *testing.T) {
	var n notices
	secret := errors.New("token=abc")
	cases := map[string]struct {
		run  Runner
		kind string
	}{
		"fail":  {func(context.Context, Task) (string, error) { return "", secret }, "execution"},
		"panic": {func(context.Context, Task) (string, error) { panic("boom") }, "panic"},
		"attention": {func(context.Context, Task) (string, error) {
			return "", &durable.OperationError{ID: "x", Kind: "ambiguous"}
		}, "attention"},
		"waiting": {func(context.Context, Task) (string, error) { return "", &durable.WaitError{} }, "waiting"},
	}
	m, err := New(testConfig(newStore(t, t.TempDir()), n.add))
	require.NoError(t, err)
	for name, tc := range cases {
		m.Register(name, tc.run)
	}
	runManager(t, m)
	ctx := context.Background()
	for name, tc := range cases {
		_, err := m.Spawn(ctx, Task{ID: name, Owner: "o", Runner: name, Mode: ModeBackground})
		require.NoError(t, err)
		record := waitStatus(t, m, name, StatusFailed)
		require.Equal(t, tc.kind, record.FailureKind, name)
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "abc", "不保存错误文本")
	}
}

type leaseLost struct {
	durable.Store
	lost func() bool
}

func (s leaseLost) SaveAll(ctx context.Context, writes ...durable.Write) error {
	if s.lost() {
		return agentErrors.ErrLeaseLost
	}
	return s.Store.SaveAll(ctx, writes...)
}

// 失去控制权：停止全部子工作并返回错误，旧宿主不继续。
func TestLeaseLostStopsManagerAndChildren(t *testing.T) {
	var lost sync.Mutex
	isLost := false
	store := leaseLost{Store: newStore(t, t.TempDir()), lost: func() bool {
		lost.Lock()
		defer lost.Unlock()
		return isLost
	}}
	entered := make(chan struct{}, 1)
	stopped := make(chan struct{})
	m := newManager(t, testConfig(store, (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		entered <- struct{}{}
		<-ctx.Done()
		close(stopped)
		return "", ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- m.Run(context.Background()) }()
	require.NoError(t, m.Ready(context.Background()))
	_, err := m.Spawn(context.Background(), bg("c1"))
	require.NoError(t, err)
	<-entered
	lost.Lock()
	isLost = true
	lost.Unlock()
	require.ErrorIs(t, m.TurnEnded(context.Background(), "o"), agentErrors.ErrLeaseLost)
	select {
	case err := <-done:
		require.ErrorIs(t, err, agentErrors.ErrLeaseLost)
	case <-time.After(5 * time.Second):
		t.Fatal("Manager 没有停止")
	}
	<-stopped
}

func TestLongResultIsTruncatedOnRuneBoundary(t *testing.T) {
	var n notices
	cfg := testConfig(newStore(t, t.TempDir()), n.add)
	cfg.MaxResult = 4
	m := newManager(t, cfg, func(context.Context, Task) (string, error) { return "中文内容", nil })
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("c1"))
	require.NoError(t, err)
	record := waitStatus(t, m, "c1", StatusCompleted)
	require.Equal(t, "中", record.Result)
	require.True(t, record.ResultTruncated)
}

func TestFinishedRecordsAreBounded(t *testing.T) {
	cfg := testConfig(newStore(t, t.TempDir()), (&notices{}).add)
	cfg.RetainFinished = 2
	m := newManager(t, cfg, func(context.Context, Task) (string, error) { return "done", nil })
	runManager(t, m)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		_, err := m.Spawn(ctx, bg(id))
		require.NoError(t, err)
		waitStatus(t, m, id, StatusCompleted)
	}
	records, err := m.Records(ctx, "o")
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "d", records[1].ID)
}

func TestSlotsReserveCapacityForPeople(t *testing.T) {
	ctx := context.Background()
	slots := NewSlots(1)
	release, err := slots.Acquire(ctx, PriorityLaunch)
	require.NoError(t, err)

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = slots.Acquire(short, PriorityWake)
	require.ErrorIs(t, err, context.DeadlineExceeded, "自动输入不能用保留槽位")

	immediate, cancelImmediate := context.WithTimeout(ctx, time.Second)
	defer cancelImmediate()
	human, err := slots.Acquire(immediate, PriorityHuman)
	require.NoError(t, err, "普通槽位占满时，人的输入可以立即进入")
	require.Equal(t, 2, slots.Used())

	// 保留槽位也被占用时，人的输入排在其他等待者之前，不抢占进行中的轮次。
	order := make(chan string, 3)
	go func() {
		r, err := slots.Acquire(ctx, PriorityLaunch)
		if err == nil {
			order <- "launch"
			r()
		}
	}()
	time.Sleep(10 * time.Millisecond)
	go func() {
		r, err := slots.Acquire(ctx, PriorityWake)
		if err == nil {
			order <- "wake"
			r()
		}
	}()
	time.Sleep(10 * time.Millisecond)
	go func() {
		r, err := slots.Acquire(ctx, PriorityHuman)
		if err == nil {
			order <- "human"
			r()
		}
	}()
	time.Sleep(10 * time.Millisecond)
	human()
	require.Equal(t, "human", <-order)
	release()
	require.Equal(t, "wake", <-order)
	require.Equal(t, "launch", <-order)
	release()
	require.Equal(t, 0, slots.Used(), "重复释放被忽略")
}

// Pause expiry immediately before its state transaction. A turn can clear the
// deadline after the initial scan; expiry must recheck the committed record.
type expiryLockKey struct{}
type pausedExpiryStore struct {
	durable.Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *pausedExpiryStore) Lock(ctx context.Context, id string) (func() error, error) {
	if ctx.Value(expiryLockKey{}) != nil {
		s.once.Do(func() { close(s.entered); <-s.release })
	}
	return s.Store.Lock(ctx, id)
}

func TestExpireRechecksDeadlineAfterTurnStarted(t *testing.T) {
	ctx := context.Background()
	store := &pausedExpiryStore{Store: newStore(t, t.TempDir()), entered: make(chan struct{}), release: make(chan struct{})}
	m := newManager(t, testConfig(store, (&notices{}).add), nil)
	require.NoError(t, m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		state.Records = append(state.Records, Record{Task: bg("c1"), Status: StatusRunning, WaitUntil: time.Now().Add(-time.Hour)})
		return nil, nil
	}))
	done := make(chan error, 1)
	go func() { done <- m.expire(context.WithValue(ctx, expiryLockKey{}, true)) }()
	<-store.entered
	require.NoError(t, m.TurnStarted(ctx, "o"))
	close(store.release)
	require.NoError(t, <-done)
	record := mustGet(t, m, "c1")
	require.Equal(t, StatusRunning, record.Status)
	require.True(t, record.WaitUntil.IsZero())
	records, err := m.queue.Records(ctx)
	require.NoError(t, err)
	require.Empty(t, records, "cleared deadlines must not emit termination notices")
}

// Waiting children retain their deadline while parked for a host response.
func TestExpireKillsWaitingChild(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), nil)
	require.NoError(t, m.update(ctx, func(state *managerState) ([]effects.Effect, error) {
		state.Records = append(state.Records, Record{Task: bg("c1"), Status: StatusWaiting, WaitUntil: time.Now().Add(-time.Hour)})
		return nil, nil
	}))
	require.NoError(t, m.expire(ctx))
	record := mustGet(t, m, "c1")
	require.Equal(t, StatusKilled, record.Status)
	require.True(t, record.WaitUntil.IsZero())
	records, err := m.queue.Records(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1, "expired waiting children must notify their owner")
}
