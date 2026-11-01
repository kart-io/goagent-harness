package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealDockerLifecycleAndWorkspace(t *testing.T) {
	domain := os.Getenv("HARNESS_SANDBOX_DOMAIN")
	if domain == "" {
		t.Skip("run scripts/verify-sandbox.py for an isolated real Docker service")
	}
	p, err := New(Options{Connection: sdk.ConnectionConfig{Domain: domain, Protocol: "http", APIKey: os.Getenv("HARNESS_SANDBOX_API_KEY"), UseServerProxy: true}, MaxOutputBytes: 4096})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	r := sandbox.Request{Provider: "opensandbox", Scope: "real-test", Image: os.Getenv("HARNESS_SANDBOX_IMAGE"), Network: true}
	e, err := p.Create(ctx, r)
	if e != nil {
		t.Cleanup(func() {
			clean, c := context.WithTimeout(context.Background(), 20*time.Second)
			defer c()
			require.NoError(t, e.Destroy(clean))
		})
	}
	require.NoError(t, err)
	t.Log("owned sandbox", e.ID())
	f := e.FileSystem()
	data := []byte(strings.Repeat("binary\x00文字", 20000))
	v, err := f.WriteFile(ctx, "nested/input.bin", data, env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)
	got, version, err := f.ReadFile(ctx, "nested/input.bin")
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.Equal(t, v, version)
	_, err = f.WriteFile(ctx, "nested/input.bin", nil, env.WriteIntent{IfVersion: "stale"})
	require.ErrorIs(t, err, env.ErrStale)
	args := []string{"space value", "$(touch injected)", "semi;colon", "中文"}
	cmd := append([]string{"python3", "-c", "import sys,json; print(json.dumps(sys.argv[1:]))"}, args...)
	result, err := e.Executor().Run(ctx, env.Command{Argv: cmd})
	require.NoError(t, err)
	var actual []string
	require.NoError(t, json.Unmarshal(result.Output, &actual))
	require.Equal(t, args, actual)
	result, err = e.Executor().Run(ctx, env.Command{Argv: []string{"python3", "-c", "import pathlib,hashlib,sys; print(hashlib.sha256(pathlib.Path('input.bin').read_bytes()).hexdigest());sys.exit(7)"}, Dir: "nested"})
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, string(v), strings.TrimSpace(string(result.Output)))
	_, err = e.Executor().Run(ctx, env.Command{Argv: []string{"python3", "-c", "import os;os.symlink('/etc','escape')"}})
	require.NoError(t, err)
	_, err = f.Resolve("escape/passwd")
	require.ErrorIs(t, err, env.ErrOutsideWorkspace)
	require.NoError(t, f.Remove(ctx, "escape"))
	// Cancel only after the actual process has recorded its PID.
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := e.Executor().Run(runCtx, env.Command{Argv: []string{"python3", "-c", "import os,pathlib,time;pathlib.Path('pid').write_text(str(os.getpid()));time.sleep(60)"}})
		done <- err
	}()
	var pid string
	require.Eventually(t, func() bool {
		b, _, err := f.ReadFile(ctx, "pid")
		if err == nil {
			pid = string(b)
		}
		return err == nil
	}, 10*time.Second, 50*time.Millisecond)
	stop()
	require.ErrorIs(t, <-done, context.Canceled)
	result, err = e.Executor().Run(ctx, env.Command{Argv: []string{"python3", "-c", fmt.Sprintf("import os,sys\ntry: os.kill(%s,0)\nexcept ProcessLookupError: sys.exit(0)\nsys.exit(1)", pid)}})
	require.NoError(t, err)
	require.Zero(t, result.ExitCode, "cancelled PID must no longer exist")
	_, err = e.Executor().Run(ctx, env.Command{Argv: []string{"python3", "-c", "import os,time\nwhile True: os.write(1,b'x'*4096);time.sleep(.001)"}})
	require.ErrorIs(t, err, sandbox.ErrOutputLimit)
	require.NoError(t, e.Close(ctx))
	connected, err := p.Connect(ctx, e.ID(), r)
	require.NoError(t, err)
	got, _, err = connected.FileSystem().ReadFile(ctx, "nested/input.bin")
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.NoError(t, connected.Close(ctx))
	other, err := p.Create(ctx, r)
	if other != nil {
		t.Cleanup(func() {
			clean, c := context.WithTimeout(context.Background(), 20*time.Second)
			defer c()
			require.NoError(t, other.Destroy(clean))
		})
	}
	require.NoError(t, err)
	require.NotEqual(t, e.ID(), other.ID())
	_, _, err = other.FileSystem().ReadFile(ctx, "nested/input.bin")
	require.Error(t, err)
	t.Log("independent sandbox", other.ID())
}

func TestRealDockerPresetAssembly(t *testing.T) {
	domain := os.Getenv("HARNESS_SANDBOX_DOMAIN")
	if domain == "" {
		t.Skip("requires isolated Docker service")
	}
	p, err := New(Options{Connection: sdk.ConnectionConfig{Domain: domain, Protocol: "http", APIKey: os.Getenv("HARNESS_SANDBOX_API_KEY"), UseServerProxy: true}})
	require.NoError(t, err)
	registry := &sandbox.Registry{Namespace: "preset-validation", Providers: map[string]sandbox.Provider{"opensandbox": p}}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		require.NoError(t, registry.Close(ctx))
	})
	data := fmt.Sprintf(`presets: [{id: remote-coder, components: [{id: workspace, use: workspace, config: {executor: sandbox, provider: opensandbox, image: %q, sandbox: workspace-write, network: true, commandTimeout: 30s, maxOutputBytes: 1048576}}, {id: coding, use: coding-tools, config: {tools: [read_file, write_file, edit_file, glob, grep, run_command]}}, {id: permissions, use: permissions, config: {base: coding}}]}]`, os.Getenv("HARNESS_SANDBOX_IMAGE"))
	set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Path: "validation", Trusted: true, Data: []byte(data)})
	require.NoError(t, err)
	selected, ok := set.Get("remote-coder")
	require.True(t, ok)
	assembly, err := preset.Build(context.Background(), selected, preset.Host{Root: t.TempDir(), ArtifactDir: t.TempDir(), Sandboxes: registry})
	require.NoError(t, err)
	t.Log("preset sandbox", assembly.Manifest.Workspace.SandboxID)
	t.Cleanup(func() { require.NoError(t, assembly.Close()) })
	out, err := assembly.Dispatcher.Invoke(context.Background(), tools.ToolRequest{Name: "write_file", Input: &interfaces.ToolInput{Args: map[string]interface{}{"path": "nested/result.txt", "content": "remote preset"}}})
	require.NoError(t, err)
	require.True(t, out.Success, "%+v", out)
	out, err = assembly.Dispatcher.Invoke(context.Background(), tools.ToolRequest{Name: "read_file", Input: &interfaces.ToolInput{Args: map[string]interface{}{"path": "nested/result.txt"}}})
	require.NoError(t, err)
	require.True(t, out.Success)
	require.Contains(t, fmt.Sprint(out.Result), "remote preset")
	require.NoError(t, assembly.Close())
}
