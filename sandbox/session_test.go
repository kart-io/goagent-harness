package sandbox

import (
	"context"
	"errors"
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
