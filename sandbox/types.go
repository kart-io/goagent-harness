// Package sandbox manages host-selected execution environments without binding
// the Harness root module to a sandbox vendor SDK.
package sandbox

import (
	"context"
	"errors"
	"fmt"

	"time"

	"github.com/kart-io/goagent/harness/env"
)

var ErrClosed = errors.New("sandbox session is closing or closed")
var ErrUnsupported = errors.New("sandbox capability is unsupported")
var ErrUnconfirmed = errors.New("sandbox operation outcome is unconfirmed")
var ErrOutputLimit = errors.New("sandbox output limit exceeded")

// ErrInstanceNotFound 表示 Provider 确认实例已不存在（如 TTL 到期或已被删除）。网络错误等查不准的情况不能返回它。
var ErrInstanceNotFound = errors.New("sandbox instance not found")

// ErrNotCreated 表示 Create 确认没有产生实例。Create 的其他错误都按结果未知处理：
// 响应丢失时实例可能已经存在，调用方会保留分配意图，之后再核对。
var ErrNotCreated = errors.New("sandbox instance not created")

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

// InstanceFinder is optionally implemented by providers that can look up an
// instance by the Scope given to Create. It lets a later process recover an
// instance allocated just before a crash, before anyone recorded its ID.
// found=false 必须是确定的结论：调用方会据此分配新实例或结束释放。
// 后端的列表接口有延迟或查询失败时，要返回错误，否则先前的实例会泄漏。
type InstanceFinder interface {
	FindByScope(context.Context, Request) (id string, found bool, err error)
}

// InstanceDestroyer is optionally implemented by providers that can delete an
// instance by ID without connecting. Without it Registry.Destroy has to Connect
// first, which fails for instances that are stopped or expired. Deleting an
// absent instance must succeed or return ErrInstanceNotFound.
type InstanceDestroyer interface {
	DestroyInstance(ctx context.Context, id string, r Request) error
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

// ErrCommandLost 表示按键附着时查不到命令的结果，它也不在运行：可能从未启动、被杀，或状态已丢失。
// 命令可能已经部分生效，调用方不能据此重跑。
var ErrCommandLost = errors.New("sandbox command lost")

// KeyedCommand 是按键执行的命令。
type KeyedCommand struct {
	// Key 是这次调用的稳定身份，同键只启动一次。
	Key     string
	Command env.Command
	// Timeout 由沙箱内部执行：宿主可能已经不在。
	Timeout time.Duration
	// MaxOutput 限制保留的输出字节数，超出不影响命令；0 表示默认值。
	MaxOutput int
	// AttachOnly 只附着不启动，查不到时返回 ErrCommandLost。
	AttachOnly bool
}

// KeyedResult 是按键命令的结果。
type KeyedResult struct {
	env.Result
	TimedOut bool
}

// KeyedExecutor 可选，让命令在宿主进程停止后继续运行，重启后取回结果。RunKeyed 的 ctx 结束时只断开、不终止命令；
// Environment.Close 也不能终止它们，Destroy 才会。CancelKeyed 终止命令，started 报告这个键是否启动过。
// 没有原生能力的后端可以用 CommandSupervisor。
type KeyedExecutor interface {
	RunKeyed(context.Context, KeyedCommand, Policy) (KeyedResult, error)
	CancelKeyed(ctx context.Context, key string, p Policy) (started bool, err error)
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

// MutationProtector optionally enforces excluded path component names at the
// actual mutation boundary, after any intermediate wrapper re-resolution.
// Returned views share the same environment and add immutable restrictions.
// This does not constrain commands; it protects FileSystem mutations only.
type MutationProtector interface {
	WithProtectedNames(names ...string) env.FileSystem
}

// PolicyExecutor enforces the complete invocation policy at the execution boundary.
// WorkspaceRoot identifies the trusted host authorization domain; the provider
// must bind that domain to this environment's workspace. Implementations must
// enforce mode and network for commands and descendants, reject unsupported
// mappings, and never treat isolation flags as evidence of enforcement.
// CheckPolicy must verify the boundary before admitting a workspace. RunWithPolicy
// must revalidate and enforce the supplied policy for every invocation.
type PolicyExecutor interface {
	CheckPolicy(context.Context, Policy) error
	RunWithPolicy(context.Context, env.Command, Policy) (env.Result, error)
}
