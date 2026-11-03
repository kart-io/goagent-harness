package preset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func buildEnvContext(t *testing.T, config string) (*Assembly, string) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644))
	require.NoError(t, os.Mkdir(filepath.Join(root, "internal"), 0o755))
	src := trusted(`presets: [{id: p, components: [` + localWorkspace + `, {id: e, use: env-context` + config + `}]}]`)
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: root, ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	return asm, root
}

func TestEnvContextAddsSnapshotToSystemPrompt(t *testing.T) {
	asm, _ := buildEnvContext(t, `, config: {tools: [git]}`)
	require.Contains(t, asm.SystemPrompt, "工作区环境")
	require.Contains(t, asm.SystemPrompt, "os: ")
	require.Contains(t, asm.SystemPrompt, "git: git version")
	require.Contains(t, asm.SystemPrompt, "\ngo.mod")
	require.Contains(t, asm.SystemPrompt, "\ninternal/")
}

func TestEnvContextEmptyToolsSkipsToolchains(t *testing.T) {
	asm, _ := buildEnvContext(t, `, config: {tools: []}`)
	require.Contains(t, asm.SystemPrompt, "os: ")
	require.NotContains(t, asm.SystemPrompt, "git: git version")
}

func TestEnvContextProbeDisabled(t *testing.T) {
	asm, _ := buildEnvContext(t, `, config: {probe: false}`)
	require.NotContains(t, asm.SystemPrompt, "os: ")
	require.Contains(t, asm.SystemPrompt, "\ngo.mod")
}

func TestEnvContextValidation(t *testing.T) {
	for _, cfg := range []string{`{maxEntries: -1}`, `{tools: ["go; rm -rf /"]}`} {
		_, err := Load(trusted(`presets: [{id: p, components: [` + localWorkspace + `, {id: e, use: env-context, config: ` + cfg + `}]}]`))
		require.Error(t, err, cfg)
	}
}

func TestEnvContextRequiresWorkspace(t *testing.T) {
	src := trusted(`presets: [{id: p, components: [{id: e, use: env-context}]}]`)
	_, err := Load(src)
	if err == nil {
		_, err = Build(context.Background(), mustGet(t, src), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	}
	require.Error(t, err)
}

// Docker 执行器下探测在容器内运行，看到的是容器的系统与工具链。
func TestEnvContextProbesInsideDocker(t *testing.T) {
	image := os.Getenv("GOAGENT_CODING_DOCKER_IMAGE")
	if image == "" {
		t.Skip("未设置 GOAGENT_CODING_DOCKER_IMAGE，跳过 Docker 用例")
	}
	src := trusted(`presets: [{id: p, components: [{id: w, use: workspace, config: {executor: docker, image: "` + image + `", sandbox: read-only}}, {id: e, use: env-context, config: {tools: [sh, apk]}}]}]`)
	asm, err := Build(context.Background(), mustGet(t, src), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	require.Contains(t, asm.SystemPrompt, "os: Linux")
	require.Contains(t, asm.SystemPrompt, "apk: apk-tools", "容器内的 Alpine 包管理器")
	require.NotContains(t, asm.SystemPrompt, "sh: ", "没有版本输出的工具不列出")
}
