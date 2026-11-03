package coding

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

func TestEnvironmentSnapshot(t *testing.T) {
	w := newTestWorkspace(t)
	root := w.Root()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "cmd", "app"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env.example"), []byte("A=1\n"), 0o644))
	mustSucceed(t, w, ToolRunCommand, map[string]any{"command": "git init -q -b feat && git add go.mod && git -c user.name=t -c user.email=t@t commit -qm init && echo x > dirty.txt"})

	snap, err := w.Environment(context.Background(), EnvironmentOptions{Tools: []string{"sh", "git", "no-such-tool-xyz"}})
	require.NoError(t, err)
	require.Contains(t, snap.Entries, "cmd/")
	require.Contains(t, snap.Entries, "go.mod")
	require.Contains(t, snap.Entries, ".env.example")
	require.NotContains(t, snap.Entries, ".git/", "不列出 .git")
	require.Contains(t, snap.Probe, "os: ")
	require.Contains(t, snap.Probe, "git: 分支 feat，2 个未提交改动")
	require.Contains(t, snap.Probe, "git: git version")
	require.NotContains(t, snap.Probe, "no-such-tool-xyz")
	require.NotContains(t, snap.Probe, "sh: ", "不支持 --version 的工具不列出")
}

func TestEnvironmentLimitsEntries(t *testing.T) {
	w := newTestWorkspace(t)
	for i := range 5 {
		require.NoError(t, os.WriteFile(filepath.Join(w.Root(), fmt.Sprintf("f%d", i)), nil, 0o644))
	}
	snap, err := w.Environment(context.Background(), EnvironmentOptions{MaxEntries: 3})
	require.NoError(t, err)
	require.Equal(t, []string{"f0", "f1", "f2"}, snap.Entries)
	require.Equal(t, 2, snap.OmittedEntries)
}

func TestEnvironmentRejectsUnsafeToolNames(t *testing.T) {
	w := newTestWorkspace(t)
	_, err := w.Environment(context.Background(), EnvironmentOptions{Tools: []string{"go; rm -rf /"}})
	require.Error(t, err)
}

type failingExecutor struct{ env.Executor }

func (failingExecutor) Run(context.Context, env.Command) (env.Result, error) {
	return env.Result{}, errors.New("sandbox unavailable")
}

// 探测失败只省略探测结果，目录列表照常返回。
func TestEnvironmentProbeFailureIsNotFatal(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), nil, 0o644))
	w, err := NewWorkspace(root, WithExecutor(failingExecutor{env.LocalExecutor{Root: root}}))
	require.NoError(t, err)
	snap, err := w.Environment(context.Background(), EnvironmentOptions{})
	require.NoError(t, err)
	require.Equal(t, []string{"a.txt"}, snap.Entries)
	require.Empty(t, snap.Probe)
}

func TestEnvironmentHonorsCanceledContext(t *testing.T) {
	w := newTestWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Environment(ctx, EnvironmentOptions{})
	require.ErrorIs(t, err, context.Canceled)
}
