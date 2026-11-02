// Package postgres 在真实 Postgres 上验证后台子工作：FileStore 不校验锁的持有者，租约防护只有这里能覆盖。
// 独立模块，避免根模块依赖 pgx。未设置 GOAGENT_PGSTORE_DSN 时跳过。
package postgres

import (
	"context"
	crand "crypto/rand"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/kart-io/goagent-contrib/durable/pgstore"
	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/core/durable"
	"github.com/stretchr/testify/require"
)

const waitKey = "approval-key"

func schema(t *testing.T) (string, string) {
	t.Helper()
	dsn := os.Getenv("GOAGENT_PGSTORE_DSN")
	if dsn == "" {
		t.Skip("未设置 GOAGENT_PGSTORE_DSN，跳过 Postgres 测试")
	}
	name := "t_" + strings.ToLower(crand.Text()[:12])
	admin, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+name+` CASCADE`)
		admin.Close()
	})
	_, err = admin.Exec(context.Background(), `CREATE SCHEMA `+name)
	require.NoError(t, err)
	pool := newPool(t, dsn, name)
	for _, stmt := range strings.Split(pgstore.Schema+pgstore.SessionSchema, ";") {
		if strings.TrimSpace(stmt) != "" {
			_, err = pool.Exec(context.Background(), stmt)
			require.NoError(t, err)
		}
	}
	return dsn, name
}

func newPool(t *testing.T, dsn, name string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = name
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func store(t *testing.T, dsn, name string) *pgstore.Store {
	t.Helper()
	s, err := pgstore.New(context.Background(), pgstore.Config{Pool: newPool(t, dsn, name), LeaseTTL: time.Second, RenewInterval: 200 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.Close(ctx)
	})
	return s
}

type log struct {
	mu sync.Mutex
	n  []background.Notice
	r  []background.Resource
}

func (l *log) notify(_ context.Context, n background.Notice) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n = append(l.n, n)
	return nil
}
func (l *log) release(_ context.Context, r background.Resource) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.r = append(l.r, r)
	return nil
}
func (l *log) released() int { l.mu.Lock(); defer l.mu.Unlock(); return len(l.r) }

func manager(t *testing.T, s durable.Store, l *log, run background.Runner) *background.Manager {
	m, err := background.New(background.Config{Store: s, Name: "m", Notify: l.notify, Poll: 20 * time.Millisecond,
		Effects: effects.Config{Poll: 20 * time.Millisecond, MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, LaterDelay: 20 * time.Millisecond}})
	require.NoError(t, err)
	m.Register("r", run)
	m.RegisterResource("box", l.release)
	return m
}

func run(t *testing.T, m *background.Manager) (func(), <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	require.NoError(t, m.Ready(context.Background()))
	return cancel, done
}

func status(t *testing.T, m *background.Manager, id string, want background.Status) background.Record {
	var last background.Record
	require.Eventually(t, func() bool {
		r, err := m.Get(context.Background(), id)
		last = r
		return err == nil && r.Status == want
	}, 15*time.Second, 10*time.Millisecond, "want %s", want)
	return last
}

func task(id string) background.Task {
	return background.Task{ID: id, Owner: "o", Runner: "r", Input: "x", Mode: background.ModeBackground}
}

// 等待、停止、换连接重启、续跑、释放资源。
func TestWaitRestartResumeRelease(t *testing.T) {
	dsn, name := schema(t)
	var l1, l2 log
	var m1, m2 *background.Manager
	var runs atomic.Int32
	runner := func(get func() *background.Manager) background.Runner {
		return func(ctx context.Context, tk background.Task) (string, error) {
			runs.Add(1)
			if err := get().Attach(ctx, tk.ID, background.Resource{Kind: "box", ID: "box-1"}); err != nil {
				return "", err
			}
			if _, ok := durable.ResumeInput(durable.WithExecutionKey(ctx, waitKey)); !ok {
				return "", &durable.WaitError{Path: []durable.WaitStep{{ID: "a", Key: waitKey}}}
			}
			return "done", nil
		}
	}
	m1 = manager(t, store(t, dsn, name), &l1, runner(func() *background.Manager { return m1 }))
	stop, done := run(t, m1)
	_, err := m1.Spawn(context.Background(), task("a"))
	require.NoError(t, err)
	status(t, m1, "a", background.StatusWaiting)
	stop()
	require.NoError(t, <-done)

	m2 = manager(t, store(t, dsn, name), &l2, runner(func() *background.Manager { return m2 }))
	stop2, done2 := run(t, m2)
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, 1, runs.Load(), "等待中的工作重启后不重跑")
	r := status(t, m2, "a", background.StatusWaiting)
	require.Len(t, r.Resources, 1)
	require.NoError(t, m2.Resume(context.Background(), "a", map[string]string{waitKey: "ok"}))
	status(t, m2, "a", background.StatusCompleted)
	require.Eventually(t, func() bool { return l2.released() == 1 }, 15*time.Second, 10*time.Millisecond)
	stop2()
	require.NoError(t, <-done2)
	require.Zero(t, l1.released())
}

// 旧持有者失去租约：写入被拒绝，不释放资源；新持有者接管后继续并释放一次。
func TestTakeoverFencesOldHolder(t *testing.T) {
	dsn, name := schema(t)
	admin := newPool(t, dsn, name)
	var l1, l2 log
	attached := make(chan struct{})
	finish := make(chan struct{})
	var m1 *background.Manager
	s1 := store(t, dsn, name)
	m1 = manager(t, s1, &l1, func(ctx context.Context, tk background.Task) (string, error) {
		if err := m1.Attach(ctx, tk.ID, background.Resource{Kind: "box", ID: "box-1"}); err != nil {
			return "", err
		}
		close(attached)
		<-finish
		return "old-result", nil
	})
	stop1, done1 := run(t, m1)
	defer stop1()
	_, err := m1.Spawn(context.Background(), task("a"))
	require.NoError(t, err)
	<-attached

	// 把旧持有者的租约转给新持有者：模拟旧宿主暂停后租约过期。
	var m2 *background.Manager
	m2Done := make(chan struct{})
	m2 = manager(t, store(t, dsn, name), &l2, func(context.Context, background.Task) (string, error) {
		defer close(m2Done)
		return "new-result", nil
	})
	tag, err := admin.Exec(context.Background(), `DELETE FROM goagent_durable_lease`)
	require.NoError(t, err)
	require.Positive(t, tag.RowsAffected())
	stop2, done2 := run(t, m2)
	<-m2Done
	status(t, m2, "a", background.StatusCompleted)
	require.Eventually(t, func() bool { return l2.released() == 1 }, 15*time.Second, 10*time.Millisecond)

	close(finish)
	select {
	case err := <-done1:
		require.True(t, err == nil || errors.Is(err, durable.ErrLeaseLost), "%v", err)
	case <-time.After(15 * time.Second):
		stop1()
		<-done1
	}
	require.Zero(t, l1.released(), "旧持有者不释放资源")
	r, err := m2.Get(context.Background(), "a")
	require.NoError(t, err)
	require.Equal(t, "new-result", r.Result, "旧持有者的结果没有覆盖")
	stop2()
	require.NoError(t, <-done2)
}

// 通知去重依赖键与条目原子落盘：换连接、确认之后都不产生第二条。
func TestInboxAppendOnceAcrossConnections(t *testing.T) {
	dsn, name := schema(t)
	ctx := durable.WithJournal(durable.WithExecutionKey(context.Background(), "owner"), store(t, dsn, name))
	box, _, err := durable.OpenInbox(ctx)
	require.NoError(t, err)
	seq, duplicate, err := box.AppendOnce(ctx, "notice-1", "steer", "a")
	require.NoError(t, err)
	require.False(t, duplicate)
	require.NoError(t, box.Ack(ctx, seq))

	other := durable.WithJournal(durable.WithExecutionKey(context.Background(), "owner"), store(t, dsn, name))
	reopened, entries, err := durable.OpenInbox(other)
	require.NoError(t, err)
	require.Empty(t, entries)
	_, duplicate, err = reopened.AppendOnce(other, "notice-1", "steer", "a")
	require.NoError(t, err)
	require.True(t, duplicate)
}
