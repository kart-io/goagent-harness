package preset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent/core/durable"
	"github.com/stretchr/testify/require"
)

func TestSkillsManifestTracksRawMetadata(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".agents", "skills")
	p := mustGet(t, trusted(`presets: [{id: p, components: [{id: s, use: skills}]}]`))
	build := func(version string) *Assembly {
		writeSkill(t, dir, "demo", "name: demo\ndescription: demo\nmetadata:\n  version: \""+version+"\"", "same instructions")
		asm, err := Build(context.Background(), p, Host{Root: root, ArtifactDir: t.TempDir()})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, asm.Close()) })
		raw, err := os.ReadFile(filepath.Join(dir, "demo", "SKILL.md"))
		require.NoError(t, err)
		require.Equal(t, "3", asm.Manifest.Components[0].Version)
		require.Equal(t, "demo@"+sha256Hex(string(raw))[:12], asm.Manifest.Components[0].Info["skills"])
		return asm
	}
	store, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	ctx := durable.WithJournal(durable.WithExecutionKey(context.Background(), "run"), store)
	first := build("1")
	require.NoError(t, first.PinManifest(ctx))
	require.NoError(t, build("1").PinManifest(ctx), "unchanged skill resumes")
	changed := build("2")
	require.Equal(t, first.SystemPrompt, changed.SystemPrompt)
	require.ErrorIs(t, changed.PinManifest(ctx), ErrManifestChanged, "metadata-only changes invalidate the pinned manifest")
}
