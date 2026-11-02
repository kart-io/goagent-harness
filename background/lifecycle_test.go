package background

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/stretchr/testify/require"
)

const waitKey = "wait-key"

func waitErr() error {
	return &durable.WaitError{Path: []durable.WaitStep{{ID: "approval", Key: waitKey}}}
}

// waitingRunner 没有续跑输入时暂停；resumed 记录每次收到的输入。子工作的执行键与等待键不同，所以按等待键取输入。
func waitingRunner(resumed *atomic.Value) Runner {
	return func(ctx context.Context, _ Task) (string, error) {
		input, ok := durable.ResumeInput(durable.WithExecutionKey(ctx, waitKey))
		if !ok {
			return "", waitErr()
		}
		resumed.Store(input)
		return "批准后完成", nil
	}
}

func TestWaitParksThenResumesWithInput(t *testing.T) {
	var n notices
	var resumed atomic.Value
	m := newManager(t, testConfig(newStore(t, t.TempDir()), n.add), waitingRunner(&resumed))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)

	record := waitStatus(t, m, "a", StatusWaiting)
	require.Equal(t, waitKey, record.Waits[0].Path[0].Key)
	require.Empty(t, record.FailureKind)
	require.Eventually(t, func() bool { return len(n.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	waiting := n.all()[0]
	require.Equal(t, StatusWaiting, waiting.Status)
	require.Equal(t, waitKey, waiting.Waits[0].Path[0].Key)

	busy, err := m.Busy(ctx, "o")
	require.NoError(t, err)
	require.True(t, busy, "等待审批的工作还没有结果，所有者不算空闲")
	waited, err := m.Wait(ctx, "a")
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, waited.Status, "Wait 不能无限等待需要宿主续跑的工作")
	require.Eventually(t, func() bool { return m.Slots().Used() == 0 }, 30*time.Second, 2*time.Millisecond, "等待期间不占槽位")

	require.NoError(t, m.Resume(ctx, "a", map[string]string{waitKey: "approved"}))
	record = waitStatus(t, m, "a", StatusCompleted)
	require.Equal(t, "批准后完成", record.Result)
	require.Empty(t, record.Waits)
	require.Equal(t, "approved", resumed.Load())

	// 两次通知的序号不同：所有者不能把结束通知当作等待通知的重复而丢弃。
	require.Eventually(t, func() bool { return len(n.all()) == 2 }, 30*time.Second, 2*time.Millisecond)
	final := n.all()[1]
	require.Equal(t, StatusCompleted, final.Status)
	require.NotEqual(t, waiting.Generation, final.Generation)
}

func TestWaitingIsNotRerunAfterRestartAndInputIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	var runs atomic.Int32
	var resumed atomic.Value
	inner := waitingRunner(&resumed)
	runner := func(ctx context.Context, task Task) (string, error) {
		runs.Add(1)
		return inner(ctx, task)
	}
	first := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), runner)
	stop := runManager(t, first)
	_, err := first.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	waitStatus(t, first, "a", StatusWaiting)
	require.NoError(t, stop())
	require.EqualValues(t, 1, runs.Load())

	var n notices
	second := newManager(t, testConfig(newStore(t, dir), n.add), runner)
	runManager(t, second)
	time.Sleep(30 * time.Millisecond)
	require.EqualValues(t, 1, runs.Load(), "等待中的工作重启后不自动重跑")
	require.Equal(t, StatusWaiting, waitStatus(t, second, "a", StatusWaiting).Status)

	secret := "approval-secret-" + strings.Repeat("x", 8)
	require.NoError(t, second.Resume(context.Background(), "a", map[string]string{waitKey: secret}))
	waitStatus(t, second, "a", StatusCompleted)
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NotContains(t, string(data), secret, "续跑输入是一次性的，不能落盘：%s", path)
		return nil
	}))
}

// 续跑后崩溃：输入没有保存，子工作重新进入时回到等待，不会沿用已消费的批准。
func TestCrashAfterResumeWaitsAgain(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	first := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		if _, ok := durable.ResumeInput(durable.WithExecutionKey(ctx, waitKey)); !ok {
			return "", waitErr()
		}
		entered <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	stop := runManager(t, first)
	_, err := first.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	waitStatus(t, first, "a", StatusWaiting)
	require.NoError(t, first.Resume(context.Background(), "a", map[string]string{waitKey: "approved"}))
	<-entered
	require.NoError(t, stop())
	close(release)

	var resumed atomic.Value
	second := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), waitingRunner(&resumed))
	runManager(t, second)
	waitStatus(t, second, "a", StatusWaiting)
	require.Nil(t, resumed.Load())
}

func TestResumeValidatesStateAndKeys(t *testing.T) {
	var resumed atomic.Value
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), waitingRunner(&resumed))
	runManager(t, m)
	ctx := context.Background()
	require.ErrorIs(t, m.Resume(ctx, "missing", map[string]string{waitKey: "x"}), os.ErrNotExist)
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusWaiting)
	require.ErrorContains(t, m.Resume(ctx, "a", map[string]string{"forged": "x"}), "forged")
	require.ErrorContains(t, m.Resume(ctx, "a", nil), "续跑输入")
	require.Equal(t, StatusWaiting, waitStatus(t, m, "a", StatusWaiting).Status)
	require.NoError(t, m.Resume(ctx, "a", map[string]string{waitKey: "x"}))
	waitStatus(t, m, "a", StatusCompleted)
	require.ErrorIs(t, m.Resume(ctx, "a", map[string]string{waitKey: "x"}), ErrNotWaiting)
}

// 没有定位信息的等待无法续跑，保持原来的失败分类。
func TestWaitWithoutPathFails(t *testing.T) {
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(context.Context, Task) (string, error) {
		return "", &durable.WaitError{}
	})
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	require.Equal(t, "waiting", waitStatus(t, m, "a", StatusFailed).FailureKind)
}

func TestWaitingExpiresAndHandsOff(t *testing.T) {
	now := time.Now()
	var clock sync.Mutex
	cfg := testConfig(newStore(t, t.TempDir()), (&notices{}).add)
	cfg.Now = func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	var resumed atomic.Value
	m := newManager(t, cfg, waitingRunner(&resumed))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusWaiting)
	require.NoError(t, m.TurnEnded(ctx, "o"))
	clock.Lock()
	now = now.Add(2 * time.Minute)
	clock.Unlock()
	waitStatus(t, m, "a", StatusKilled)

	require.NoError(t, m.TurnStarted(ctx, "o"))
	_, err = m.Spawn(ctx, bg("b"))
	require.NoError(t, err)
	waitStatus(t, m, "b", StatusWaiting)
	require.NoError(t, m.Handoff(ctx, "o"))
	waitStatus(t, m, "b", StatusStopped)
}

func durableBudget(t *testing.T, store durable.Store) *budget.Controller {
	t.Helper()
	controller, err := budget.NewDurable(budget.Config{RootKey: "root", Version: "1", MaxModelRequests: 2}, store)
	require.NoError(t, err)
	return controller
}

// 重启后没有父调用的 ctx，上限不能因此消失：预算从记录恢复，并继续计入同一份账本。
func TestBudgetSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := newStore(t, dir)
	entered := make(chan struct{})
	first := newManager(t, testConfig(store, (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		controller, ok := budget.FromContext(ctx)
		require.True(t, ok)
		require.NoError(t, controller.Reserve(ctx, "first"))
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	stop := runManager(t, first)
	_, err := first.Spawn(budget.WithController(context.Background(), durableBudget(t, store)), bg("a"))
	require.NoError(t, err)
	<-entered
	require.NoError(t, stop())

	var remaining atomic.Int64
	remaining.Store(-1)
	var reserveErr atomic.Value
	second := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, _ Task) (string, error) {
		controller, ok := budget.FromContext(ctx)
		if !ok {
			return "", errors.New("重启后丢失预算")
		}
		left, err := controller.Remaining(ctx)
		if err != nil {
			return "", err
		}
		remaining.Store(int64(left))
		if err := controller.Reserve(ctx, "second"); err != nil {
			return "", err
		}
		if err := controller.Reserve(ctx, "third"); err != nil {
			reserveErr.Store(err)
		}
		return "done", nil
	})
	runManager(t, second)
	waitStatus(t, second, "a", StatusCompleted)
	require.EqualValues(t, 1, remaining.Load(), "第一次运行的消耗仍然计入")
	require.NotNil(t, reserveErr.Load(), "超过上限时被拒绝")
}

func TestSpawnRejectsBudgetThatCannotBeRestored(t *testing.T) {
	store := newStore(t, t.TempDir())
	m := newManager(t, testConfig(store, (&notices{}).add), func(context.Context, Task) (string, error) { return "x", nil })
	runManager(t, m)

	memory, err := budget.New(budget.Config{RootKey: "root", Version: "1", MaxModelRequests: 2})
	require.NoError(t, err)
	_, err = m.Spawn(budget.WithController(context.Background(), memory), bg("a"))
	require.ErrorContains(t, err, "持久根预算")

	// 账本在另一个 Store 时，重启后会得到一份空账本，上限等于被重置。
	other := durableBudget(t, newStore(t, t.TempDir()))
	require.NoError(t, other.Reserve(context.Background(), "spent"))
	_, err = m.Spawn(budget.WithController(context.Background(), other), bg("b"))
	require.ErrorContains(t, err, "同一个 Store")
}

type releaseLog struct {
	mu   sync.Mutex
	list []Resource
	err  error
}

func (r *releaseLog) release(_ context.Context, resource Resource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.list = append(r.list, resource)
	return nil
}

func (r *releaseLog) all() []Resource {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Resource(nil), r.list...)
}

func (r *releaseLog) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func TestResourceSurvivesRestartAndIsReleasedOnceAfterFinish(t *testing.T) {
	dir := t.TempDir()
	var released releaseLog
	entered := make(chan struct{})
	var first *Manager
	first = newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		require.NoError(t, first.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"}))
		require.NoError(t, first.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"}), "重复登记同一资源")
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	first.RegisterResource("box", released.release)
	stop := runManager(t, first)
	_, err := first.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	<-entered
	require.NoError(t, stop())
	require.Empty(t, released.all(), "进程停止不是子工作结束，不释放")

	var second *Manager
	var seen atomic.Value
	second = newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		record, err := second.Get(ctx, task.ID)
		if err != nil {
			return "", err
		}
		seen.Store(record.Resources)
		return "done", nil
	})
	second.RegisterResource("box", released.release)
	runManager(t, second)
	waitStatus(t, second, "a", StatusCompleted)
	require.Equal(t, []Resource{{Kind: "box", ID: "box-1"}}, seen.Load())
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, Resource{Kind: "box", ID: "box-1"}, released.all()[0])
}

// 终止后 Runner 可能还在退出：资源要等它结束才释放，不能在使用中被销毁。
func TestResourceReleasedOnlyAfterRunnerExits(t *testing.T) {
	var released releaseLog
	attached := make(chan struct{})
	exit := make(chan struct{})
	var m *Manager
	m = newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"}))
		close(attached)
		<-ctx.Done()
		<-exit
		return "", ctx.Err()
	})
	m.RegisterResource("box", released.release)
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	<-attached
	require.NoError(t, m.Kill(context.Background(), "a"))
	time.Sleep(30 * time.Millisecond)
	require.Empty(t, released.all())
	close(exit)
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
}

func TestHandoffReleasesStoppedChildResources(t *testing.T) {
	var released releaseLog
	attached := make(chan struct{})
	var m *Manager
	m = newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: task.ID}))
		attached <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	m.RegisterResource("box", released.release)
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("stop"))
	require.NoError(t, err)
	<-attached
	exempt := bg("keep")
	exempt.SurviveHandoff = true
	_, err = m.Spawn(ctx, exempt)
	require.NoError(t, err)
	<-attached
	require.NoError(t, m.Handoff(ctx, "o"))
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, "stop", released.all()[0].ID, "继续运行的工作保留资源")
}

func TestAttachAfterFinishFails(t *testing.T) {
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(context.Context, Task) (string, error) { return "x", nil })
	m.RegisterResource("box", (&releaseLog{}).release)
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusCompleted)
	require.ErrorContains(t, m.Attach(ctx, "a", Resource{Kind: "box", ID: "late"}), "已结束")
	require.ErrorContains(t, m.Attach(ctx, "a", Resource{Kind: "unknown", ID: "late"}), "未注册")
}

func TestReleaseFailureRetriesWithoutRerunningChild(t *testing.T) {
	var released releaseLog
	released.fail(errors.New("provider unavailable"))
	var runs atomic.Int32
	var m *Manager
	m = newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		runs.Add(1)
		return "x", m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"})
	})
	m.RegisterResource("box", released.release)
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusCompleted)
	time.Sleep(20 * time.Millisecond)
	released.fail(nil)
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.EqualValues(t, 1, runs.Load())
}

// 失去控制权的旧宿主不能销毁资源：新持有者还在使用它。
func TestLeaseLostDoesNotReleaseResources(t *testing.T) {
	dir := t.TempDir()
	var lost atomic.Bool
	store := leaseLost{Store: newStore(t, dir), lost: lost.Load}
	var released releaseLog
	attached := make(chan struct{})
	exit := make(chan struct{})
	var m *Manager
	m = newManager(t, testConfig(store, (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"}))
		close(attached)
		<-exit
		return "结果", nil
	})
	m.RegisterResource("box", released.release)
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	<-attached
	lost.Store(true)
	close(exit)
	time.Sleep(50 * time.Millisecond)
	require.Empty(t, released.all())
}

// 进程退出后待释放的资源由下次启动继续释放。
func TestPendingReleaseSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	var failing releaseLog
	failing.fail(effects.ErrLater)
	var first *Manager
	first = newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		return "x", first.Attach(ctx, task.ID, Resource{Kind: "box", ID: "box-1"})
	})
	first.RegisterResource("box", failing.release)
	stop := runManager(t, first)
	_, err := first.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	waitStatus(t, first, "a", StatusCompleted)
	require.NoError(t, stop())

	var released releaseLog
	second := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), func(context.Context, Task) (string, error) { return "", nil })
	second.RegisterResource("box", released.release)
	runManager(t, second)
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
}

func TestFailureErrorKindIsKept(t *testing.T) {
	m := newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(context.Context, Task) (string, error) {
		return "", &effects.FailureError{Kind: "manifest_changed", Err: errors.New("secret detail")}
	})
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	require.Equal(t, "manifest_changed", waitStatus(t, m, "a", StatusFailed).FailureKind)
}

// 宿主在通知回调里立刻续跑：上一次运行的 goroutine 可能还没退出，新的运行不能因此丢失。
func TestResumeImmediatelyFromNotice(t *testing.T) {
	for range 30 {
		var resumed atomic.Value
		var m *Manager
		notify := func(ctx context.Context, notice Notice) error {
			if notice.Status == StatusWaiting {
				return m.Resume(ctx, notice.ChildID, map[string]string{waitKey: "approved"})
			}
			return nil
		}
		m = newManager(t, testConfig(newStore(t, t.TempDir()), notify), waitingRunner(&resumed))
		stop := runManager(t, m)
		_, err := m.Spawn(context.Background(), bg("a"))
		require.NoError(t, err)
		waitStatus(t, m, "a", StatusCompleted)
		require.NoError(t, stop())
	}
}

func TestResumeRequiresRunningManager(t *testing.T) {
	dir := t.TempDir()
	var resumed atomic.Value
	m := newManager(t, testConfig(newStore(t, dir), (&notices{}).add), waitingRunner(&resumed))
	stop := runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusWaiting)
	require.NoError(t, stop())
	require.ErrorIs(t, m.Resume(context.Background(), "a", map[string]string{waitKey: "x"}), ErrNotRunning, "输入不能被静默丢弃")
	record, err := m.Get(context.Background(), "a")
	require.NoError(t, err)
	require.Equal(t, StatusWaiting, record.Status)
}

// 等待通知还没送达就被终止：只投递最新状态，不重复投递同一状态。
func TestStaleNoticeIsDropped(t *testing.T) {
	var n notices
	var open atomic.Bool
	notify := func(ctx context.Context, notice Notice) error {
		if !open.Load() {
			return effects.ErrLater
		}
		return n.add(ctx, notice)
	}
	var resumed atomic.Value
	m := newManager(t, testConfig(newStore(t, t.TempDir()), notify), waitingRunner(&resumed))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusWaiting)
	require.NoError(t, m.Kill(ctx, "a"))
	open.Store(true)
	require.Eventually(t, func() bool { return len(n.all()) >= 1 }, 30*time.Second, 2*time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	require.Len(t, n.all(), 1)
	require.Equal(t, StatusKilled, n.all()[0].Status)
}

// gatedUnlock 在 ID 为 docID 的记录写入 waiting 后，让那次写入的解锁停在 gate 上：记录已提交、锁已释放，但写入方还没返回。
type gatedUnlock struct {
	durable.Store
	docID   string
	pending atomic.Bool
	reached chan struct{}
	gate    chan struct{}
}

func (s *gatedUnlock) SaveAll(ctx context.Context, writes ...durable.Write) error {
	err := s.Store.SaveAll(ctx, writes...)
	for _, write := range writes {
		if write.ID == s.docID && write.Snapshot != nil && strings.Contains(string(write.Snapshot.Data), `"status":"waiting"`) {
			s.pending.Store(true)
		}
	}
	return err
}

func (s *gatedUnlock) Lock(ctx context.Context, id string) (func() error, error) {
	unlock, err := s.Store.Lock(ctx, id)
	if err != nil {
		return nil, err
	}
	return func() error {
		err := unlock()
		if id == s.docID && s.pending.CompareAndSwap(true, false) {
			close(s.reached)
			<-s.gate
		}
		return err
	}, nil
}

// 确定性复现：记录已是 waiting，上一次运行的 goroutine 还没退出。此时续跑不能被当作“已在运行”而丢失。
func TestResumeWaitsForPreviousRunToExit(t *testing.T) {
	store := &gatedUnlock{Store: newStore(t, t.TempDir()), docID: "goagent/background/m", reached: make(chan struct{}), gate: make(chan struct{})}
	var resumed atomic.Value
	m := newManager(t, testConfig(store, (&notices{}).add), waitingRunner(&resumed))
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	<-store.reached
	result := make(chan error, 1)
	go func() { result <- m.Resume(ctx, "a", map[string]string{waitKey: "approved"}) }()
	select {
	case err := <-result:
		close(store.gate)
		t.Fatalf("上一次运行没有退出时 Resume 就返回了: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(store.gate)
	require.NoError(t, <-result)
	waitStatus(t, m, "a", StatusCompleted)
	require.Equal(t, "approved", resumed.Load())
}

// Attach 按 Kind 和 ID 更新：先记意图，分配后补上实例。Detach 撤销没有分配成的意图。
func TestAttachUpdatesAndDetachRemoves(t *testing.T) {
	var released releaseLog
	proceed := make(chan struct{})
	attached := make(chan struct{})
	var m *Manager
	m = newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "k", Data: []byte(`{"instance":""}`)}))
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "k", Data: []byte(`{"instance":"i-1"}`)}))
		require.NoError(t, m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "gone"}))
		require.NoError(t, m.Detach(ctx, task.ID, "box", "gone"))
		close(attached)
		<-proceed
		return "x", nil
	})
	m.RegisterResource("box", released.release)
	runManager(t, m)
	_, err := m.Spawn(context.Background(), bg("a"))
	require.NoError(t, err)
	<-attached
	record, err := m.Get(context.Background(), "a")
	require.NoError(t, err)
	require.Len(t, record.Resources, 1)
	close(proceed)
	require.Eventually(t, func() bool { return len(released.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.JSONEq(t, `{"instance":"i-1"}`, string(released.all()[0].Data))
}

// 无法确认资源是否存在时不静默放过：交给宿主裁决，不重新执行业务。
func TestUnknownResourceBecomesProblemForHost(t *testing.T) {
	var runs atomic.Int32
	var calls atomic.Int32
	var m *Manager
	m = newManager(t, testConfig(newStore(t, t.TempDir()), (&notices{}).add), func(ctx context.Context, task Task) (string, error) {
		runs.Add(1)
		return "x", m.Attach(ctx, task.ID, Resource{Kind: "box", ID: "k"})
	})
	m.RegisterResource("box", func(context.Context, Resource) error {
		calls.Add(1)
		return ErrResourceUnknown
	})
	runManager(t, m)
	ctx := context.Background()
	_, err := m.Spawn(ctx, bg("a"))
	require.NoError(t, err)
	waitStatus(t, m, "a", StatusCompleted)
	var problems []effects.Record
	require.Eventually(t, func() bool {
		problems, err = m.Problems(ctx)
		return err == nil && len(problems) == 1
	}, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, effects.StateAttention, problems[0].State)
	settled := calls.Load()
	time.Sleep(30 * time.Millisecond)
	require.Equal(t, settled, calls.Load(), "交给宿主后不再重试")

	require.NoError(t, m.ResolveProblem(ctx, problems[0].Seq, true))
	problems, err = m.Problems(ctx)
	require.NoError(t, err)
	require.Empty(t, problems)
	require.EqualValues(t, 1, runs.Load())
}

// 宿主用 Key 派生新运行的执行键：重投时进入同一运行，而不是再开一次。
func TestNoticeKeyIsStablePerGeneration(t *testing.T) {
	first := Notice{ChildID: "c", Generation: 1}
	require.Equal(t, first.Key(), Notice{ChildID: "c", Generation: 1, Status: StatusCompleted}.Key())
	require.NotEqual(t, first.Key(), Notice{ChildID: "c", Generation: 2}.Key())
	require.NotEqual(t, first.Key(), Notice{ChildID: "d", Generation: 1}.Key())
}
