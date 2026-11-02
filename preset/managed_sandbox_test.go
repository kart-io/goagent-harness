package preset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

type sandboxFixtureEnvironment struct {
	id                string
	fs                env.FileSystem
	network, isolated bool
	destroyed, closed atomic.Int32
	lastMode          atomic.Value
}

func (e *sandboxFixtureEnvironment) ID() string                 { return e.id }
func (e *sandboxFixtureEnvironment) Root() string               { return "/remote/workspace" }
func (e *sandboxFixtureEnvironment) FileSystem() env.FileSystem { return e.fs }
func (e *sandboxFixtureEnvironment) Executor() env.Executor     { return e }
func (e *sandboxFixtureEnvironment) Isolated() bool             { return e.isolated }
func (e *sandboxFixtureEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Network: e.network}
}
func (e *sandboxFixtureEnvironment) Run(ctx context.Context, _ env.Command) (env.Result, error) {
	return env.Result{Output: []byte("fixture")}, ctx.Err()
}
func (e *sandboxFixtureEnvironment) Close(context.Context) error   { e.closed.Add(1); return nil }
func (e *sandboxFixtureEnvironment) Destroy(context.Context) error { e.destroyed.Add(1); return nil }

type sandboxFixtureProvider struct {
	t  *testing.T
	mu sync.Mutex
	// networkOverride 让实例的网络能力与请求不符。
	networkOverride bool
	requests        []sandbox.Request
	environments    map[string]*sandboxFixtureEnvironment
	isolated        bool
}

func (*sandboxFixtureProvider) Version() string { return "fixture-v1" }
func (p *sandboxFixtureProvider) Create(_ context.Context, r sandbox.Request) (sandbox.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	root := p.t.TempDir()
	fs, err := env.NewLocalFS(root)
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(root, "remote.txt"), []byte("only remote data"), 0600); err != nil {
		return nil, err
	}
	e := &sandboxFixtureEnvironment{id: fmt.Sprintf("fixture-%d", len(p.environments)+1), fs: fs, network: r.Network != p.networkOverride, isolated: p.isolated}
	p.environments[e.id] = e
	p.requests = append(p.requests, r)
	return e, nil
}
func (p *sandboxFixtureProvider) Connect(_ context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r)
	return p.environments[id], nil
}
func sandboxHost(t *testing.T) (Host, *sandboxFixtureProvider) {
	t.Helper()
	p := &sandboxFixtureProvider{t: t, environments: map[string]*sandboxFixtureEnvironment{}, isolated: true}
	registry := &sandbox.Registry{Namespace: "job-1", Providers: map[string]sandbox.Provider{"fake": p}}
	return Host{Root: t.TempDir(), ArtifactDir: t.TempDir(), Sandboxes: registry}, p
}

func TestSandboxWorkspaceBindsRemoteFilesAndCleanup(t *testing.T) {
	host, provider := sandboxHost(t)
	p := mustGet(t, workspacePreset("{executor: sandbox, provider: fake, image: runtime}", "[read_file]"))
	a, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	require.Equal(t, "/remote/workspace", a.Manifest.Workspace.Root)
	text, err := a.Workspace.ProjectInstructionsFrom(context.Background(), "remote.txt")
	require.NoError(t, err)
	require.Contains(t, text, "only remote data")
	require.NotEmpty(t, a.Manifest.Components[0].Info["sandbox_id"])
	require.NoError(t, a.Close())
	require.EqualValues(t, 1, provider.environments["fixture-1"].destroyed.Load())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
func TestSandboxWorkspaceFailureReclaimsAllocatedEnvironment(t *testing.T) {
	for _, isolation := range []bool{false, true} {
		t.Run(fmt.Sprint(isolation), func(t *testing.T) {
			host, provider := sandboxHost(t)
			provider.isolated = isolation
			source := trusted(`presets: [{id: p, components: [{id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}, {id: fail, use: test-probe, config: {fail: true}}]}]`)
			_, err := Build(context.Background(), mustGet(t, source), host)
			require.Error(t, err)
			require.EqualValues(t, 1, provider.environments["fixture-1"].destroyed.Load())
			require.NoError(t, host.Sandboxes.Close(context.Background()))
		})
	}
}
func TestSandboxReadOnlyRemovesCommandsWithoutReadonlyRoot(t *testing.T) {
	host, _ := sandboxHost(t)
	p := mustGet(t, workspacePreset("{executor: sandbox, provider: fake, image: runtime, sandbox: read-only}", "[]"))
	a, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	require.NotContains(t, toolNames(a), "run_command")
	require.NotContains(t, toolNames(a), "write_file")
	require.NoError(t, a.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
func TestSandboxManifestRequiresSameRetainedEnvironment(t *testing.T) {
	host, _ := sandboxHost(t)
	host.Sandboxes.Resolve = func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{Retain: true}, nil }
	p := mustGet(t, workspacePreset("{executor: sandbox, provider: fake, image: runtime}", "[read_file]"))
	a, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	firstDigest, id := a.Manifest.Digest(), a.Manifest.Components[0].Info["sandbox_id"]
	require.NoError(t, a.Close())
	host.Sandboxes.Resolve = func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{ID: id}, nil }
	same, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	require.Equal(t, firstDigest, same.Manifest.Digest())
	require.NoError(t, same.Close())
	host.Sandboxes.Resolve = nil
	fresh, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	require.NotEqual(t, firstDigest, fresh.Manifest.Digest())
	require.NoError(t, fresh.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
func TestChildSandboxExpansionRejectedBeforeAllocation(t *testing.T) {
	for _, parent := range []string{"{executor: docker, image: unused}", "{executor: local, sandbox: read-only}"} {
		t.Run(parent, func(t *testing.T) {
			host, provider := sandboxHost(t)
			host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "unused", nil }
			childNetwork := "true"
			if parent == "{executor: local, sandbox: read-only}" {
				childNetwork = "false"
			}
			p := mustGet(t, trusted(`presets:
 - id: p
   components:
    - {id: w, use: workspace, config: `+parent+`}
    - {id: delegate, use: subagent, config: {preset: child}}
 - id: child
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime, network: `+childNetwork+`}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
 `))
			_, err := Build(context.Background(), p, host)
			require.Error(t, err)
			require.Empty(t, provider.requests)
		})
	}
}
func TestChildSandboxScopesAreDistinctForSamePreset(t *testing.T) {
	host, provider := sandboxHost(t)
	host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "unused", nil }
	p := mustGet(t, trusted(`presets:
 - id: p
   components:
    - {id: delegate_a, use: subagent, config: {preset: child, toolName: worker_a}}
    - {id: delegate_b, use: subagent, config: {preset: child, toolName: worker_b}}
 - id: child
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
 `))
	a, err := Build(context.Background(), p, host)
	require.NoError(t, err)
	require.Len(t, provider.requests, 2)
	require.NotEqual(t, provider.requests[0].Scope, provider.requests[1].Scope)
	require.NoError(t, a.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
func TestSandboxConfigCannotChooseConnectionIDOrUnknownProvider(t *testing.T) {
	_, err := Load(workspacePreset("{executor: sandbox, provider: fake, image: runtime, sandboxID: supplied-by-model}", "[]"))
	require.Error(t, err)
	host, provider := sandboxHost(t)
	p := mustGet(t, workspacePreset("{executor: sandbox, provider: unknown, image: runtime}", "[read_file]"))
	_, err = Build(context.Background(), p, host)
	require.Error(t, err)
	require.Empty(t, provider.requests)
}

func TestReviewGrandchildCannotLoseAncestorCeilings(t *testing.T) {
	for _, mode := range []string{"sandbox", "network"} {
		t.Run(mode, func(t *testing.T) {
			host, provider := sandboxHost(t)
			defer func() { require.NoError(t, host.Sandboxes.Close(context.Background())) }()
			host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "", nil }
			parentSandbox, network := "read-only", true
			if mode == "network" {
				parentSandbox = "workspace-write"
				network = false
			}
			p := mustGet(t, trusted(fmt.Sprintf(`presets:
 - id: p
   components:
    - {id: w, use: workspace, config: {executor: docker, image: unused, sandbox: %s, network: %t}}
    - {id: d, use: subagent, config: {preset: middle, maxDepth: 3}}
 - id: middle
   components:
    - {id: d, use: subagent, config: {preset: leaf, maxDepth: 3}}
 - id: leaf
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime, sandbox: workspace-write, network: true}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
 `, parentSandbox, network)))
			a, err := Build(context.Background(), p, host)
			if a != nil {
				defer func() { require.NoError(t, a.Close()) }()
			}
			require.Error(t, err)
			require.Empty(t, provider.requests)
		})
	}
}

// The fixture executes only a fixed response; unlike a general command backend
// it cannot mutate files, escape roots or access a network. Assert the policy
// contract explicitly instead of relying on its legacy Isolated flag.
func (e *sandboxFixtureEnvironment) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.AdditionalWritableRoots) != 0 || (p.Network == sandbox.NetworkInherit) != e.network {
		return sandbox.ErrUnsupported
	}
	return nil
}
func (e *sandboxFixtureEnvironment) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return env.Result{}, err
	}
	e.lastMode.Store(p.Mode)
	return e.Run(ctx, c)
}
