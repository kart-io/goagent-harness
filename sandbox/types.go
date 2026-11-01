// Package sandbox manages host-selected execution environments without binding
// the Harness root module to a sandbox vendor SDK.
package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/kart-io/goagent/harness/env"
)

var ErrClosed = errors.New("sandbox session is closing or closed")
var ErrUnsupported = errors.New("sandbox capability is unsupported")
var ErrUnconfirmed = errors.New("sandbox operation outcome is unconfirmed")
var ErrOutputLimit = errors.New("sandbox output limit exceeded")

// UnconfirmedError carries operation identity without credentials. It must not
// be interpreted as proof that the remote operation never happened.
type UnconfirmedError struct {
	SandboxID, CommandID string
	Cause                error
}

func (e *UnconfirmedError) Error() string {
	return fmt.Sprintf("%v (sandbox=%s command=%s): %v", ErrUnconfirmed, e.SandboxID, e.CommandID, e.Cause)
}
func (e *UnconfirmedError) Unwrap() []error { return []error{ErrUnconfirmed, e.Cause} }

// Request contains validated preset requirements. Registry qualifies Scope
// with the trusted host namespace. It never contains credentials or sandbox ID.
type Request struct {
	Provider, Scope, Image string
	Network                bool
}
type Binding struct {
	ID     string
	Retain bool
}

type Provider interface {
	Version() string
	Create(context.Context, Request) (Environment, error)
	Connect(context.Context, string, Request) (Environment, error)
}

// Environment must bind its file system and executor to the same workspace.
// Close disconnects clients; Destroy deletes the remote instance. Both must
// honor context and tolerate retries after errors. Returned partial resources
// must accompany an error so the caller can still take ownership of cleanup.
type Environment interface {
	ID() string
	Root() string
	FileSystem() env.FileSystem
	Executor() env.Executor
	Isolated() bool
	Close(context.Context) error
	Destroy(context.Context) error
}

// ContextResolver lets remote filesystems honor the lifecycle even though the
// core FileSystem.Resolve method has no context parameter.
type ContextResolver interface {
	ResolveContext(context.Context, string) (string, error)
}

type Info struct {
	ID, Root, Provider, Version string
	Isolated, Owned, Retained   bool
	Capabilities                env.Capabilities
}
