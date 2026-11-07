package integration_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

type uncertainCommandEnvironment struct {
	fs    env.FileSystem
	calls atomic.Int32
}

func (e *uncertainCommandEnvironment) ID() string                    { return "uncertain-command" }
func (e *uncertainCommandEnvironment) Root() string                  { return "/managed/workspace" }
func (e *uncertainCommandEnvironment) FileSystem() env.FileSystem    { return contextualManagedFS{e.fs} }
func (e *uncertainCommandEnvironment) Executor() env.Executor        { return e }
func (e *uncertainCommandEnvironment) Isolated() bool                { return true }
func (e *uncertainCommandEnvironment) Close(context.Context) error   { return nil }
func (e *uncertainCommandEnvironment) Destroy(context.Context) error { return nil }
func (e *uncertainCommandEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Remote: true}
}
func (e *uncertainCommandEnvironment) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	return ctx.Err()
}
func (e *uncertainCommandEnvironment) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	return e.Run(ctx, c)
}
func (e *uncertainCommandEnvironment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	if e.calls.Add(1) == 1 {
		return env.Result{}, &sandbox.UnconfirmedError{SandboxID: e.ID(), CommandID: "effect-already-started", Cause: context.DeadlineExceeded}
	}
	return env.Result{Output: []byte("duplicate execution")}, nil
}

type uncertainMutationFS struct {
	env.FileSystem
	calls atomic.Int32
}

func (f *uncertainMutationFS) WriteFile(ctx context.Context, p string, data []byte, intent env.WriteIntent) (env.Version, error) {
	f.calls.Add(1)
	version, err := f.FileSystem.WriteFile(ctx, p, data, intent)
	if err != nil {
		return version, err
	}
	return version, &sandbox.UnconfirmedError{SandboxID: "uncertain-command", CommandID: "file-effect", Cause: context.DeadlineExceeded}
}

type contextualManagedFS struct{ env.FileSystem }

func (f contextualManagedFS) ResolveContext(ctx context.Context, p string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.Resolve(p)
}

type uncertainCommandProvider struct{ environment *uncertainCommandEnvironment }

func (p uncertainCommandProvider) Version() string { return "uncertain-test-v1" }
func (p uncertainCommandProvider) Create(context.Context, sandbox.Request) (sandbox.Environment, error) {
	return p.environment, nil
}
func (p uncertainCommandProvider) Connect(context.Context, string, sandbox.Request) (sandbox.Environment, error) {
	return p.environment, nil
}

// Exercise actual preset schema decoration, Dispatcher admission and durable
// recording. A second call after a lost remote response must not repeat an effect.
func TestManagedCommandUnconfirmedIsNotDurablyReexecuted(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"run_command", map[string]any{"command": "perform remote effect"}},
		{"git_commit", map[string]any{"message": "requested commit"}},
		{"write_file", map[string]any{"path": "effect.txt", "content": "already applied"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, err := env.NewLocalFS(t.TempDir())
			require.NoError(t, err)
			mutation := &uncertainMutationFS{FileSystem: fs}
			backend := &uncertainCommandEnvironment{fs: fs}
			count := &backend.calls
			if tc.name == "write_file" {
				backend.fs = mutation
				count = &mutation.calls
			}
			registry := &sandbox.Registry{Namespace: "durable-test", Providers: map[string]sandbox.Provider{"managed": uncertainCommandProvider{backend}}}
			set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Trusted: true, Data: []byte(`presets:
 - id: managed
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: managed, image: runtime}}
    - {id: c, use: coding-tools, config: {tools: [run_command, git_commit, write_file]}}
    - {id: p, use: permissions, config: {rules: [{tool: '*', decision: allow}]}}
`)})
			require.NoError(t, err)
			selected, ok := set.Get("managed")
			require.True(t, ok)
			asm, err := preset.Build(context.Background(), selected, preset.Host{Root: t.TempDir(), ArtifactDir: t.TempDir(), Sandboxes: registry})
			require.NoError(t, err)
			defer func() { require.NoError(t, asm.Close()); require.NoError(t, registry.Close(context.Background())) }()
			journalDir := t.TempDir()
			store, err := durable.NewFileStore(journalDir)
			require.NoError(t, err)
			ctx := durable.WithJournal(durable.WithExecutionKey(context.Background(), "managed-node"), store)
			req := tools.ToolRequest{Name: tc.name, Input: &interfaces.ToolInput{CallID: "stable-call", Args: tc.args}}
			out, err := asm.Dispatcher.Invoke(ctx, req)
			require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
			require.False(t, out != nil && out.Success)
			require.EqualValues(t, 1, count.Load())
			// Reopen persisted journal state to exercise recovery admission.
			store, err = durable.NewFileStore(journalDir)
			require.NoError(t, err)
			ctx = durable.WithJournal(durable.WithExecutionKey(context.Background(), "managed-node"), store)
			out, err = asm.Dispatcher.Invoke(ctx, req)
			require.EqualValues(t, 1, count.Load(), "an unconfirmed effect must not execute again")
			require.ErrorIs(t, err, agentErrors.ErrOperationAttention)
			require.False(t, out != nil && out.Success)
			require.False(t, errors.Is(err, context.DeadlineExceeded), "recovery needs host attention, not a confirmed timeout")
		})
	}
}
