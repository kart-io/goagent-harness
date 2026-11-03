package preset

import (
	"slices"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core"
)

// Loop 是组件贡献的循环扩展点，按组件在列表中的顺序排列。
type Loop struct {
	PreStep        []builder.PreStepHook
	Request        []builder.RequestHook
	RequestError   []builder.RequestErrorHook
	TurnStopping   []builder.TurnStoppingHook
	PromptSections []builder.PromptSection
	PromptContexts []builder.PromptContext
}

func (l *Loop) add(o Loop) {
	l.PreStep = append(l.PreStep, o.PreStep...)
	l.Request = append(l.Request, o.Request...)
	l.RequestError = append(l.RequestError, o.RequestError...)
	l.TurnStopping = append(l.TurnStopping, o.TurnStopping...)
	l.PromptSections = append(l.PromptSections, o.PromptSections...)
	l.PromptContexts = append(l.PromptContexts, o.PromptContexts...)
}

// AddPreStep 追加 PreStep 钩子。
func (a *Assembler) AddPreStep(h builder.PreStepHook) {
	a.slot().loop.PreStep = append(a.slot().loop.PreStep, h)
}

// AddRequest 追加 Request 钩子，可按步调整推理强度、换模型。持久运行中须是确定性的：结果不入日志，恢复时重新执行。
func (a *Assembler) AddRequest(h builder.RequestHook) {
	a.slot().loop.Request = append(a.slot().loop.Request, h)
}

// AddRequestError 追加 RequestError 钩子。
func (a *Assembler) AddRequestError(h builder.RequestErrorHook) {
	a.slot().loop.RequestError = append(a.slot().loop.RequestError, h)
}

// AddTurnStopping 追加 TurnStopping 钩子。
func (a *Assembler) AddTurnStopping(h builder.TurnStoppingHook) {
	a.slot().loop.TurnStopping = append(a.slot().loop.TurnStopping, h)
}

// AddPromptSection 追加进入 system 的提示词段落。
func (a *Assembler) AddPromptSection(s builder.PromptSection) {
	a.slot().loop.PromptSections = append(a.slot().loop.PromptSections, s)
}

// AddPromptContext 追加每步重算的提示词上下文。
func (a *Assembler) AddPromptContext(c builder.PromptContext) {
	a.slot().loop.PromptContexts = append(a.slot().loop.PromptContexts, c)
}

// transientRetries 是模型请求遇到 429、5xx 与连接错误时的重试次数。
const transientRetries = 3

// Configure 把装配结果交给 Builder：系统提示词、工具与循环扩展点。
// 编码 Agent 的会话很长，默认让模型看到可恢复的工具失败（WithToolErrorFeedback），
// 并在组件钩子之后重试暂时性的模型请求失败（RetryTransient）。
func Configure[C any, S core.State](b *builder.AgentBuilder[C, S], asm *Assembly) *builder.AgentBuilder[C, S] {
	l := asm.Loop
	return b.WithSystemPrompt(asm.SystemPrompt).WithTools(asm.Tools...).
		WithPreStep(l.PreStep...).WithRequestHook(l.Request...).WithTurnStopping(l.TurnStopping...).
		WithPromptSections(l.PromptSections...).WithPromptContexts(l.PromptContexts...).
		WithRequestError(0, append(slices.Clone(l.RequestError), builder.RetryTransient(transientRetries, nil))...).
		WithToolErrorFeedback()
}
