package preset_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

// 验证真实装配与日志：已知资源限制应反馈给模型以便选择其他操作，
// 不应消耗失败操作的重试次数。
func TestSkillsEnumerationLimitsDurableRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(*testing.T, string)
	}{
		{
			name: "depth", reason: "skill resource depth limit exceeded",
			setup: func(t *testing.T, dir string) {
				for range constants.SkillMaxResourceDepth + 1 {
					dir = filepath.Join(dir, "d")
				}
				require.NoError(t, os.MkdirAll(dir, 0700))
			},
		},
		{
			name: "entries", reason: "skill directory entry limit exceeded",
			setup: func(t *testing.T, dir string) {
				// SKILL.md 本身也占用一个资源枚举条目。
				for i := range constants.SkillMaxEntries {
					require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("ref-%05d.txt", i)), nil, 0600))
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, ".agents", "skills", "demo")
			require.NoError(t, os.MkdirAll(dir, 0700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\ndescription: durable limit fixture\n---\nRead the references."), 0600))
			tc.setup(t, dir)

			t.Run("dispatcher", func(t *testing.T) {
				asm := skillsLimitAssembly(t, root)
				ctx := skillsLimitJournal(t)
				require.NoError(t, asm.PinManifest(ctx))
				req := tools.ToolRequest{Name: preset.ToolLoadSkill, Input: &interfaces.ToolInput{
					CallID: "limit", Args: map[string]any{"name": "demo"},
				}}
				out, err := asm.Dispatcher.Invoke(ctx, req)
				require.NoError(t, err)
				message, rejected := tools.RejectedResult(out)
				require.True(t, rejected)
				require.Contains(t, message, tc.reason)
				skillsLimitCompleted(t, ctx, "tool/limit")

				// 关闭来源后仍能返回原拒绝结果，证明调用使用了日志重放。
				require.NoError(t, asm.Close())
				again, err := asm.Dispatcher.Invoke(ctx, req)
				require.NoError(t, err)
				replayedMessage, rejected := tools.RejectedResult(again)
				require.True(t, rejected)
				require.Equal(t, message, replayedMessage)
				skillsLimitCompleted(t, ctx, "tool/limit")
			})

			t.Run("builder", func(t *testing.T) {
				asm := skillsLimitAssembly(t, root)
				ctx := skillsLimitJournal(t)
				require.NoError(t, asm.PinManifest(ctx))
				model := &skillsLimitModel{}
				agent, err := preset.Configure(builder.NewSimpleBuilder(model), asm).
					WithDurableScope("skills-limit").WithMaxIterations(3).Build()
				require.NoError(t, err)
				out, err := agent.AsRunnable().Invoke(ctx, "load demo")
				require.NoError(t, err, "已知枚举限制应反馈给模型，不能中止持久运行")
				require.Equal(t, "choose another skill", out.Result)
				require.Len(t, model.requests, 2)
				messages := model.requests[1].Messages
				require.NotEmpty(t, messages)
				feedback := messages[len(messages)-1]
				require.Equal(t, "tool", feedback.Role)
				require.Equal(t, "c1", feedback.ToolCallID)
				var result struct {
					Error   string `json:"error"`
					Message string `json:"message"`
				}
				require.NoError(t, json.Unmarshal([]byte(feedback.Content), &result))
				require.Equal(t, "rejected", result.Error)
				require.Contains(t, result.Message, tc.reason)
				agentCtx := durable.WithExecutionKey(ctx, durable.ChildKey(durable.ExecutionKey(ctx), "agent/skills-limit"))
				skillsLimitCompleted(t, agentCtx, "tool/turn/0/call/c1")

				require.NoError(t, asm.Close())
				again, err := agent.AsRunnable().Invoke(ctx, "load demo")
				require.NoError(t, err)
				require.Equal(t, out.Result, again.Result)
				require.Len(t, model.requests, 2, "恢复时不应发送新的模型请求")
				skillsLimitCompleted(t, agentCtx, "tool/turn/0/call/c1")
			})
		})
	}
}

func skillsLimitAssembly(t *testing.T, root string) *preset.Assembly {
	t.Helper()
	set, err := preset.Load(preset.Source{
		Layer: preset.LayerHost, Path: "skills-durable-test", Trusted: true,
		Data: []byte("presets: [{id: p, components: [{id: s, use: skills}]}]"),
	})
	require.NoError(t, err)
	p, ok := set.Get("p")
	require.True(t, ok)
	asm, err := preset.Build(context.Background(), p, preset.Host{Root: root, ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	return asm
}

func skillsLimitJournal(t *testing.T) context.Context {
	t.Helper()
	store, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	return durable.WithJournal(durable.WithExecutionKey(context.Background(), "skills-limit-test"), store)
}

func skillsLimitCompleted(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	record, err := durable.InspectOperation(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "completed", record.Status)
	require.Equal(t, 1, record.Attempts)
}

type skillsLimitModel struct {
	requests []*llm.CompletionRequest
}

func (*skillsLimitModel) Provider() constants.Provider { return constants.ProviderOpenAI }

func (*skillsLimitModel) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return nil, errors.New("不应进入旧版模型调用路径")
}

func (m *skillsLimitModel) CompleteToolsStream(_ context.Context, req *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	copy := *req
	copy.Messages = append([]llm.Message(nil), req.Messages...)
	m.requests = append(m.requests, &copy)
	stream := make(chan *llm.StreamChunk, 2)
	switch len(m.requests) {
	case 1:
		call := llm.ToolCall{ID: "c1", Type: "function"}
		call.Function.Name = preset.ToolLoadSkill
		call.Function.Arguments = `{"name":"demo"}`
		stream <- &llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}}
	case 2:
		stream <- &llm.StreamChunk{Delta: "choose another skill"}
		stream <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
	default:
		return nil, errors.New("日志重放时不应再次请求模型")
	}
	close(stream)
	return stream, nil
}
