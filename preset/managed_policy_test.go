package preset

import (
	"context"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

type legacyManagedProvider struct{ sandbox.Provider }
type legacyManagedEnvironment struct{ sandbox.Environment }
type legacyManagedExecutor struct{ env.Executor }

func (e legacyManagedEnvironment) Executor() env.Executor {
	return legacyManagedExecutor{e.Environment.Executor()}
}
func (p legacyManagedProvider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	e, err := p.Provider.Create(ctx, r)
	if e == nil {
		return nil, err
	}
	return legacyManagedEnvironment{e}, err
}
func TestManagedPolicyRejectsIsolationClaimsWithoutEnforcement(t *testing.T) {
	host, p := sandboxHost(t)
	host.Sandboxes.Providers["fake"] = legacyManagedProvider{p}
	_, err := Build(context.Background(), mustGet(t, workspacePreset("{executor: sandbox, provider: fake, image: runtime}", "[run_command]")), host)
	require.ErrorIs(t, err, sandbox.ErrUnsupported)
	require.EqualValues(t, 1, p.environments["fixture-1"].destroyed.Load())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
func TestManagedPolicyHostCeilingBeforeAllocation(t *testing.T) {
	host, p := sandboxHost(t)
	host.SandboxCeiling = &sandbox.Policy{Mode: sandbox.ReadOnly, WorkspaceRoot: host.Root}
	_, err := Build(context.Background(), mustGet(t, workspacePreset("{executor: sandbox, provider: fake, image: runtime}", "[read_file]")), host)
	require.ErrorContains(t, err, "hard host ceiling")
	require.Empty(t, p.requests)
}
func TestManagedPolicyRejectsUnmappedHostResources(t *testing.T) {
	for _, config := range []string{
		"{executor: sandbox, provider: fake, image: runtime, envAllowlist: [SECRET]}",
		"{executor: sandbox, provider: fake, image: runtime, additionalWritableRoots: [ROOT]}",
	} {
		t.Run(config, func(t *testing.T) {
			host, p := sandboxHost(t)
			config = strings.ReplaceAll(config, "ROOT", strconv.Quote(t.TempDir()))
			_, err := Build(context.Background(), mustGet(t, workspacePreset(config, "[read_file]")), host)
			require.ErrorIs(t, err, sandbox.ErrUnsupported)
			require.Empty(t, p.requests)
		})
	}
}

func TestManagedPolicySingleCallFileApprovalAndLiveCommandMode(t *testing.T) {
	host, p := sandboxHost(t)
	asked := 0
	host.Approver = tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		asked++
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	})
	source := sandboxPreset("{executor: sandbox, provider: fake, image: runtime, sandbox: read-only, maxSandbox: workspace-write}", "[write_file, run_command]", "{rules: [{tool: '*', decision: allow}]}")
	a, err := Build(context.Background(), mustGet(t, source), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, a.Close()); require.NoError(t, host.Sandboxes.Close(context.Background())) }()
	ctx := tools.WithSessionID(context.Background(), "managed-session")
	call := func(name, id string, args map[string]any) *interfaces.ToolOutput {
		for _, tool := range a.Tools {
			if tool.Name() == name {
				out, err := tool.Invoke(ctx, &interfaces.ToolInput{CallID: id, Args: args})
				require.NoError(t, err)
				return out
			}
		}
		t.Fatalf("missing tool %s", name)
		return nil
	}
	require.False(t, call("write_file", "denied", map[string]any{"path": "denied.txt", "content": "bad"}).Success)
	require.True(t, call("write_file", "approved", map[string]any{"path": "approved.txt", "content": "ok", "sandbox_permissions": "workspace-write", "justification": "requested output"}).Success)
	require.False(t, call("write_file", "later", map[string]any{"path": "later.txt", "content": "bad"}).Success)
	require.Equal(t, 1, asked)
	require.True(t, call("run_command", "narrow", map[string]any{"command": "fixed response"}).Success)
	require.Equal(t, sandbox.ReadOnly, p.environments["fixture-1"].lastMode.Load())
	require.NoError(t, a.Permissions.SetSession(ctx, "managed-session", sandbox.WorkspaceWrite))
	require.True(t, call("run_command", "wide", map[string]any{"command": "fixed response"}).Success)
	require.Equal(t, sandbox.WorkspaceWrite, p.environments["fixture-1"].lastMode.Load())
	// The operation reached the bound managed filesystem, never the host root.
	_, err = os.Stat(filepath.Join(host.Root, "approved.txt"))
	require.True(t, os.IsNotExist(err))
	data, _, err := p.environments["fixture-1"].fs.ReadFile(ctx, "approved.txt")
	require.NoError(t, err)
	require.Equal(t, "ok", string(data))
}
