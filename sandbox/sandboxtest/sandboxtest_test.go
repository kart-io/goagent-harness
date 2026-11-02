package sandboxtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

// fakeProvider 把实例放在本地目录，刻意写坏某一点时用来证明契约能发现它。
type fakeProvider struct {
	mu     sync.Mutex
	dir    string
	scopes map[string]string
	next   int
	broken string
}

func (p *fakeProvider) Version() string { return "fake-v1" }

func (p *fakeProvider) open(id string) (sandbox.Environment, error) {
	root := filepath.Join(p.dir, id, "root")
	fsys, err := env.NewLocalFS(root)
	if err != nil {
		return nil, err
	}
	return &fakeEnvironment{p: p, id: id, root: root, fs: fsys}, nil
}

func (p *fakeProvider) Create(_ context.Context, r sandbox.Request) (sandbox.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := fmt.Sprintf("i%d", p.next)
	if err := os.MkdirAll(filepath.Join(p.dir, id, "root"), 0o700); err != nil {
		return nil, err
	}
	if p.scopes == nil {
		p.scopes = map[string]string{}
	}
	p.scopes[r.Scope] = id
	return p.open(id)
}

func (p *fakeProvider) Connect(_ context.Context, id string, _ sandbox.Request) (sandbox.Environment, error) {
	if _, err := os.Stat(filepath.Join(p.dir, id)); err != nil {
		if p.broken == "connect-plain-error" {
			return nil, errors.New("连接失败")
		}
		return nil, sandbox.ErrInstanceNotFound
	}
	return p.open(id)
}

func (p *fakeProvider) FindByScope(_ context.Context, r sandbox.Request) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.broken == "find-stale" {
		return "", false, nil
	}
	id, ok := p.scopes[r.Scope]
	if !ok {
		return "", false, nil
	}
	if _, err := os.Stat(filepath.Join(p.dir, id)); err != nil {
		return "", false, nil
	}
	return id, true, nil
}

func (p *fakeProvider) Stop(context.Context, string) error { return sandbox.ErrUnsupported }
func (p *fakeProvider) Exists(_ context.Context, id string) (bool, error) {
	_, err := os.Stat(filepath.Join(p.dir, id))
	return err == nil, nil
}

type fakeEnvironment struct {
	p    *fakeProvider
	id   string
	root string
	fs   *env.LocalFS
}

func (e *fakeEnvironment) ID() string                 { return e.id }
func (e *fakeEnvironment) Root() string               { return e.root }
func (e *fakeEnvironment) FileSystem() env.FileSystem { return e.fs }
func (e *fakeEnvironment) Executor() env.Executor     { return e }
func (e *fakeEnvironment) Isolated() bool             { return true }
func (e *fakeEnvironment) Close(context.Context) error {
	return nil
}
func (e *fakeEnvironment) Destroy(context.Context) error {
	dir := filepath.Join(e.p.dir, e.id)
	if e.p.broken == "destroy-not-retryable" {
		if _, err := os.Stat(dir); err != nil {
			return errors.New("实例不存在，删除失败")
		}
	}
	if e.p.broken == "destroy-keeps-instance" {
		return nil
	}
	return os.RemoveAll(dir)
}
func (e *fakeEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Network: e.p.broken == "network-open"}
}
func (e *fakeEnvironment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = filepath.Join(e.root, c.Dir)
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return env.Result{Output: out, ExitCode: exited.ExitCode()}, ctx.Err()
	}
	return env.Result{Output: out}, errors.Join(err, ctx.Err())
}
func (e *fakeEnvironment) CheckPolicy(context.Context, sandbox.Policy) error { return nil }
func (e *fakeEnvironment) RunWithPolicy(ctx context.Context, c env.Command, _ sandbox.Policy) (env.Result, error) {
	return e.Run(ctx, c)
}

func (e *fakeEnvironment) supervisor() *sandbox.CommandSupervisor {
	return &sandbox.CommandSupervisor{Exec: e, StateDir: filepath.Join(e.p.dir, e.id, "state"), PollInterval: 10 * time.Millisecond}
}
func (e *fakeEnvironment) RunKeyed(ctx context.Context, c sandbox.KeyedCommand, p sandbox.Policy) (sandbox.KeyedResult, error) {
	if e.p.broken == "keyed-attach-starts" {
		c.AttachOnly = false
	}
	return e.supervisor().RunKeyed(ctx, c, p)
}
func (e *fakeEnvironment) CancelKeyed(ctx context.Context, key string, p sandbox.Policy) (bool, error) {
	return e.supervisor().CancelKeyed(ctx, key, p)
}

func requirePOSIX(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "setsid", "flock", "mkfifo", "head"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("需要 %s", tool)
		}
	}
}

func TestContractPassesForCorrectProvider(t *testing.T) {
	requirePOSIX(t)
	p := &fakeProvider{dir: t.TempDir()}
	Run(t, Harness{Provider: p, Admin: p, Timeout: 30 * time.Second})
}

// 每种写坏的 Provider 都要被对应的检查发现。
func TestContractCatchesBrokenProviders(t *testing.T) {
	requirePOSIX(t)
	for _, test := range []struct{ broken, check string }{
		{"connect-plain-error", "connect-missing"},
		{"destroy-not-retryable", "lifecycle"},
		{"destroy-keeps-instance", "lifecycle"},
		{"find-stale", "find-by-scope"},
		{"network-open", "network"},
		{"keyed-attach-starts", "keyed-command"},
	} {
		t.Run(test.broken, func(t *testing.T) {
			p := &fakeProvider{dir: t.TempDir(), broken: test.broken}
			for _, check := range Checks() {
				if check.Name != test.check {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				require.Error(t, check.Run(ctx, Harness{Provider: p, Admin: p}))
				return
			}
			t.Fatalf("没有检查 %s", test.check)
		})
	}
}

func TestFaultsLoseCreateResponseKeepsInstance(t *testing.T) {
	inner := &fakeProvider{dir: t.TempDir()}
	f := &Faults{Provider: inner}
	f.LoseCreateResponse.Store(1)
	r := sandbox.Request{Scope: "s"}
	_, err := f.Create(context.Background(), r)
	require.ErrorIs(t, err, ErrInjected)
	id, found, err := f.FindByScope(context.Background(), r)
	require.NoError(t, err)
	require.True(t, found, "响应丢了，实例还在")

	f.FailDestroy.Store(1)
	require.ErrorIs(t, f.DestroyInstance(context.Background(), id, r), ErrInjected)
	require.NoError(t, f.DestroyInstance(context.Background(), id, r))
	require.ErrorIs(t, f.DestroyInstance(context.Background(), id, r), sandbox.ErrInstanceNotFound)
}
