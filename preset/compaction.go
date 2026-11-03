package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
)

// ComponentCompaction 是对话压缩组件名。
const ComponentCompaction = "compaction"

// CompactionConfig 是 compaction 组件的配置。估算的对话长度超过 MaxTokens 时，把较早的消息交给
// Host.CompactionModel 写成一条摘要，最近约 KeepTokens 的消息与本步新收到的输入原样保留；
// 模型请求因超出上下文失败时（非持久运行）强制压缩一次后重试。压缩发生在 PreStep，持久运行中摘要随该步记入日志。
// 被压缩的部分中，用户原话（第一条与最近的，合计约 KeepTokens/2）与最近获批的计划原样保留，工作区的待办附在摘要后；
// 摘要调用计入根预算，失败时退化为省略旧消息的说明，不中止运行（取消与预算耗尽除外）。只作用于原生工具流。
type CompactionConfig struct {
	// MaxTokens 是触发压缩的估算 token 数，默认 100000。按 UTF-8 字节数的三分之一估算，对英文偏保守。
	MaxTokens int `yaml:"maxTokens"`
	// KeepTokens 是原样保留的最近消息的估算 token 数，默认 MaxTokens 的四分之一。
	KeepTokens int `yaml:"keepTokens"`
	// Instructions 是对摘要的补充要求，如必须保留的信息。
	Instructions string `yaml:"instructions"`
}

func init() {
	Register(Spec[CompactionConfig]{
		Name: ComponentCompaction, Version: "2", Singleton: true,
		Validate: func(c *CompactionConfig) error {
			if c.MaxTokens == 0 {
				c.MaxTokens = 100000
			}
			if c.KeepTokens == 0 {
				c.KeepTokens = c.MaxTokens / 4
			}
			if c.MaxTokens < 0 || c.KeepTokens < 0 {
				return errors.New("maxTokens 与 keepTokens 不能为负")
			}
			if c.KeepTokens >= c.MaxTokens {
				return errors.New("keepTokens 须小于 maxTokens")
			}
			return nil
		},
		Build: func(_ context.Context, a *Assembler, c *CompactionConfig) error {
			if a.host.CompactionModel == nil {
				return errors.New("压缩需要 Host.CompactionModel")
			}
			cp := &compactor{model: a.host.CompactionModel, cfg: *c, todos: func() []coding.Todo {
				// workspace 可能排在 compaction 之后初始化，压缩时再取。
				if w, _, err := a.Workspace(); err == nil {
					return w.Todos()
				}
				return nil
			}}
			a.AddPreStep(cp.preStep)
			a.AddRequestError(cp.requestError)
			return nil
		},
	})
}

type compactor struct {
	model llm.Client
	cfg   CompactionConfig
	todos func() []coding.Todo
}

const compactionPrompt = "你负责压缩 Agent 的对话历史。把用户给出的对话整理成摘要，供 Agent 接着完成任务：" +
	"保留用户的目标与要求、已经做出的决定、已完成与未完成的工作、关键事实（文件路径、命令、错误信息、数值）以及尚未解决的问题；" +
	"省略寒暄与重复内容。只输出摘要。"

// summaryPrefix 标记摘要消息，planPrefix 标记保留的获批计划；再次压缩时据此识别。
const (
	summaryPrefix = "[此前对话的摘要]\n"
	planPrefix    = "[已获批的计划]\n"
)

// summaryOutputTokens 是预留预算时为摘要输出估计的 token 数。
const summaryOutputTokens = 2048

// 摘要请求中单条消息的上限，避免个别超长的工具结果撑满摘要模型的上下文。
const maxRenderedRunes = 4000

func (cp *compactor) preStep(ctx context.Context, step *builder.Step) (builder.PreStepResult, error) {
	if estimateTokens(step.Messages) <= cp.cfg.MaxTokens {
		return builder.PreStepResult{}, nil
	}
	out, _, err := cp.compact(ctx, step.Messages, step.Inputs)
	if err != nil {
		return builder.PreStepResult{}, fmt.Errorf("压缩对话: %w", err)
	}
	step.Messages = out
	return builder.PreStepResult{}, nil
}

func (cp *compactor) requestError(ctx context.Context, f *builder.RequestFailure) (builder.RequestErrorDecision, error) {
	if !f.Class.ShouldCompress {
		return builder.RequestErrorDecision{}, nil
	}
	// 已经超出上下文，按 keepTokens 保留不一定腾得出空间：只保留最近一次交互。
	out, changed, err := cp.compactKeeping(ctx, f.Messages, 0, 0)
	if err != nil || !changed {
		return builder.RequestErrorDecision{}, err
	}
	return builder.RequestErrorDecision{Action: builder.RequestRetry, Messages: out}, nil
}

// compact 把 system 之后、保留区之前的消息替换为一条摘要。保留区从非 tool 消息开始，
// 因而不会拆开 assistant 的工具调用与其结果；末尾 inputs 条消息（至少一条）总在保留区内。
// 找不到可压缩的区间，或区间里只有旧摘要与原样保留的消息时原样返回，changed 为假。
func (cp *compactor) compact(ctx context.Context, messages []llm.Message, inputs int) (out []llm.Message, changed bool, err error) {
	return cp.compactKeeping(ctx, messages, inputs, cp.cfg.KeepTokens)
}

// compactKeeping 同 compact，保留区约为 keep 估算 token。
func (cp *compactor) compactKeeping(ctx context.Context, messages []llm.Message, inputs, keep int) (out []llm.Message, changed bool, err error) {
	head := 0
	if len(messages) > 0 && messages[0].Role == "system" {
		head = 1
	}
	limit := len(messages) - max(inputs, 1)
	cut := -1
	tail := 0
	for _, m := range messages[max(limit, 0):] {
		tail += messageTokens(m)
	}
	for i := limit; i > head; i-- {
		if i < limit {
			tail += messageTokens(messages[i])
		}
		if messages[i].Role == "tool" {
			continue
		}
		if cut < 0 || tail <= keep {
			cut = i
		}
		if tail > keep {
			break
		}
	}
	if cut <= head {
		return messages, false, nil
	}
	span := messages[head:cut]
	kept := keptMessages(span, cp.cfg.KeepTokens/2)
	// 区间里只有旧摘要和本就原样保留的消息时，再请求摘要模型只会复述旧摘要。
	summaries := 0
	for _, m := range span {
		if m.Role == "user" && strings.HasPrefix(m.Content, summaryPrefix) {
			summaries++
		}
	}
	if summaries+len(kept) == len(span) {
		return messages, false, nil
	}
	summary, err := cp.summarize(ctx, span)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, budget.ErrBudgetExceeded) {
			return nil, false, err
		}
		summary = fallbackSummary(span, err)
	}
	if cp.todos != nil {
		if todos := cp.todos(); len(todos) > 0 {
			summary += "\n\n当前待办："
			for _, t := range todos {
				summary += fmt.Sprintf("\n- [%s] %s", t.Status, t.Content)
			}
		}
	}
	out = append([]llm.Message{}, messages[:head]...)
	out = append(out, llm.UserMessage(summaryPrefix+summary))
	out = append(out, kept...)
	return append(out, messages[cut:]...), true, nil
}

// summarize 请求摘要模型；绑定了根预算时先预留再登记用量，与 Agent 的模型请求共用额度。
func (cp *compactor) summarize(ctx context.Context, span []llm.Message) (string, error) {
	msgs := []llm.Message{
		llm.SystemMessage(strings.TrimSpace(compactionPrompt + "\n" + cp.cfg.Instructions)),
		llm.UserMessage(renderTranscript(span)),
	}
	c, metered := budget.FromContext(ctx)
	// 每次请求用新键：同一键不能重复预留，持久运行恢复时重新压缩会再占一份额度。
	key := "compaction/" + uuid.NewString()
	if metered {
		estimate := 0
		if c.Configuration().MaxReservedTokens > 0 {
			estimate = estimateTokens(msgs) + summaryOutputTokens
		}
		if err := c.ReserveEstimated(ctx, key, estimate); err != nil {
			return "", err
		}
	}
	resp, err := cp.model.Chat(ctx, msgs)
	if metered {
		var u *interfaces.TokenUsage
		if resp != nil {
			u = resp.Usage
		}
		err = errors.Join(err, c.RecordUsage(ctx, key, u))
	}
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(resp.Content)
	if summary == "" {
		return "", errors.New("摘要模型返回了空内容")
	}
	return summary, nil
}

// fallbackSummary 在摘要失败时说明省略了什么，模型据此知道需要时重新查看。
func fallbackSummary(span []llm.Message, cause error) string {
	var names []string
	counts := map[string]int{}
	for _, m := range span {
		for _, tc := range m.ToolCalls {
			if counts[tc.Function.Name] == 0 {
				names = append(names, tc.Function.Name)
			}
			counts[tc.Function.Name]++
		}
	}
	text := fmt.Sprintf("摘要生成失败（%v），较早的 %d 条消息已省略。", cause, len(span))
	if len(names) > 0 {
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = fmt.Sprintf("%s %d 次", n, counts[n])
		}
		text += "其中调用过的工具：" + strings.Join(parts, "、") + "。需要其中的信息时请重新查看。"
	}
	return text
}

// keptMessages 从被压缩的部分挑出原样保留的消息，按原顺序返回：最近获批的计划（不计入 limit），
// 以及用户原话——先保留第一条（通常是任务原文），再从最近往前，合计不超过 limit 估算 token。
// 摘要、loop-detect 的提醒与 verify-before-stop 的反馈不算用户原话。
func keptMessages(span []llm.Message, limit int) []llm.Message {
	plans := map[string]string{}
	planAt, plan := -1, ""
	var users []int
	for i, m := range span {
		switch m.Role {
		case "assistant":
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == ToolExitPlanMode {
					var args struct {
						Plan string `json:"plan"`
					}
					if json.Unmarshal([]byte(tc.Function.Arguments), &args) == nil {
						plans[tc.ID] = args.Plan
					}
				}
			}
		case "tool":
			var r planResult
			if p, ok := plans[m.ToolCallID]; ok && json.Unmarshal([]byte(m.Content), &r) == nil && r.PlanMode == planExited {
				planAt, plan = i, planPrefix+p
			}
		case "user":
			switch {
			case strings.HasPrefix(m.Content, planPrefix):
				planAt, plan = i, m.Content
			case !strings.HasPrefix(m.Content, summaryPrefix) && !strings.HasPrefix(m.Content, loopMarkerPrefix) &&
				!strings.HasPrefix(m.Content, verifyFeedbackPrefix):
				users = append(users, i)
			}
		}
	}
	keep := map[int]llm.Message{}
	used := 0
	take := func(i int) {
		if n := messageTokens(span[i]); used+n <= limit {
			keep[i], used = span[i], used+n
		}
	}
	if len(users) > 0 {
		take(users[0])
	}
	for j := len(users) - 1; j > 0; j-- {
		take(users[j])
	}
	if planAt >= 0 {
		keep[planAt] = llm.UserMessage(plan)
	}
	var out []llm.Message
	for i := range span {
		if m, ok := keep[i]; ok {
			out = append(out, m)
		}
	}
	return out
}

// renderTranscript 把消息写成纯文本交给摘要模型，不依赖提供方对工具调用消息的支持。
func renderTranscript(messages []llm.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		content := m.Content
		// 旧摘要不截断，否则多次压缩逐级失真。
		if !strings.HasPrefix(content, summaryPrefix) {
			content = truncateRunes(content, maxRenderedRunes)
		}
		fmt.Fprintf(&sb, "[%s]\n%s\n", m.Role, content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&sb, "调用工具 %s：%s\n", tc.Function.Name, truncateRunes(tc.Function.Arguments, maxRenderedRunes))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…（已截断）"
	}
	return s
}

func estimateTokens(messages []llm.Message) int {
	n := 0
	for _, m := range messages {
		n += messageTokens(m)
	}
	return n
}

// messageTokens 按 UTF-8 字节数的三分之一估算：中文约一字一 token，英文约四字符一 token，因此偏保守。
func messageTokens(m llm.Message) int {
	n := len(m.Content) + len(m.ReasoningContent)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n/3 + 4
}
