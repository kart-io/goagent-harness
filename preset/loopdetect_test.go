package preset

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
)

// history 按 [工具名, 参数, 结果] 依次生成 assistant(tool_calls) 与 tool 消息。
func history(calls ...[3]string) []llm.Message {
	msgs := []llm.Message{llm.UserMessage("修复测试")}
	for i, c := range calls {
		id := fmt.Sprintf("c%d", i)
		tc := llm.ToolCall{ID: id, Type: "function"}
		tc.Function.Name, tc.Function.Arguments = c[0], c[1]
		msgs = append(msgs,
			llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{tc}},
			llm.Message{Role: "tool", ToolCallID: id, Content: c[2]})
	}
	return msgs
}

func defaultLoopConfig() LoopDetectConfig {
	c := LoopDetectConfig{}
	if err := loopDetectSpec.Validate(&c); err != nil {
		panic(err)
	}
	return c
}

func goTest(output string, ms int) [3]string {
	return [3]string{"run_command", `{"command":"go test ./..."}`, fmt.Sprintf(`{"exit_code":1,"output":%q,"duration":"%dms"}`, output, ms)}
}

func TestLoopDetectRepeatedCallWithSameResult(t *testing.T) {
	c := defaultLoopConfig()
	msgs := history(goTest("FAIL x", 10), goTest("FAIL x", 20))
	require.Empty(t, detectLoop(msgs, c).warning, "两次不提醒")

	msgs = history(goTest("FAIL x", 10), goTest("FAIL x", 20), goTest("FAIL x", 35))
	d := detectLoop(msgs, c)
	require.Contains(t, d.warning, "run_command")
	require.Contains(t, d.warning, "3 次")
	require.Empty(t, d.halt)
}

func TestLoopDetectDifferentResultsAreProgress(t *testing.T) {
	msgs := history(goTest("FAIL a", 1), goTest("FAIL b", 1), goTest("FAIL c", 1))
	require.Empty(t, detectLoop(msgs, defaultLoopConfig()).warning)
}

func TestLoopDetectArgumentOrderDoesNotMatter(t *testing.T) {
	a := [3]string{"grep", `{"pattern":"x","path":"."}`, "none"}
	b := [3]string{"grep", `{"path":".","pattern":"x"}`, "none"}
	require.NotEmpty(t, detectLoop(history(a, b, a), defaultLoopConfig()).warning)
}

func TestLoopDetectWarnsOncePerNewRepeat(t *testing.T) {
	c := defaultLoopConfig()
	msgs := history(goTest("FAIL x", 1), goTest("FAIL x", 2), goTest("FAIL x", 3))
	d := detectLoop(msgs, c)
	msgs = append(msgs, llm.UserMessage(d.warning))
	require.Empty(t, detectLoop(msgs, c).warning, "已提醒过且没有新的重复")

	msgs = append(msgs, history(goTest("FAIL x", 4))[1:]...)
	msgs[len(msgs)-2].ToolCalls[0].ID, msgs[len(msgs)-1].ToolCallID = "c9", "c9"
	d = detectLoop(msgs, c)
	require.Contains(t, d.warning, "4 次")
}

func TestLoopDetectFileEdits(t *testing.T) {
	c := defaultLoopConfig()
	c.FileEdits = 3
	var calls [][3]string
	for i := range 3 {
		calls = append(calls, [3]string{"edit_file", fmt.Sprintf(`{"path":"calc.go","old_string":"a%d","new_string":"b%d"}`, i, i), `{"path":"calc.go"}`})
	}
	d := detectLoop(history(calls...), c)
	require.Contains(t, d.warning, "calc.go")
	require.Contains(t, d.warning, "3 次")
}

func TestLoopDetectWindowForgetsOldCalls(t *testing.T) {
	c := defaultLoopConfig()
	c.Window = 4
	calls := [][3]string{goTest("FAIL x", 1), goTest("FAIL x", 2)}
	for i := range 3 {
		calls = append(calls, [3]string{"read_file", fmt.Sprintf(`{"path":"f%d"}`, i), "x"})
	}
	calls = append(calls, goTest("FAIL x", 3))
	require.Empty(t, detectLoop(history(calls...), c).warning)
}

func TestLoopDetectHalt(t *testing.T) {
	c := defaultLoopConfig()
	c.HaltRepeats = 4
	calls := [][3]string{goTest("FAIL x", 1), goTest("FAIL x", 2), goTest("FAIL x", 3), goTest("FAIL x", 4)}
	d := detectLoop(history(calls...), c)
	require.Contains(t, d.halt, "run_command")
}

func TestLoopDetectOnlyAfterToolResults(t *testing.T) {
	msgs := history(goTest("FAIL x", 1), goTest("FAIL x", 2), goTest("FAIL x", 3))
	msgs = append(msgs, llm.UserMessage("换个思路"))
	require.Empty(t, detectLoop(msgs, defaultLoopConfig()).warning, "用户刚给出新输入时不打断")
}

func TestLoopDetectValidation(t *testing.T) {
	for _, cfg := range []string{`{repeats: 1}`, `{window: -1}`, `{fileEdits: -1}`, `{haltRepeats: 2}`} {
		_, err := Load(trusted(`presets: [{id: p, components: [{id: l, use: loop-detect, config: ` + cfg + `}]}]`))
		require.Error(t, err, cfg)
	}
}

// 组件经 Configure 生效：第三次相同的失败后，下一次模型请求带着提醒。
func TestLoopDetectInjectsReminder(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, codingPreset(`, {id: loop, use: loop-detect}`)), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	cmd := `{"command":"echo FAIL"}`
	model := &scriptModel{turns: []scriptTurn{call("c1", "run_command", cmd), call("c2", "run_command", cmd), call("c3", "run_command", cmd), say("换个方法")}}
	agent, err := Configure(builder.NewSimpleBuilder(model), asm).WithSessionID("s").WithMaxIterations(8).Build()
	require.NoError(t, err)
	_, err = agent.AsRunnable().Invoke(context.Background(), "修复")
	require.NoError(t, err)
	hasReminder := func(msgs []llm.Message) bool {
		return slices.ContainsFunc(msgs, func(m llm.Message) bool { return m.Role == "user" && strings.HasPrefix(m.Content, loopMarkerPrefix) })
	}
	require.False(t, hasReminder(model.request(2)), "两次相同结果不提醒")
	require.True(t, hasReminder(model.request(3)), "第三次相同结果后提醒")
}
