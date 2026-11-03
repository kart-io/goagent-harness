package preset

import (
	"context"
	"strings"
	"testing"

	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
)

func verifyPreset(config string) Source {
	return codingPreset(`, {id: v, use: verify-before-stop, config: ` + config + `}`)
}

func verifyFeedback(msgs []llm.Message) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.HasPrefix(m.Content, "结束前的验证") {
			return m.Content
		}
	}
	return ""
}

// 验证失败时把输出交还模型续跑，修复后通过即结束。
func TestVerifyBeforeStopContinuesUntilCheckPasses(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, verifyPreset(`{command: 'test -f ok || { echo missing ok; exit 1; }'}`)), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	model := &scriptModel{turns: []scriptTurn{
		call("c1", "write_file", `{"path":"a.txt","content":"x"}`), say("完成"),
		call("c2", "write_file", `{"path":"ok","content":"x"}`), say("修好了"),
	}}
	answer, err := run(t, asm, model, "s", "改代码")
	require.NoError(t, err)
	require.Equal(t, "修好了", answer)
	feedback := verifyFeedback(model.request(2))
	require.Contains(t, feedback, "第 1/3 次")
	require.Contains(t, feedback, "退出码 1")
	require.Contains(t, feedback, "missing ok")
	require.Len(t, model.requests, 4)
}

// 改动过的文件按路径排序作为位置参数传给检查命令。
func TestVerifyBeforeStopPassesChangedFiles(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, verifyPreset(`{command: 'echo "changed:$*"; exit 1', maxAttempts: 1}`)), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	model := &scriptModel{turns: []scriptTurn{
		call("c1", "write_file", `{"path":"pkg/b.go","content":"x"}`), call("c2", "write_file", `{"path":"a.go","content":"x"}`),
		say("完成"), say("结束"),
	}}
	_, err = run(t, asm, model, "s", "改代码")
	require.NoError(t, err)
	require.Contains(t, verifyFeedback(model.request(3)), "changed:a.go pkg/b.go")
}

// 没有经写入类工具改动文件时不检查；续跑达到 maxAttempts 后按模型的答复结束。
func TestVerifyBeforeStopSkipsAndCaps(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, verifyPreset(`{command: 'exit 1', maxAttempts: 1}`)), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	model := &scriptModel{turns: []scriptTurn{say("只是回答")}}
	answer, err := run(t, asm, model, "s1", "问个问题")
	require.NoError(t, err)
	require.Equal(t, "只是回答", answer)

	model = &scriptModel{turns: []scriptTurn{call("c1", "write_file", `{"path":"a.txt","content":"x"}`), say("完成"), say("修不好")}}
	answer, err = run(t, asm, model, "s2", "改代码")
	require.NoError(t, err)
	require.Equal(t, "修不好", answer)
	require.Contains(t, verifyFeedback(model.request(2)), "第 1/1 次")
	require.Len(t, model.requests, 3)
}

func TestVerifyBeforeStopValidation(t *testing.T) {
	for _, cfg := range []string{`{}`, `{command: ' '}`, `{command: x, maxAttempts: -1}`} {
		_, err := Load(trusted(`presets: [{id: p, components: [` + localWorkspace + `, {id: v, use: verify-before-stop, config: ` + cfg + `}]}]`))
		require.Error(t, err, cfg)
	}
	_, err := Load(trusted(`presets: [{id: p, components: [{id: v, use: verify-before-stop, config: {command: x}}]}]`))
	require.ErrorContains(t, err, "workspace")
}
