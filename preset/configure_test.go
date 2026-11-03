package preset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Configure 默认把工具名错误交还模型，编码 Agent 不因一次笔误中止。
func TestConfigureFeedsToolErrorsBack(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, codingPreset("")), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	model := &scriptModel{turns: []scriptTurn{call("c1", "no_such_tool", `{}`), say("改用正确工具")}}
	answer, err := run(t, asm, model, "s1", "做点事")
	require.NoError(t, err)
	require.Equal(t, "改用正确工具", answer)
	require.Contains(t, joined(model.request(1)), "tool: ")
	require.Contains(t, joined(model.request(1)), "no_such_tool")
}
