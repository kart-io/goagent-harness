package preset

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

// A delegated child must not acquire networking merely by choosing another preset.
func TestChildCannotWidenNetwork(t *testing.T) {
	src := trusted(`presets:
  - id: lead
    components:
      - {id: w, use: workspace, config: {executor: docker, image: alpine:3}}
      - {id: sub, use: subagent, config: {preset: worker}}
  - id: worker
    components:
      - {id: w, use: workspace, config: {executor: docker, image: alpine:3, network: true}}
      - {id: tools, use: coding-tools}
`)
	_, err := Build(context.Background(), mustGetID(t, "lead", src), Host{Root: t.TempDir(), RunAgent: func(context.Context, *Assembly, string) (string, error) { return "", nil }})
	require.Error(t, err, "child networking exceeds parent ceiling")
}

func TestAdditionalRootsRequireProjectTrust(t *testing.T) {
	extra := t.TempDir()
	src := Source{Layer: LayerProject, Data: []byte(fmt.Sprintf(`presets:
  - id: p
    components:
      - {id: w, use: workspace, config: {executor: docker, image: alpine:3, additionalWritableRoots: [%q]}}
`, extra))}
	set, err := Load(src)
	require.NoError(t, err)
	p, ok := set.Get("p")
	require.True(t, ok)
	_, err = Build(context.Background(), p, Host{Root: t.TempDir()})
	var trust *TrustRequiredError
	require.True(t, errors.As(err, &trust), "extra writable roots must be explicit expansions: %v", err)
}

func TestReviewerRoleCannotWriteOrExecute(t *testing.T) {
	src := sandboxPreset("{sandbox: danger-full-access, executor: local, network: true}", "[]", "{role: reviewer}")
	root := t.TempDir()
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: root})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "new.txt", "content": "bad"}},
		{"run_command", map[string]any{"command": "touch new.txt"}},
	} {
		var out *interfaces.ToolOutput
		for _, tool := range asm.Tools {
			if tool.Name() == tc.tool {
				out, err = tool.Invoke(context.Background(), &interfaces.ToolInput{Args: tc.args})
				require.NoError(t, err)
			}
		}
		require.NotNil(t, out)
		require.False(t, out.Success)
	}
	_, err = os.Stat(filepath.Join(root, "new.txt"))
	require.True(t, os.IsNotExist(err))
}

func TestNativeReadOnlyAndSingleCallFileEscalation(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("native backend unavailable on this platform")
	}
	root := t.TempDir()
	src := sandboxPreset("{sandbox: read-only, maxSandbox: workspace-write, executor: native}", "[write_file]", "{rules: [{tool: write_file, decision: allow}]}")
	asked := 0
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: root, AgentID: "coder", Approver: tools.ApproverFunc(func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
		asked++
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	})})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	ctx := tools.WithSessionID(context.Background(), "session")
	write := func(id string, args map[string]any) *interfaces.ToolOutput {
		out, err := asm.Tools[0].Invoke(ctx, &interfaces.ToolInput{CallID: id, Args: args})
		require.NoError(t, err)
		return out
	}
	require.False(t, write("denied", map[string]any{"path": "a.txt", "content": "bad"}).Success)
	require.True(t, write("approved", map[string]any{"path": "a.txt", "content": "ok", "sandbox_permissions": "workspace-write", "justification": "Create requested file"}).Success)
	require.False(t, write("next", map[string]any{"path": "b.txt", "content": "bad"}).Success)
	require.Equal(t, 1, asked)
	data, err := os.ReadFile(filepath.Join(root, "a.txt"))
	require.NoError(t, err)
	require.Equal(t, "ok", string(data))
	_, err = os.Stat(filepath.Join(root, "b.txt"))
	require.True(t, os.IsNotExist(err))
}

func sandboxPreset(config, names, perms string) Source {
	return trusted(`presets: [{id: p, components: [{id: w, use: workspace, config: ` + config + `}, {id: c, use: coding-tools, config: {tools: ` + names + `}}, {id: permissions, use: permissions, config: ` + perms + `}]}]`)
}

// A host can narrow a live session and the model must retain a sanctioned way
// to request one call at the deployment's permitted wider mode.
func TestSessionNarrowingRetainsEscalationSchema(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, workspacePreset("{executor: docker, image: alpine:3}", "[write_file]")), Host{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	require.NoError(t, asm.Permissions.SetSession(context.Background(), "session", sandbox.ReadOnly))
	require.Contains(t, asm.Tools[0].ArgsSchema(), "sandbox_permissions")
}

func TestHostCeilingCannotBeApprovedAway(t *testing.T) {
	root := t.TempDir()
	ceiling := sandbox.Policy{Mode: sandbox.ReadOnly, WorkspaceRoot: root}
	_, err := Build(context.Background(), mustGet(t, workspacePreset("{executor: docker, image: alpine:3}", "[]")), Host{Root: root, SandboxCeiling: &ceiling})
	require.ErrorContains(t, err, "hard host ceiling")
}

func TestNeverApprovalAlsoDeniesSandboxEscalation(t *testing.T) {
	root := t.TempDir()
	src := sandboxPreset("{executor: docker, sandbox: read-only, maxSandbox: workspace-write, image: alpine:3}", "[write_file]", "{approval: never, rules: [{tool: write_file, decision: allow}]}")
	asks := 0
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: root, Approver: tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
		asks++
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	})})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	out, err := asm.Tools[0].Invoke(tools.WithSessionID(context.Background(), "s"), &interfaces.ToolInput{CallID: "c", Args: map[string]any{"path": "bad", "content": "bad", "sandbox_permissions": "workspace-write", "justification": "try"}})
	require.NoError(t, err)
	require.False(t, out.Success)
	require.Zero(t, asks)
	_, err = os.Stat(filepath.Join(root, "bad"))
	require.True(t, os.IsNotExist(err))
}

func TestRuntimePromptReflectsHostSessionPolicy(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, workspacePreset("{executor: docker, image: alpine:3}", "[write_file]")), Host{Root: t.TempDir(), AgentID: "worker"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	require.NoError(t, asm.Permissions.SetSession(context.Background(), "s", sandbox.ReadOnly))
	var fact string
	for _, provider := range asm.Loop.PromptContexts {
		if provider.Name == "sandbox-policy" {
			fact = provider.Text(builder.PromptScope{SessionID: "s"})
		}
	}
	require.Contains(t, fact, `"mode":"read-only"`)
	require.Contains(t, fact, `"network":"deny"`)
	require.Contains(t, fact, "worker")
}

func TestChildCannotInheritAdditionalSecretEnvironment(t *testing.T) {
	src := trusted(`presets:
  - id: lead
    components:
      - {id: w, use: workspace, config: {executor: docker, image: alpine:3}}
      - {id: sub, use: subagent, config: {preset: worker}}
  - id: worker
    components:
      - {id: w, use: workspace, config: {executor: docker, image: alpine:3, envAllowlist: [CHILD_SECRET]}}
      - {id: tools, use: coding-tools}
`)
	_, err := Build(context.Background(), mustGetID(t, "lead", src), Host{Root: t.TempDir(), RunAgent: func(context.Context, *Assembly, string) (string, error) { return "", nil }})
	require.Error(t, err, "delegation cannot add inherited credential variables")
}

func TestExtraScopeLinterUsesExecutionPath(t *testing.T) {
	extra, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	config := fmt.Sprintf(`{sandbox: danger-full-access, executor: local, network: true, additionalWritableRoots: [%q], linters: [{ext: .go, command: [sh, -c, 'test -f "$1"', lint]}]}`, extra)
	asm, err := Build(context.Background(), mustGet(t, sandboxPreset(config, "[write_file]", "{base: coding}")), Host{Root: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	out := invokeTool(t, asm, "write_file", map[string]any{"path": "@root1/new.go", "content": "package demo\n"})
	require.True(t, out.Success, "%+v", out)
	data, err := os.ReadFile(filepath.Join(extra, "new.go"))
	require.NoError(t, err)
	require.Equal(t, "package demo\n", string(data))
}

func TestManifestBindsAgentIdentityButNotSessionRevision(t *testing.T) {
	p := mustGet(t, workspacePreset("{executor: docker, image: alpine:3}", "[write_file]"))
	host := Host{Root: t.TempDir(), AgentID: "first-worker"}
	first, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Close() })
	initial := first.Manifest.Digest()
	require.NoError(t, first.Permissions.SetSession(context.Background(), "s", sandbox.ReadOnly))
	require.Equal(t, initial, first.Manifest.Digest(), "mutable session versions must not alter assembly identity")
	host.AgentID = "second-worker"
	second, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	require.NotEqual(t, initial, second.Manifest.Digest(), "another trusted Agent identity must not replay the same assembly scope")
}

// bwrap 在宿主上用不了时回退到 Landlock，恢复契约要记录它，避免恢复时悄悄换成另一种隔离。
func TestNativeFallbackRecordedInContract(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("只有 Linux 有 Landlock")
	}
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "bwrap"), []byte("#!/bin/sh\necho 'bwrap: setting up uid map: Permission denied' >&2\nexit 1\n"), 0o700))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := (&sandbox.Executor{Backend: "native", Runner: sandbox.RunnerLandlock, Policy: sandbox.Policy{WorkspaceRoot: t.TempDir(), Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny}}).Probe(context.Background()); err != nil {
		t.Skip(err)
	}
	src := sandboxPreset("{sandbox: workspace-write, maxSandbox: workspace-write, executor: native}", "[write_file]", "{rules: [{tool: write_file, decision: allow}]}")
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: t.TempDir(), AgentID: "coder"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = asm.Close() })
	require.Contains(t, asm.Manifest.Workspace.Contract, ";runner="+sandbox.RunnerLandlock)
}

// workspace 缺省使用 native；只写 image 的旧配置报错并提示改写 executor: docker。
func TestWorkspaceDefaultsToNative(t *testing.T) {
	p := mustGet(t, workspacePreset("{}", "[]"))
	var cfg *WorkspaceConfig
	for _, row := range p.Rows {
		if c, ok := row.config.(*WorkspaceConfig); ok {
			cfg = c
		}
	}
	require.NotNil(t, cfg)
	require.Equal(t, "native", cfg.Executor)
	_, err := Load(workspacePreset("{image: alpine:3}", "[]"))
	require.ErrorContains(t, err, "executor: docker")
}
