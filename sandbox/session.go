package sandbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/kart-io/goagent/harness/env"
)

// Session owns operation tracking and the selected lifetime policy.
type Session struct {
	backend         Environment
	fs              env.FileSystem
	executor        env.Executor
	info            Info
	ctx             context.Context
	cancel          context.CancelFunc
	mu              sync.Mutex
	closing, closed bool
	active          int
	idle            chan struct{}
	closeGate       chan struct{}
	destroyed       bool
	onClosed        func()
}

func newSession(e Environment, provider, version string, owned, retain bool) *Session {
	ctx, cancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	s := &Session{backend: e, fs: e.FileSystem(), executor: e.Executor(), ctx: ctx, cancel: cancel, idle: idle, closeGate: make(chan struct{}, 1)}
	s.closeGate <- struct{}{}
	s.info = Info{ID: e.ID(), Root: e.Root(), Provider: provider, Version: version, Isolated: e.Isolated(), Owned: owned, Retained: retain}
	if s.executor != nil {
		s.info.Capabilities = s.executor.Capabilities()
	}
	return s
}
func (s *Session) Info() Info                 { return s.info }
func (s *Session) Executor() env.Executor     { return sessionExecutor{s} }
func (s *Session) FileSystem() env.FileSystem { return managedFS(s, s.fs) }

func (s *Session) begin(parent context.Context) (context.Context, func(), error) {
	if err := parent.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, ErrClosed
	}
	if s.active == 0 {
		s.idle = make(chan struct{})
	}
	s.active++
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(s.ctx, cancel)
	done := func() {
		stop()
		cancel()
		s.mu.Lock()
		s.active--
		if s.active == 0 {
			close(s.idle)
		}
		s.mu.Unlock()
	}
	return ctx, done, nil
}

// BeginClose closes operation admission and cancels without waiting.
func (s *Session) BeginClose() { s.mu.Lock(); s.closing = true; s.cancel(); s.mu.Unlock() }

// Close closes admission, cancels and waits for actual operation returns, then
// deletes owned/non-retained environments. Deadline failure can be retried.
func (s *Session) Close(ctx context.Context) error {
	s.BeginClose()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.closeGate:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { s.closeGate <- struct{}{} }()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	s.cancel()
	idle := s.idle
	s.mu.Unlock()
	select {
	case <-idle:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.info.Owned && !s.info.Retained && !s.destroyed {
		if err := s.backend.Destroy(ctx); err != nil {
			return err
		}
		s.destroyed = true
	}
	if err := s.backend.Close(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if s.onClosed != nil {
		s.onClosed()
	}
	return nil
}

type sessionExecutor struct{ s *Session }

func (e sessionExecutor) Capabilities() env.Capabilities { return e.s.info.Capabilities }
func (e sessionExecutor) Run(parent context.Context, c env.Command) (env.Result, error) {
	ctx, done, err := e.s.begin(parent)
	if err != nil {
		return env.Result{}, err
	}
	defer done()
	return e.s.executor.Run(ctx, c)
}

type sessionFS struct {
	s  *Session
	fs env.FileSystem
}

func (f sessionFS) Resolve(path string) (string, error) {
	return f.ResolveContext(context.Background(), path)
}
func (f sessionFS) ResolveContext(parent context.Context, path string) (string, error) {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return "", err
	}
	defer done()
	if resolver, ok := f.fs.(ContextResolver); ok {
		return resolver.ResolveContext(ctx, path)
	}
	return f.fs.Resolve(path)
}
func (f sessionFS) Stat(parent context.Context, path string) (env.Info, error) {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return env.Info{}, err
	}
	defer done()
	return f.fs.Stat(ctx, path)
}
func (f sessionFS) ReadFile(parent context.Context, path string) ([]byte, env.Version, error) {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return nil, "", err
	}
	defer done()
	return f.fs.ReadFile(ctx, path)
}
func (f sessionFS) WriteFile(parent context.Context, path string, data []byte, intent env.WriteIntent) (env.Version, error) {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return "", err
	}
	defer done()
	return f.fs.WriteFile(ctx, path, data, intent)
}
func (f sessionFS) Remove(parent context.Context, path string) error {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return err
	}
	defer done()
	return f.fs.Remove(ctx, path)
}
func (f sessionFS) Walk(parent context.Context, path string, fn func(env.Info) error) error {
	ctx, done, err := f.s.begin(parent)
	if err != nil {
		return err
	}
	defer done()
	return f.fs.Walk(ctx, path, fn)
}

// Advertise atomic mutation protection only when the backend provides it.
type protectedSessionFS struct{ sessionFS }

func managedFS(s *Session, base env.FileSystem) env.FileSystem {
	f := sessionFS{s, base}
	if _, ok := base.(MutationProtector); ok {
		return protectedSessionFS{f}
	}
	return f
}
func (f protectedSessionFS) WithProtectedNames(names ...string) env.FileSystem {
	base := f.fs.(MutationProtector).WithProtectedNames(names...)
	return managedFS(f.s, base)
}

// CheckPolicy validates the actual backend contract, not its capability flags.
func (s *Session) CheckPolicy(parent context.Context, p Policy) error {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return err
	}
	defer done()
	e, ok := s.executor.(PolicyExecutor)
	if !ok {
		return fmt.Errorf("%w: managed executor does not enforce structured policies", ErrUnsupported)
	}
	return e.CheckPolicy(ctx, clone(p))
}

// RunWithPolicy tracks policy-aware execution under the session lifecycle.
func (s *Session) RunWithPolicy(parent context.Context, c env.Command, p Policy) (env.Result, error) {
	ctx, done, err := s.begin(parent)
	if err != nil {
		return env.Result{}, err
	}
	defer done()
	e, ok := s.executor.(PolicyExecutor)
	if !ok {
		return env.Result{}, ErrUnsupported
	}
	if err = e.CheckPolicy(ctx, clone(p)); err != nil {
		return env.Result{}, err
	}
	return e.RunWithPolicy(ctx, c, clone(p))
}
