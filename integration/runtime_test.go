package integration_test

import (
	"context"
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
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

// 原生工具流模型只生成固定的写入和答复，测试不依赖外部模型服务。
type writerModel struct {
	*llm.MockStreamClient
	calls int
}

func (m *writerModel) CompleteToolsStream(_ context.Context, _ *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	m.calls++
	chunk := &llm.StreamChunk{Delta: "已写入", Done: true, FinishReason: "stop"}
	if m.calls == 1 {
		call := llm.ToolCall{ID: "write-1", Type: "function"}
		call.Function.Name = "write_file"
		call.Function.Arguments = `{"path":"result.txt","content":"任务完成"}`
		chunk = &llm.StreamChunk{ToolCalls: []llm.ToolCall{call}, Done: true, FinishReason: "tool_calls"}
	} else if m.calls > 2 {
		return nil, fmt.Errorf("恢复后不应再次调用模型")
	}
	stream := make(chan *llm.StreamChunk, 1)
	stream <- chunk
	close(stream)
	return stream, nil
}

type taskState struct {
	Result string
}

// 新模块仅使用 GoAgent 公开接口：装配后嵌入持久图，重建宿主后批准续跑，完成记录再次打开时不重复执行。
func TestHarnessRunsInDurableWorkflow(t *testing.T) {
	ctx := context.Background()
	root, journal, artifacts := t.TempDir(), t.TempDir(), t.TempDir()
	model := &writerModel{MockStreamClient: llm.NewMockStreamClient()}
	approvals := 0
	build := func() *workflow.DurableGraph[taskState] {
		set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Trusted: true, Data: []byte(`
presets:
  - id: writer
    components:
      - {id: workspace, use: workspace, config: {sandbox: danger-full-access, executor: local, network: true}}
      - {id: coding, use: coding-tools, config: {tools: [write_file]}}
      - {id: permissions, use: permissions, config: {rules: [{tool: write_file, decision: ask}]}}
`)})
		require.NoError(t, err)
		p, ok := set.Get("writer")
		require.True(t, ok)
		asm, err := preset.Build(ctx, p, preset.Host{Root: root, ArtifactDir: artifacts,
			Approver: tools.ApproverFunc(func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
				approvals++
				require.Equal(t, "write_file", req.Call.Tool)
				return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
			}),
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, asm.Close()) })
		agent, err := preset.Configure(builder.NewSimpleBuilder(model), asm).
			WithSessionID("writer").WithDurableScope("writer").WithMaxIterations(3).Build()
		require.NoError(t, err)
		g := workflow.NewGraph[taskState]()
		require.NoError(t, g.AddNode("agent", func(ctx context.Context, in taskState) (taskState, error) {
			if err := asm.PinManifest(ctx); err != nil {
				return in, err
			}
			out, err := agent.AsRunnable().Invoke(ctx, "写入任务结果")
			if err != nil {
				return in, err
			}
			in.Result = fmt.Sprint(out.Result)
			return in, nil
		}))
		require.NoError(t, g.AddEdge(constants.WorkflowStart, "agent"))
		require.NoError(t, g.AddEdge("agent", constants.WorkflowEnd))
		store, err := durable.NewFileStore(journal)
		require.NoError(t, err)
		d, err := g.CompileDurable(ctx, workflow.DurableConfig{Store: store, Version: "writer-v1", ApprovalNodes: []string{"agent"}})
		require.NoError(t, err)
		return d
	}

	waiting, err := build().AsRunnable().Invoke(ctx, workflow.RunRequest[taskState]{RunID: "task-1"})
	require.NoError(t, err)
	require.True(t, waiting.Waiting)
	require.False(t, waiting.Done)
	require.Zero(t, model.calls, "图审批前不执行模型和工具")
	_, err = os.Stat(filepath.Join(root, "result.txt"))
	require.True(t, os.IsNotExist(err))

	finished, err := build().AsRunnable().Invoke(ctx, workflow.RunRequest[taskState]{RunID: "task-1", ApprovalKey: waiting.Key})
	require.NoError(t, err)
	require.True(t, finished.Done)
	require.Equal(t, "已写入", finished.State.Result)
	content, err := os.ReadFile(filepath.Join(root, "result.txt"))
	require.NoError(t, err)
	require.Equal(t, "任务完成", string(content))
	require.Equal(t, 2, model.calls)
	require.Equal(t, 1, approvals)

	replayed, err := build().AsRunnable().Invoke(ctx, workflow.RunRequest[taskState]{RunID: "task-1"})
	require.NoError(t, err)
	require.True(t, replayed.Done)
	require.Equal(t, "已写入", replayed.State.Result)
	require.Equal(t, 2, model.calls, "完成后不重复请求模型")
	require.Equal(t, 1, approvals, "完成后不重复审批或执行工具")
}
