package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

type testEnvironment struct {
	fs               env.FileSystem
	run              func(context.Context, env.Command) (env.Result, error)
	destroys, closes atomic.Int32
}

func (e *testEnvironment) ID() string                     { return "sandbox-test" }
func (e *testEnvironment) Root() string                   { return "/workspace" }
func (e *testEnvironment) FileSystem() env.FileSystem     { return e.fs }
func (e *testEnvironment) Executor() env.Executor         { return e }
func (e *testEnvironment) Isolated() bool                 { return true }
func (e *testEnvironment) Capabilities() env.Capabilities { return env.Capabilities{Container: true} }
func (e *testEnvironment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	if e.run != nil {
		return e.run(ctx, c)
	}
	return env.Result{Output: []byte("ok")}, nil
}
func (e *testEnvironment) Close(context.Context) error   { e.closes.Add(1); return nil }
func (e *testEnvironment) Destroy(context.Context) error { e.destroys.Add(1); return nil }

type testProvider struct {
	e        Environment
	err      error
	create   func(context.Context, Request) (Environment, error)
	requests []Request
}

func (*testProvider) Version() string { return "test-v1" }
func (p *testProvider) Create(ctx context.Context, r Request) (Environment, error) {
	p.requests = append(p.requests, r)
	if p.create != nil {
		return p.create(ctx, r)
	}
	return p.e, p.err
}
func (p *testProvider) Connect(_ context.Context, _ string, r Request) (Environment, error) {
	p.requests = append(p.requests, r)
	return p.e, p.err
}
func fixture(t *testing.T) (*testEnvironment, *Registry) {
	t.Helper()
	fs, err := env.NewLocalFS(t.TempDir())
	require.NoError(t, err)
	e := &testEnvironment{fs: fs}
	return e, &Registry{Namespace: "task-1", Providers: map[string]Provider{"test": &testProvider{e: e}}}
}
func TestSessionOwnershipAndRetain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		binding Binding
		destroy int32
	}{{"owned", Binding{}, 1}, {"borrowed", Binding{ID: "sandbox-test"}, 0}, {"retained", Binding{Retain: true}, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			e, r := fixture(t)
			r.Resolve = func(Request) (Binding, error) { return tc.binding, nil }
			s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "coder/workspace"})
			require.NoError(t, err)
			require.Equal(t, "sandbox-test", s.Info().ID)
			require.NoError(t, s.Close(context.Background()))
			require.NoError(t, s.Close(context.Background()))
			require.Equal(t, tc.destroy, e.destroys.Load())
			require.EqualValues(t, 1, e.closes.Load())
			_, err = s.Executor().Run(context.Background(), env.Command{Argv: []string{"echo"}})
			require.ErrorIs(t, err, ErrClosed)
		})
	}
}
func TestSessionCloseCancelsButDoesNotReleaseActiveOperationEarly(t *testing.T) {
	e, r := fixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	e.run = func(ctx context.Context, _ env.Command) (env.Result, error) {
		close(started)
		<-release
		return env.Result{}, ctx.Err()
	}
	s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "coder"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := s.Executor().Run(context.Background(), env.Command{}); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, s.Close(ctx), context.DeadlineExceeded)
	require.Zero(t, e.destroys.Load())
	close(release)
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, s.Close(context.Background()))
	require.EqualValues(t, 1, e.destroys.Load())
}
func TestRegistryKeepsPartialAndLateAllocationForCleanup(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial", true: "late"}[late], func(t *testing.T) {
			e, r := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cause := errors.New("failed after allocating")
			r.Providers["test"] = &testProvider{create: func(context.Context, Request) (Environment, error) {
				if late {
					cancel()
					return e, nil
				}
				return e, cause
			}}
			s, err := r.Open(ctx, Request{Provider: "test", Scope: "coder"})
			require.Error(t, err)
			require.NotNil(t, s)
			require.NoError(t, s.Close(context.Background()))
			require.EqualValues(t, 1, e.destroys.Load())
		})
	}
}
func TestSessionFileOperationsUseLifecycleAndCannotRunAfterClose(t *testing.T) {
	_, r := fixture(t)
	s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "coder"})
	require.NoError(t, err)
	_, err = s.FileSystem().WriteFile(context.Background(), "file", []byte("body"), env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)
	data, _, err := s.FileSystem().ReadFile(context.Background(), "file")
	require.NoError(t, err)
	require.Equal(t, "body", string(data))
	require.NoError(t, s.Close(context.Background()))
	_, _, err = s.FileSystem().ReadFile(context.Background(), "file")
	require.ErrorIs(t, err, ErrClosed)
	_, err = s.FileSystem().Resolve("file")
	require.ErrorIs(t, err, ErrClosed)
}
func TestRegistryValidatesBeforeCallingProvider(t *testing.T) {
	_, r := fixture(t)
	_, err := r.Open(context.Background(), Request{Provider: "missing", Scope: "coder"})
	require.Error(t, err)
	_, err = r.Open(context.Background(), Request{Provider: "test"})
	require.Error(t, err)
	r.Namespace = ""
	_, err = r.Open(context.Background(), Request{Provider: "test", Scope: "coder"})
	require.Error(t, err)
}

func TestRegistryShutdownTracksLateAllocation(t *testing.T) {
	e, r := fixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.Providers["test"] = &testProvider{create: func(context.Context, Request) (Environment, error) { close(entered); <-release; return e, nil }}
	done := make(chan error, 1)
	go func() { _, err := r.Open(context.Background(), Request{Provider: "test", Scope: "coder"}); done <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, r.Close(ctx), context.DeadlineExceeded)
	close(release)
	require.Error(t, <-done)
	require.NoError(t, r.Close(context.Background()))
	require.EqualValues(t, 1, e.destroys.Load())
	require.EqualValues(t, 1, e.closes.Load())
	_, err := r.Open(context.Background(), Request{Provider: "test", Scope: "other"})
	require.ErrorIs(t, err, ErrClosed)
}

func TestRegistryRejectsProviderThatCannotHonorNetwork(t *testing.T) {
	e, r := fixture(t)
	s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "coder", Network: true})
	require.ErrorIs(t, err, ErrUnsupported)
	require.NotNil(t, s)
	require.NoError(t, s.Close(context.Background()))
	require.EqualValues(t, 1, e.destroys.Load())
}

type testProtectedFS struct {
	env.FileSystem
	names []string
}

func (f testProtectedFS) WithProtectedNames(names ...string) env.FileSystem {
	f.names = append(append([]string(nil), f.names...), names...)
	return f
}
func (f testProtectedFS) WriteFile(ctx context.Context, path string, data []byte, intent env.WriteIntent) (env.Version, error) {
	for _, name := range f.names {
		if path == name {
			return "", env.ErrReadOnly
		}
	}
	return f.FileSystem.WriteFile(ctx, path, data, intent)
}
func TestSessionForwardsMutationProtectionWithoutBypassingLifetime(t *testing.T) {
	e, r := fixture(t)
	e.fs = testProtectedFS{FileSystem: e.fs}
	s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "protected"})
	require.NoError(t, err)
	guarded := s.FileSystem().(MutationProtector).WithProtectedNames(".git")
	_, err = guarded.WriteFile(context.Background(), ".git", nil, env.WriteIntent{})
	require.ErrorIs(t, err, env.ErrReadOnly)
	require.NoError(t, s.Close(context.Background()))
	_, err = guarded.WriteFile(context.Background(), "safe", nil, env.WriteIntent{})
	require.ErrorIs(t, err, ErrClosed)
}

func TestSessionDoesNotAdvertiseUnsupportedMutationProtection(t *testing.T) {
	_, r := fixture(t)
	s, err := r.Open(context.Background(), Request{Provider: "test", Scope: "plain"})
	require.NoError(t, err)
	_, supported := s.FileSystem().(MutationProtector)
	require.False(t, supported)
	require.NoError(t, s.Close(context.Background()))
}

func TestRegistryDestroyConnectsAndDeletes(t *testing.T) {
	e, r := fixture(t)
	req := Request{Provider: "test", Scope: "s"}
	require.NoError(t, r.Destroy(context.Background(), req, "sandbox-test"))
	require.EqualValues(t, 1, e.destroys.Load())
	require.EqualValues(t, 1, e.closes.Load())
	require.NoError(t, r.Close(context.Background()))
	require.ErrorIs(t, r.Destroy(context.Background(), req, "sandbox-test"), ErrClosed)
	require.EqualValues(t, 1, e.destroys.Load())
}

func TestRegistryOpenWithUsesCallerBinding(t *testing.T) {
	e, r := fixture(t)
	r.Resolve = func(Request) (Binding, error) { return Binding{}, errors.New("不应调用 Resolve") }
	s, err := r.OpenWith(context.Background(), Request{Provider: "test", Scope: "s"}, func(req Request) (Binding, error) {
		require.Contains(t, req.Scope, "task-1", "回调收到的 Scope 带命名空间")
		return Binding{Retain: true}, nil
	})
	require.NoError(t, err)
	require.True(t, s.Info().Owned)
	require.True(t, s.Info().Retained)
	require.NoError(t, s.Close(context.Background()))
	require.Zero(t, e.destroys.Load())
	require.NoError(t, r.Close(context.Background()))
}

type findingProvider struct {
	*testProvider
	id    string
	found bool
	asked []Request
}

func (p *findingProvider) FindByScope(_ context.Context, r Request) (string, bool, error) {
	p.asked = append(p.asked, r)
	return p.id, p.found, nil
}

func TestRegistryFindByScope(t *testing.T) {
	e, r := fixture(t)
	req := Request{Provider: "test", Scope: "s"}
	_, _, err := r.FindByScope(context.Background(), req)
	require.ErrorIs(t, err, ErrUnsupported)

	finder := &findingProvider{testProvider: &testProvider{e: e}, id: "i-1", found: true}
	r.Providers["test"] = finder
	id, found, err := r.FindByScope(context.Background(), req)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "i-1", id)
	require.Contains(t, finder.asked[0].Scope, "task-1", "与 Create 收到的 Scope 相同，带命名空间")
	require.NoError(t, r.Close(context.Background()))
}

func TestPlaceholderSessionHasNoInstance(t *testing.T) {
	e, r := fixture(t)
	session, err := r.Placeholder(Request{Provider: "test", Scope: "s", Network: true})
	require.NoError(t, err)
	require.True(t, session.Info().Capabilities.Network)
	_, _, err = session.FileSystem().ReadFile(context.Background(), "a.txt")
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = session.Executor().Run(context.Background(), env.Command{Argv: []string{"true"}})
	require.ErrorIs(t, err, ErrUnsupported)
	require.NoError(t, session.Close(context.Background()))
	require.Zero(t, e.destroys.Load())
	_, err = r.Placeholder(Request{Provider: "unknown", Scope: "s"})
	require.Error(t, err)
	require.NoError(t, r.Close(context.Background()))
}

// destroyingProvider 不能连接已停止的实例，但能直接删除。
type destroyingProvider struct {
	*testProvider
	destroyed []string
	err       error
}

func (p *destroyingProvider) Connect(context.Context, string, Request) (Environment, error) {
	return nil, errors.New("实例已停止，无法连接")
}

func (p *destroyingProvider) DestroyInstance(_ context.Context, id string, _ Request) error {
	p.destroyed = append(p.destroyed, id)
	return p.err
}

func TestRegistryDestroyDoesNotNeedConnection(t *testing.T) {
	p := &destroyingProvider{testProvider: &testProvider{}}
	r := &Registry{Namespace: "ns", Providers: map[string]Provider{"p": p}}
	req := Request{Provider: "p", Scope: "s", Network: true}
	require.NoError(t, r.Destroy(context.Background(), req, "stopped"))
	require.Equal(t, []string{"stopped"}, p.destroyed)

	p.err = ErrInstanceNotFound
	require.NoError(t, r.Destroy(context.Background(), req, "gone"), "已经不存在就是删除成功，重试不报错")

	p.err = errors.New("网络断开")
	require.ErrorContains(t, r.Destroy(context.Background(), req, "unknown"), "网络断开", "查不准时不能当作成功")

	require.NoError(t, r.Close(context.Background()))
	require.ErrorIs(t, r.Destroy(context.Background(), req, "late"), ErrClosed)
}

// goneProvider 没有实现 InstanceDestroyer，实例已不存在。
type goneProvider struct{ *testProvider }

func (goneProvider) Connect(context.Context, string, Request) (Environment, error) {
	return nil, fmt.Errorf("连接失败: %w", ErrInstanceNotFound)
}

func TestRegistryDestroyTreatsMissingInstanceAsDone(t *testing.T) {
	r := &Registry{Namespace: "ns", Providers: map[string]Provider{"p": goneProvider{&testProvider{}}}}
	require.NoError(t, r.Destroy(context.Background(), Request{Provider: "p", Scope: "s", Network: true}, "gone"))
	require.NoError(t, r.Close(context.Background()))
}
