package preset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

func TestBoundarySkillMetadataCannotFollowOutsideSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, ".agents", "skills", "demo")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	file := filepath.Join(outside, "external.md")
	require.NoError(t, os.WriteFile(file, []byte("---\nname: demo\ndescription: demo\n---\nOUTSIDE_METADATA_SENTINEL"), 0o600))
	require.NoError(t, os.Symlink(file, filepath.Join(dir, "SKILL.md")))
	p := mustGet(t, project(`presets: [{id: p, components: [{id: s, use: skills}]}]`))
	asm, err := Build(context.Background(), p, Host{Root: root, ArtifactDir: t.TempDir()})
	if asm != nil {
		t.Cleanup(func() { require.NoError(t, asm.Close()) })
	}
	require.Error(t, err, "Build must reject skill metadata pointing outside its directory")
}

func TestBoundarySkillDirectoryCannotBeReboundAfterBuild(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	skills := filepath.Join(root, ".agents", "skills")
	writeSkill(t, skills, "demo", "name: demo\ndescription: demo", "Read ref.txt")
	require.NoError(t, os.WriteFile(filepath.Join(skills, "demo", "ref.txt"), []byte("ORIGINAL_REFERENCE"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "ref.txt"), []byte("OUTSIDE_REFERENCE_SENTINEL"), 0o600))
	asm, err := Build(context.Background(), mustGet(t, project(`presets: [{id: p, components: [{id: s, use: skills}]}]`)), Host{Root: root, ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	dir := filepath.Join(skills, "demo")
	require.NoError(t, os.Rename(dir, dir+"-original"))
	require.NoError(t, os.Symlink(outside, dir))
	out, err := byToolName(asm)[ToolLoadSkill].Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"name": "demo", "file": "ref.txt"}})
	require.NoError(t, err)
	require.True(t, out.Success)
	require.Equal(t, "ORIGINAL_REFERENCE", out.Result.(map[string]any)["content"], "load_skill must retain the original directory")
}

func TestBoundarySkillReferencesStayConfined(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	skills := filepath.Join(root, ".agents", "skills")
	writeSkill(t, skills, "demo", "name: demo\ndescription: demo", "Read ref.txt")
	dir := filepath.Join(skills, "demo")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ref.txt"), []byte("inside"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600))
	require.NoError(t, os.Symlink("ref.txt", filepath.Join(dir, "inside-link")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "outside-link")))
	asm, err := Build(context.Background(), mustGet(t, project(`presets: [{id: p, components: [{id: s, use: skills}]}]`)), Host{Root: root, ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	tool := byToolName(asm)[ToolLoadSkill]
	out, err := tool.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"name": "demo", "file": "inside-link"}})
	require.NoError(t, err)
	require.True(t, out.Success)
	require.Equal(t, "inside", out.Result.(map[string]any)["content"])
	// Opaque os.Root errors propagate as execution failures rather than being
	// mislabeled as completed "missing file" rejections; access stays denied.
	out, err = tool.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"name": "demo", "file": "outside-link"}})
	require.Error(t, err)
	var pathError *os.PathError
	require.ErrorAs(t, err, &pathError)
	require.Equal(t, "outside-link", pathError.Path)
	if out != nil {
		require.False(t, out.Success)
		require.Nil(t, out.Result)
	}
	for _, file := range []string{"../demo/ref.txt", filepath.Join(outside, "secret.txt")} {
		out, err = tool.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"name": "demo", "file": file}})
		require.NoError(t, err)
		_, rejected := tools.RejectedResult(out)
		require.True(t, rejected, "unsafe file %q was accepted", file)
	}
	require.NoError(t, asm.Close())
	out, err = tool.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"name": "demo", "file": "ref.txt"}})
	if err == nil {
		_, rejected := tools.RejectedResult(out)
		require.True(t, rejected, "closed assembly must release skill directory handles")
	}
}

func TestBoundarySkillDiscoveryCannotFollowOutsideDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeSkill(t, outside, "demo", "name: demo\ndescription: demo", "outside")
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".agents"), 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, ".agents", "skills")))
	asm, err := Build(context.Background(), mustGet(t, project(`presets: [{id: p, components: [{id: s, use: skills}]}]`)), Host{Root: root, ArtifactDir: t.TempDir()})
	if asm != nil {
		t.Cleanup(func() { require.NoError(t, asm.Close()) })
	}
	require.Error(t, err, "relative skill discovery must stay within the workspace")
}

func TestBoundaryLoadRejectsTrailingYAMLDocument(t *testing.T) {
	for _, trailing := range []string{"presets: [{id: ignored}]", "malformed: ["} {
		t.Run(trailing, func(t *testing.T) {
			_, err := Load(project("presets: [{id: p}]\n---\n" + trailing))
			require.Error(t, err, "a second YAML document must not be silently discarded")
		})
	}
}
