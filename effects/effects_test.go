package effects

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/kart-io/goagent/core/durable"
	"github.com/stretchr/testify/require"
)

const crashExit = 90

// harness 运行队列并在测试结束时等待它退出。
type harness struct {
	t      *testing.T
	queue  *Queue
	cancel context.CancelFunc
	done   chan error
}

func testConfig(store durable.Store, kinds map[string]Kind) Config {
	return Config{Store: store, Name: "q", Kinds: kinds, Poll: 5 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, LaterDelay: 5 * time.Millisecond}
}

func newStore(t *testing.T, dir string) *durable.FileStore {
	t.Helper()
	store, err := durable.NewFileStore(dir)
	require.NoError(t, err)
	return store
}

func newQueue(t *testing.T, cfg Config) *Queue {
	t.Helper()
	queue, err := New(cfg)
	require.NoError(t, err)
	return queue
}

func start(t *testing.T, queue *Queue) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, queue: queue, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- queue.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("队列没有退出")
	}
	h.done <- nil
}

func effect(kind, key, target string) Effect {
	return Effect{Kind: kind, Key: key, Target: target, Payload: json.RawMessage(`"p"`)}
}

func recordOf(t *testing.T, q *Queue, key string) Record {
	t.Helper()
	records, err := q.Records(context.Background())
	require.NoError(t, err)
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Key == key {
			return records[i]
		}
	}
	t.Fatalf("没有记录 %s", key)
	return Record{}
}

func waitState(t *testing.T, q *Queue, key string, state State) Record {
	t.Helper()
	var last Record
	require.Eventually(t, func() bool {
		last = recordOf(t, q, key)
		return last.State == state
	}, 30*time.Second, 2*time.Millisecond, "等待 %s 进入 %s", key, state)
	return last
}

// counter 记录每个键的执行次数和顺序。
type counter struct {
	mu    sync.Mutex
	runs  map[string]int
	order []string
}

func (c *counter) run(_ context.Context, e Effect) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runs == nil {
		c.runs = map[string]int{}
	}
	c.runs[e.Key]++
	c.order = append(c.order, e.Key)
	return nil
}

func (c *counter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[key]
}

func TestCommitIsAtomicWithDecision(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, t.TempDir())
	var c counter
	queue := newQueue(t, testConfig(store, map[string]Kind{"send": {Run: c.run}}))
	unlock, err := store.Lock(ctx, "decision")
	require.NoError(t, err)
	decision := durable.Write{ID: "decision", Snapshot: &durable.Snapshot{Version: "v", RunID: "decision", Data: json.RawMessage(`{}`)}}
	require.NoError(t, queue.Commit(ctx, []durable.Write{decision}, effect("send", "a", "")))
	require.NoError(t, unlock())
	_, err = store.Load(ctx, "decision")
	require.NoError(t, err)
	require.Equal(t, StateWaiting, recordOf(t, queue, "a").State)

	// 写入失败时决定和待办都不可见。
	boom := errors.New("disk full")
	failing := newQueue(t, testConfig(failSaveAll{Store: store, err: boom}, map[string]Kind{"send": {Run: c.run}}))
	unlock, err = store.Lock(ctx, "other")
	require.NoError(t, err)
	defer func() { _ = unlock() }()
	err = failing.Commit(ctx, []durable.Write{{ID: "other", Snapshot: decision.Snapshot}}, effect("send", "b", ""))
	require.ErrorIs(t, err, boom)
	_, err = store.Load(ctx, "other")
	require.ErrorIs(t, err, os.ErrNotExist)
	records, err := queue.Records(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
}

type failSaveAll struct {
	durable.Store
	err error
}

func (s failSaveAll) SaveAll(context.Context, ...durable.Write) error { return s.err }

// 提交后进程退出：重启后待办仍会执行且只执行一次。
func TestCommittedEffectRunsAfterProcessExit(t *testing.T) {
	const env = "GOAGENT_EFFECTS_COMMIT_DIR"
	if dir := os.Getenv(env); dir != "" {
		queue := newQueue(t, testConfig(newStore(t, dir), map[string]Kind{"send": {Run: func(context.Context, Effect) error { return nil }}}))
		require.NoError(t, queue.Enqueue(context.Background(), effect("send", "a", "")))
		os.Exit(crashExit)
	}
	dir := runChild(t, "TestCommittedEffectRunsAfterProcessExit", env)
	var c counter
	queue := newQueue(t, testConfig(newStore(t, dir), map[string]Kind{"send": {Run: c.run}}))
	start(t, queue)
	waitState(t, queue, "a", StateDone)
	require.Equal(t, 1, c.count("a"))
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

// 执行中进程退出：结果未知，没有声明幂等也没有核对时不重做，交给宿主。
func TestInterruptedEffectIsNotRerunBlindly(t *testing.T) {
	const env = "GOAGENT_EFFECTS_RUN_DIR"
	if dir := os.Getenv(env); dir != "" {
		queue := newQueue(t, testConfig(newStore(t, dir), map[string]Kind{"send": {Run: func(context.Context, Effect) error {
			os.Exit(crashExit)
			return nil
		}}}))
		require.NoError(t, queue.Enqueue(context.Background(), effect("send", "a", "")))
		_ = queue.Run(context.Background())
		t.Fatal("期望进程退出")
	}
	cases := []struct {
		name string
		kind func(*counter) Kind
		want State
		runs int
	}{
		{"未声明", func(c *counter) Kind { return Kind{Run: c.run} }, StateAttention, 0},
		{"幂等可重做", func(c *counter) Kind { return Kind{Run: c.run, Idempotent: true} }, StateDone, 1},
		{"核对已生效", func(c *counter) Kind {
			return Kind{Run: c.run, Reconcile: func(context.Context, Effect) (bool, error) { return true, nil }}
		}, StateDone, 0},
		{"核对未生效", func(c *counter) Kind {
			return Kind{Run: c.run, Reconcile: func(context.Context, Effect) (bool, error) { return false, nil }}
		}, StateDone, 1},
		{"核对失败", func(c *counter) Kind {
			return Kind{Run: c.run, Reconcile: func(context.Context, Effect) (bool, error) { return false, errors.New("unreachable") }}
		}, StateAttention, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := runChild(t, "TestInterruptedEffectIsNotRerunBlindly", env)
			var c counter
			queue := newQueue(t, testConfig(newStore(t, dir), map[string]Kind{"send": tc.kind(&c)}))
			start(t, queue)
			waitState(t, queue, "a", tc.want)
			require.Equal(t, tc.runs, c.count("a"))
		})
	}
}

func TestHandlerUnconfirmedGoesToAttentionAndCanBeResolved(t *testing.T) {
	var c counter
	calls := 0
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(ctx context.Context, e Effect) error {
		calls++
		if calls == 1 {
			return ErrUnconfirmed
		}
		return c.run(ctx, e)
	}}}))
	ctx := context.Background()
	require.NoError(t, queue.Enqueue(ctx, effect("send", "a", "")))
	start(t, queue)
	attention := waitState(t, queue, "a", StateAttention)
	require.ErrorContains(t, queue.Enqueue(ctx, effect("send", "a", "")), "结果未知", "结果未知时不能再排同键待办")
	problems, err := queue.Problems(ctx)
	require.NoError(t, err)
	require.Len(t, problems, 1)

	require.NoError(t, queue.Resolve(ctx, attention.Seq, false))
	waitState(t, queue, "a", StateDone)
	require.Equal(t, 1, c.count("a"))
	problems, err = queue.Problems(ctx)
	require.NoError(t, err)
	require.Empty(t, problems)
}

func TestWaitingEffectIsMergedKeepingPosition(t *testing.T) {
	ctx := context.Background()
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(context.Context, Effect) error { return nil }}}))
	require.NoError(t, queue.Enqueue(ctx, effect("send", "a", "t"), effect("send", "b", "t")))
	before := recordOf(t, queue, "a")
	require.NoError(t, queue.Enqueue(ctx, Effect{Kind: "send", Key: "a", Target: "t", Payload: json.RawMessage(`"new"`)}))
	after := recordOf(t, queue, "a")
	require.Equal(t, before.Seq, after.Seq)
	require.JSONEq(t, `"new"`, string(after.Payload))
	records, err := queue.Records(ctx)
	require.NoError(t, err)
	require.Len(t, records, 2)
}

func TestEnqueueWhileRunningQueuesBehind(t *testing.T) {
	ctx := context.Background()
	entered, release := make(chan struct{}, 2), make(chan struct{})
	var seen []string
	var mu sync.Mutex
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(_ context.Context, e Effect) error {
		mu.Lock()
		seen = append(seen, string(e.Payload))
		mu.Unlock()
		entered <- struct{}{}
		<-release
		return nil
	}}}))
	require.NoError(t, queue.Enqueue(ctx, Effect{Kind: "send", Key: "a", Target: "t", Payload: json.RawMessage(`"first"`)}))
	start(t, queue)
	<-entered
	require.NoError(t, queue.Enqueue(ctx, Effect{Kind: "send", Key: "a", Target: "t", Payload: json.RawMessage(`"second"`)}))
	close(release)
	<-entered
	require.Eventually(t, func() bool {
		records, err := queue.Records(ctx)
		return err == nil && len(records) == 2 && records[0].State == StateDone && records[1].State == StateDone
	}, 30*time.Second, 2*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{`"first"`, `"second"`}, seen)
}

func TestTargetOrderHoldsUntilRepeatedFailure(t *testing.T) {
	ctx := context.Background()
	var c counter
	failures := 0
	var mu sync.Mutex
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(ctx context.Context, e Effect) error {
		if e.Key == "first" {
			mu.Lock()
			defer mu.Unlock()
			failures++
			return errors.New("target down")
		}
		return c.run(ctx, e)
	}}}))
	require.NoError(t, queue.Enqueue(ctx, effect("send", "first", "t"), effect("send", "second", "t"), effect("send", "other", "u")))
	start(t, queue)
	waitState(t, queue, "other", StateDone) // 其他目标不等待。
	waitState(t, queue, "second", StateDone)
	mu.Lock()
	seen := failures
	mu.Unlock()
	require.GreaterOrEqual(t, seen, DefaultHoldAfter, "前面的待办连续失败到阈值后才放行")
	// 它还在重试，可能正在运行：等回到等待状态再看。
	first := waitState(t, queue, "first", StateWaiting)
	require.Equal(t, "execution", first.FailureKind)
	require.NotContains(t, string(mustJSON(t, first)), "target down", "不保存第三方错误文本")
	problems, err := queue.Problems(ctx)
	require.NoError(t, err)
	require.Len(t, problems, 1)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return raw
}

func TestLaterIsNotCountedAsFailure(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(context.Context, Effect) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls < 5 {
			return ErrLater
		}
		return nil
	}}}))
	require.NoError(t, queue.Enqueue(context.Background(), effect("send", "a", "")))
	start(t, queue)
	record := waitState(t, queue, "a", StateDone)
	require.Zero(t, record.Failures)
}

func TestDropAndAuthorizeKeepReason(t *testing.T) {
	var c counter
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{
		"gone": {Run: func(context.Context, Effect) error { return Drop("conversation_deleted") }},
		"denied": {Run: c.run, Authorize: func(context.Context, Effect) error {
			return errors.New("policy revision 7 denies")
		}},
	}))
	require.NoError(t, queue.Enqueue(context.Background(), effect("gone", "a", ""), effect("denied", "b", "")))
	start(t, queue)
	require.Equal(t, "conversation_deleted", waitState(t, queue, "a", StateDropped).Reason)
	require.Equal(t, "unauthorized", waitState(t, queue, "b", StateDropped).Reason)
	require.Zero(t, c.count("b"), "策略不允许时不执行")
	record := recordOf(t, queue, "a")
	require.False(t, record.SettledAt.IsZero())
}

func TestLimitsRejectWithoutTruncating(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(context.Context, Effect) error { return nil }}})
	cfg.MaxPending, cfg.MaxPayload = 1, 8
	queue := newQueue(t, cfg)
	require.ErrorIs(t, queue.Enqueue(ctx, Effect{Kind: "send", Key: "big", Payload: json.RawMessage(`"0123456789"`)}), ErrLimit)
	require.NoError(t, queue.Enqueue(ctx, effect("send", "a", "")))
	require.ErrorIs(t, queue.Enqueue(ctx, effect("send", "b", "")), ErrLimit)
	require.NoError(t, queue.Enqueue(ctx, effect("send", "a", "")), "合并不增加条数")
	require.Error(t, queue.Enqueue(ctx, effect("unknown", "c", "")), "未注册的种类")
	require.Error(t, queue.Enqueue(ctx, Effect{Kind: "send", Key: "d", Payload: json.RawMessage(`{`)}), "内容须是 JSON")
}

func TestSettledRecordsAreBounded(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(context.Context, Effect) error { return nil }}})
	cfg.RetainSettled = 2
	queue := newQueue(t, cfg)
	for _, key := range []string{"a", "b", "c", "d"} {
		require.NoError(t, queue.Enqueue(ctx, effect("send", key, "t")))
	}
	start(t, queue)
	require.Eventually(t, func() bool {
		records, err := queue.Records(ctx)
		return err == nil && len(records) == 2 && records[1].Key == "d" && records[1].State == StateDone
	}, 30*time.Second, 2*time.Millisecond)
}

func TestShutdownKeepsPendingAndStopsClaiming(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, t.TempDir())
	entered := make(chan struct{})
	var c counter
	queue := newQueue(t, testConfig(store, map[string]Kind{"send": {Idempotent: true, Run: func(ctx context.Context, e Effect) error {
		if e.Key == "a" {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		}
		return c.run(ctx, e)
	}}}))
	require.NoError(t, queue.Enqueue(ctx, effect("send", "a", "t"), effect("send", "b", "t")))
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- queue.Run(runCtx) }()
	<-entered
	cancel()
	require.NoError(t, <-done)
	require.Equal(t, StateRunning, recordOf(t, queue, "a").State, "中断的执行结果未知，留给下次启动判定")
	require.Equal(t, StateWaiting, recordOf(t, queue, "b").State)
	require.Zero(t, c.count("b"))

	var again counter
	restarted := newQueue(t, testConfig(store, map[string]Kind{"send": {Idempotent: true, Run: again.run}}))
	start(t, restarted)
	waitState(t, restarted, "b", StateDone)
	require.Equal(t, 1, again.count("a"))
}

func TestLeaseLostStopsRunner(t *testing.T) {
	ctx := context.Background()
	store := newStore(t, t.TempDir())
	var c counter
	queue := newQueue(t, testConfig(failSaveAll{Store: store, err: durable.ErrLeaseLost}, map[string]Kind{"send": {Run: c.run}}))
	seeded := newQueue(t, testConfig(store, map[string]Kind{"send": {Run: c.run}}))
	require.NoError(t, seeded.Enqueue(ctx, effect("send", "a", "")))
	require.ErrorIs(t, queue.Run(ctx), durable.ErrLeaseLost)
	require.Zero(t, c.count("a"), "认领没有落盘时不执行")
}

func TestOnlyOneRunnerAtATime(t *testing.T) {
	store := newStore(t, t.TempDir())
	kinds := map[string]Kind{"send": {Run: func(context.Context, Effect) error { return nil }}}
	first := newQueue(t, testConfig(store, kinds))
	h := start(t, first)
	require.NoError(t, first.Enqueue(context.Background(), effect("send", "a", "")))
	waitState(t, first, "a", StateDone)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, newQueue(t, testConfig(store, kinds)).Run(ctx), durable.ErrLockUnavailable)
	_ = h
}

// 目标持续返回结果未知时按失败退避，不会空转。
func TestRepeatedUnconfirmedBacksOff(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	cfg := testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Idempotent: true, Run: func(context.Context, Effect) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return ErrUnconfirmed
	}}})
	cfg.MinBackoff, cfg.MaxBackoff = 50*time.Millisecond, 50*time.Millisecond
	queue := newQueue(t, cfg)
	require.NoError(t, queue.Enqueue(context.Background(), effect("send", "a", "")))
	begin := time.Now()
	start(t, queue)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls >= 3
	}, 30*time.Second, 2*time.Millisecond, "持续重试")
	elapsed := time.Since(begin)
	mu.Lock()
	defer mu.Unlock()
	// 磁盘慢时只会更少，所以只查上界：每次至少隔一个退避。
	require.LessOrEqual(t, calls, int(elapsed/cfg.MinBackoff)+1, "没有空转")
}

func TestPanicIsRecordedAsFailure(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	queue := newQueue(t, testConfig(newStore(t, t.TempDir()), map[string]Kind{"send": {Run: func(context.Context, Effect) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls == 1 {
			panic("boom")
		}
		return nil
	}}}))
	require.NoError(t, queue.Enqueue(context.Background(), effect("send", "a", "")))
	start(t, queue)
	require.Equal(t, 1, waitState(t, queue, "a", StateDone).Failures)
}

func TestConfigValidation(t *testing.T) {
	store := newStore(t, t.TempDir())
	_, err := New(Config{Name: "q", Kinds: map[string]Kind{"a": {Run: func(context.Context, Effect) error { return nil }}}})
	require.Error(t, err, "缺少 Store")
	_, err = New(Config{Store: store, Kinds: map[string]Kind{"a": {Run: func(context.Context, Effect) error { return nil }}}})
	require.Error(t, err, "缺少名称")
	_, err = New(Config{Store: store, Name: "q", Kinds: map[string]Kind{"a": {}}})
	require.Error(t, err, "缺少 Run")
}
