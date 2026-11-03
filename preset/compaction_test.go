package preset

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
)

// chatModel 只实现摘要用的 Chat，按 err 返回失败。
type chatModel struct {
	scriptModel
	err   error
	usage *interfaces.TokenUsage
}

func (m *chatModel) Chat(ctx context.Context, msgs []llm.Message) (*llm.CompletionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chats = append(m.chats, msgs)
	if m.err != nil {
		return nil, m.err
	}
	return &llm.CompletionResponse{Content: "摘要", Usage: m.usage}, nil
}

// 一段会被压缩的对话：最后两条是保留区。
func compactable(middle ...llm.Message) []llm.Message {
	long := strings.Repeat("长", 60)
	msgs := []llm.Message{llm.SystemMessage("sys")}
	msgs = append(msgs, middle...)
	return append(msgs,
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("r", "read_file", `{}`)}},
		llm.Message{Role: "tool", ToolCallID: "r", Content: long},
		llm.UserMessage("新输入"))
}

func newCompactor(m llm.Client) *compactor {
	return &compactor{model: m, cfg: CompactionConfig{MaxTokens: 100, KeepTokens: 40}}
}

// 第一条用户消息（任务原文）与最近的用户消息原样保留，超出预算的中间消息、摘要与组件注入的提醒不保留。
func TestCompactionKeepsUserMessages(t *testing.T) {
	big := strings.Repeat("中", 40)
	msgs := compactable(
		llm.UserMessage("任务：修复 Sum"),
		llm.UserMessage(summaryPrefix+"更早的摘要"),
		llm.UserMessage(big),
		llm.Message{Role: "assistant", Content: "好"},
		llm.UserMessage(loopMarkerPrefix+"abc] 别重复"),
		llm.UserMessage(verifyFeedbackPrefix+"未通过：FAIL"),
		llm.UserMessage("还要补测试"),
	)
	out, changed, err := newCompactor(&chatModel{}).compact(context.Background(), msgs, 1)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, strings.HasPrefix(out[1].Content, summaryPrefix+"摘要"))
	require.Equal(t, []llm.Message{llm.UserMessage("任务：修复 Sum"), llm.UserMessage("还要补测试")}, out[2:4])
}

// 获批的计划原样保留，再次压缩时继续保留；未获批的提交不算。
func TestCompactionKeepsApprovedPlan(t *testing.T) {
	msgs := compactable(
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("p1", ToolExitPlanMode, `{"plan":"旧计划"}`)}},
		llm.Message{Role: "tool", ToolCallID: "p1", Content: `{"plan_mode":"exited","message":"ok"}`},
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("p2", ToolExitPlanMode, `{"plan":"被拒的计划"}`)}},
		llm.Message{Role: "tool", ToolCallID: "p2", Content: `{"error":"rejected"}`},
	)
	cp := newCompactor(&chatModel{})
	out, _, err := cp.compact(context.Background(), msgs, 1)
	require.NoError(t, err)
	require.Equal(t, llm.UserMessage(planPrefix+"旧计划"), out[2])

	again, _, err := cp.compact(context.Background(), compactable(out[1:3]...), 1)
	require.NoError(t, err)
	require.Equal(t, llm.UserMessage(planPrefix+"旧计划"), again[2])
	rendered := cp.model.(*chatModel).chats[1][1].Content
	require.Contains(t, rendered, summaryPrefix+"摘要", "an earlier summary is rendered in full")
	require.Contains(t, cp.model.(*chatModel).chats[1][1].Content, transcriptClose+"摘要不超过 20 字", "the length limit follows the transcript")
	require.True(t, strings.HasPrefix(rendered, transcriptOpen) && strings.Contains(rendered, transcriptClose), "the transcript is delimited and followed by the instruction")
}

func TestCompactionAppendsTodos(t *testing.T) {
	cp := newCompactor(&chatModel{})
	cp.todos = func() []coding.Todo {
		return []coding.Todo{{Content: "定位 bug", Status: coding.TodoCompleted}, {Content: "补测试", Status: coding.TodoPending}}
	}
	out, _, err := cp.compact(context.Background(), compactable(llm.UserMessage("任务")), 1)
	require.NoError(t, err)
	require.Equal(t, summaryPrefix+"摘要\n\n当前待办：\n- [completed] 定位 bug\n- [pending] 补测试", out[1].Content)

	// 再次压缩时，旧摘要附的待办不交给摘要模型。
	_, _, err = cp.compact(context.Background(), compactable(out[1:3]...), 1)
	require.NoError(t, err)
	rendered := cp.model.(*chatModel).chats[1][1].Content
	require.Contains(t, rendered, summaryPrefix+"摘要")
	require.NotContains(t, rendered, "定位 bug")
}

// 摘要失败时退化为说明并继续；取消与预算耗尽仍中止。
func TestCompactionFallsBackOnSummaryFailure(t *testing.T) {
	msgs := compactable(llm.UserMessage("任务"))
	out, changed, err := newCompactor(&chatModel{err: errors.New("503")}).compact(context.Background(), msgs, 1)
	require.NoError(t, err)
	require.True(t, changed)
	require.Contains(t, out[1].Content, "摘要生成失败（503）")
	require.Equal(t, llm.UserMessage("任务"), out[2])

	// 区间里的旧摘要不因摘要失败丢失。
	withSummary := compactable(llm.UserMessage(summaryPrefix+"已定位 bug"+todoHeader+"\n- [pending] 过时"), llm.UserMessage("任务"))
	out, _, err = newCompactor(&chatModel{err: errors.New("503")}).compact(context.Background(), withSummary, 1)
	require.NoError(t, err)
	require.Contains(t, out[1].Content, "更早的摘要：\n已定位 bug")
	require.NotContains(t, out[1].Content, "过时")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = newCompactor(&chatModel{err: context.Canceled}).compact(ctx, msgs, 1)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCompactionChargesRootBudget(t *testing.T) {
	c, err := budget.New(budget.Config{RootKey: "r", Version: "1", MaxModelRequests: 1})
	require.NoError(t, err)
	ctx := budget.WithController(context.Background(), c)
	model := &chatModel{usage: &interfaces.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	cp := newCompactor(model)
	_, _, err = cp.compact(ctx, compactable(llm.UserMessage("任务")), 1)
	require.NoError(t, err)
	snap, err := c.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, snap.ReservedRequests)
	require.Equal(t, 15, snap.KnownTokens.TotalTokens)

	_, _, err = cp.compact(ctx, compactable(llm.UserMessage("任务")), 1)
	require.ErrorIs(t, err, budget.ErrBudgetExceeded, "an exhausted budget stops the run instead of degrading")
	require.Len(t, model.chats, 1, "no request is sent without budget")
}

// 区间里只有旧摘要与用户消息时不再请求摘要模型。
func TestCompactionSkipsSummaryOnlySpan(t *testing.T) {
	model := &chatModel{}
	msgs := []llm.Message{
		llm.SystemMessage("sys"), llm.UserMessage(summaryPrefix + strings.Repeat("长", 80)), llm.UserMessage("任务"),
		{Role: "assistant", Content: strings.Repeat("答", 40)}, llm.UserMessage("新输入"),
	}
	cp := &compactor{model: model, cfg: CompactionConfig{MaxTokens: 50, KeepTokens: 60}}
	out, changed, err := cp.compact(context.Background(), msgs, 1)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, msgs, out)
	require.Empty(t, model.chats)

	// 超出保留预算的用户长文仍交给摘要模型。
	msgs[2] = llm.UserMessage(strings.Repeat("贴", 200))
	_, changed, err = cp.compact(context.Background(), msgs, 1)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, model.chats, 1)
}

// 连续几次压缩之间没有修改文件或提交时附上提示，次数从旧摘要中累加；有修改时清零。run_command 不算推进。
func TestCompactionNotesReadOnlyThrash(t *testing.T) {
	cp := newCompactor(&chatModel{})
	reads := func(prev string) []llm.Message {
		return compactable(llm.UserMessage(prev), llm.UserMessage("任务"),
			llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("g", coding.ToolGlob, `{}`), toolCall("r2", coding.ToolRunCommand, `{}`)}},
			llm.Message{Role: "tool", ToolCallID: "g", Content: "a.go"}, llm.Message{Role: "tool", ToolCallID: "r2", Content: "FAIL"})
	}
	out, _, err := cp.compact(context.Background(), reads(summaryPrefix+"旧"), 1)
	require.NoError(t, err)
	require.Contains(t, out[1].Content, "连续 2 次压缩之间没有修改文件或提交")

	out, _, err = cp.compact(context.Background(), reads(out[1].Content), 1)
	require.NoError(t, err)
	require.Contains(t, out[1].Content, "连续 3 次压缩之间没有修改文件或提交")
	require.NotContains(t, cp.model.(*chatModel).chats[1][1].Content, "注意：上下文空间有限", "the note is not fed back to the summarizer")

	edited := compactable(llm.UserMessage(out[1].Content), llm.UserMessage("任务"),
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("e", coding.ToolEditFile, `{}`)}},
		llm.Message{Role: "tool", ToolCallID: "e", Content: "ok"})
	out, _, err = cp.compact(context.Background(), edited, 1)
	require.NoError(t, err)
	require.NotContains(t, out[1].Content, "注意：上下文空间有限", "progress resets the streak")
}
