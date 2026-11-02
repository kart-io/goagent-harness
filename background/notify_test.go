package background

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

// scriptModel 按顺序回复并记录每次请求的消息。
type scriptModel struct {
	llm.Client
	mu       sync.Mutex
	answers  []string
	requests [][]llm.Message
}

func (m *scriptModel) stream(answer string) <-chan *llm.StreamChunk {
	ch := make(chan *llm.StreamChunk, 2)
	ch <- &llm.StreamChunk{Delta: answer}
	ch <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
	close(ch)
	return ch
}

func (m *scriptModel) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return m.stream("x"), nil
}

func (m *scriptModel) ChatStream(context.Context, []llm.Message) (<-chan *llm.StreamChunk, error) {
	return m.stream("x"), nil
}

func (m *scriptModel) CompleteToolsStream(_ context.Context, req *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, req.Messages)
	answer := m.answers[len(m.requests)-1]
	return m.stream(answer), nil
}

// 端到端：通知进入运行中 Agent 的收件箱并唤醒它继续一轮。
func TestNotifyHandleWakesRunningAgent(t *testing.T) {
	model := &scriptModel{answers: []string{"先等结果", "收到结果"}}
	tool := tools.NewBaseTool("lookup", "test", `{"type":"object"}`, func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
		return &interfaces.ToolOutput{Success: true}, nil
	})
	var handle *builder.Handle
	var mu sync.Mutex
	notify := NotifyHandle(func(string) *builder.Handle {
		mu.Lock()
		defer mu.Unlock()
		return handle
	}, nil)
	notified := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	agent, err := builder.NewSimpleBuilder(model).WithTools(tool).WithMaxIterations(4).
		WithRequestHook(func(context.Context, *builder.ModelRequest) error {
			once.Do(func() {
				close(entered)
				<-notified
			})
			return nil
		}).Build()
	require.NoError(t, err)
	h := agent.Start(context.Background(), "任务")
	mu.Lock()
	handle = h
	mu.Unlock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		_, err := h.Wait(context.Background())
		t.Fatalf("Agent 没有发起请求: %v", err)
	}
	require.NoError(t, notify(context.Background(), Notice{Owner: "o", ChildID: "c1", Status: StatusCompleted, Result: "子工作结果"}))
	close(notified)
	out, err := h.Wait(context.Background())
	require.NoError(t, err)
	require.Equal(t, "收到结果", out.Result)
	last := model.requests[1]
	require.Contains(t, last[len(last)-1].Content, "子工作结果")
	require.Contains(t, last[len(last)-1].Content, "c1")
}

// Handle 已结束或不存在时不丢通知：交给宿主，没有 idle 时稍后重试。
func TestNotifyHandleWhenAgentIsIdle(t *testing.T) {
	model := &scriptModel{answers: []string{"完成"}}
	tool := tools.NewBaseTool("lookup", "test", `{"type":"object"}`, func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
		return &interfaces.ToolOutput{Success: true}, nil
	})
	agent, err := builder.NewSimpleBuilder(model).WithTools(tool).WithMaxIterations(2).Build()
	require.NoError(t, err)
	h := agent.Start(context.Background(), "任务")
	_, err = h.Wait(context.Background())
	require.NoError(t, err)
	notice := Notice{Owner: "o", ChildID: "c1", Status: StatusCompleted}

	require.ErrorIs(t, NotifyHandle(func(string) *builder.Handle { return h }, nil)(context.Background(), notice), effects.ErrLater)
	require.ErrorIs(t, NotifyHandle(func(string) *builder.Handle { return nil }, nil)(context.Background(), notice), effects.ErrLater)
	var seen []Notice
	idle := func(_ context.Context, n Notice) error {
		seen = append(seen, n)
		return nil
	}
	require.NoError(t, NotifyHandle(func(string) *builder.Handle { return h }, idle)(context.Background(), notice))
	require.Len(t, seen, 1)
}
