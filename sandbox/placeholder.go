package sandbox

import (
	"context"
	"fmt"

	"github.com/kart-io/goagent/harness/env"
)

const placeholderID = "placeholder"

// Placeholder returns a Session bound to no instance, for validating an assembly
// whose instance is only allocated later. Every operation fails with ErrUnsupported.
func (r *Registry) Placeholder(req Request) (*Session, error) {
	if r == nil {
		return nil, fmt.Errorf("sandbox: unknown or unversioned provider %q", req.Provider)
	}
	p := r.Providers[req.Provider]
	if p == nil || p.Version() == "" {
		return nil, fmt.Errorf("sandbox: unknown or unversioned provider %q", req.Provider)
	}
	return newSession(placeholderEnvironment{network: req.Network}, req.Provider, p.Version(), false, false), nil
}

type placeholderEnvironment struct{ network bool }

func (e placeholderEnvironment) ID() string                  { return placeholderID }
func (e placeholderEnvironment) Root() string                { return "/" + placeholderID }
func (e placeholderEnvironment) FileSystem() env.FileSystem  { return placeholderFS{} }
func (e placeholderEnvironment) Executor() env.Executor      { return e }
func (e placeholderEnvironment) Isolated() bool              { return true }
func (e placeholderEnvironment) Close(context.Context) error { return nil }
func (e placeholderEnvironment) Destroy(context.Context) error {
	return nil
}
func (e placeholderEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Network: e.network}
}
func (e placeholderEnvironment) Run(context.Context, env.Command) (env.Result, error) {
	return env.Result{}, ErrUnsupported
}

type placeholderFS struct{}

func (placeholderFS) Resolve(string) (string, error) { return "", ErrUnsupported }
func (placeholderFS) Stat(context.Context, string) (env.Info, error) {
	return env.Info{}, ErrUnsupported
}
func (placeholderFS) ReadFile(context.Context, string) ([]byte, env.Version, error) {
	return nil, "", ErrUnsupported
}
func (placeholderFS) WriteFile(context.Context, string, []byte, env.WriteIntent) (env.Version, error) {
	return "", ErrUnsupported
}
func (placeholderFS) Remove(context.Context, string) error { return ErrUnsupported }
func (placeholderFS) Walk(context.Context, string, func(env.Info) error) error {
	return ErrUnsupported
}
