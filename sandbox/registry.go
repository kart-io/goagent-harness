package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Registry is configured by the host and treated as immutable during use.
// Namespace identifies the task/recovery domain; Resolve is a trusted host
// callback that may select an existing instance. By default each Open creates.
type Registry struct {
	Namespace string
	Providers map[string]Provider
	Resolve   func(Request) (Binding, error)
	mu        sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
	closing   bool
	pending   int
	idle      chan struct{}
	sessions  map[*Session]struct{}
}

// Open can return a non-nil Session together with an error after allocation.
// Always take responsibility for closing a returned session before handling err.
func (r *Registry) Open(ctx context.Context, req Request) (*Session, error) {
	if r == nil || strings.TrimSpace(r.Namespace) == "" || req.Provider == "" || strings.TrimSpace(req.Scope) == "" {
		return nil, errors.New("sandbox: namespace, provider and scope are required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := r.Providers[req.Provider]
	if p == nil || p.Version() == "" {
		return nil, fmt.Errorf("sandbox: unknown or unversioned provider %q", req.Provider)
	}
	r.mu.Lock()
	r.initLocked()
	if r.closing {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	if r.pending == 0 {
		r.idle = make(chan struct{})
	}
	r.pending++
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	r.mu.Unlock()
	defer func() {
		stop()
		cancel()
		r.mu.Lock()
		r.pending--
		if r.pending == 0 {
			close(r.idle)
		}
		r.mu.Unlock()
	}()
	ctx = opCtx
	// A length-prefixed namespace avoids ambiguous concatenation with scope.
	req.Scope = fmt.Sprintf("%d:%s:%s", len(r.Namespace), r.Namespace, req.Scope)
	binding := Binding{}
	var err error
	if r.Resolve != nil {
		binding, err = r.Resolve(req)
		if err != nil {
			return nil, err
		}
	}
	var backend Environment
	if binding.ID == "" {
		backend, err = p.Create(ctx, req)
	} else {
		backend, err = p.Connect(ctx, binding.ID, req)
	}
	if backend == nil {
		if err == nil {
			err = errors.New("sandbox provider returned nil environment")
		}
		return nil, err
	}
	session := newSession(backend, req.Provider, p.Version(), binding.ID == "", binding.Retain)
	session.onClosed = func() { r.mu.Lock(); delete(r.sessions, session); r.mu.Unlock() }
	r.mu.Lock()
	r.sessions[session] = struct{}{}
	closing := r.closing
	r.mu.Unlock()
	if closing {
		session.BeginClose()
		return session, errors.Join(err, ErrClosed)
	}
	if err != nil {
		return session, err
	}
	if err = ctx.Err(); err != nil {
		return session, err
	}
	if session.info.ID == "" || session.info.Root == "" || session.fs == nil || session.executor == nil {
		return session, errors.New("sandbox provider returned an incomplete environment")
	}
	if session.info.Capabilities.Network != req.Network {
		return session, fmt.Errorf("%w: actual network capability differs from request", ErrUnsupported)
	}
	if session.info.Capabilities.Remote {
		if _, ok := session.fs.(ContextResolver); !ok {
			return session, fmt.Errorf("%w: remote filesystem must implement ContextResolver", ErrUnsupported)
		}
	}
	return session, nil
}

func (r *Registry) initLocked() {
	if r.ctx == nil {
		r.ctx, r.cancel = context.WithCancel(context.Background())
		r.idle = make(chan struct{})
		close(r.idle)
		r.sessions = map[*Session]struct{}{}
	}
}

// Close closes admission, cancels pending opens and all sessions, and waits for
// late allocations before cleanup. Failed cleanup remains registered for retry.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	r.initLocked()
	r.closing = true
	r.cancel()
	idle := r.idle
	sessions := make([]*Session, 0, len(r.sessions))
	for s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()
	for _, s := range sessions {
		s.BeginClose()
	}
	select {
	case <-idle:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	sessions = sessions[:0]
	for s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		errs = append(errs, s.Close(ctx))
	}
	return errors.Join(errs...)
}
