package planning

import (
	"context"
	"fmt"
	"strings"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/workflow"
)

type planningAgent struct {
	name       string
	dispatcher *tools.Dispatcher
}

func newPlanningAgent(cfg AgentConfig, name, instruction string) (*planningAgent, error) {
	if strings.TrimSpace(cfg.Version) == "" || cfg.Factory == nil {
		return nil, fmt.Errorf("规划角色 %s 需要版本及会话工厂", name)
	}
	// Adapt 消费并校验完整流；RunnableTool 接管 Close，包括工厂部分失败的情况。
	factory := func(ctx context.Context) (tools.OwnedRunnable[string, string], error) {
		raw, err := cfg.Factory(ctx)
		owned := tools.OwnedRunnable[string, string]{Close: raw.Close}
		if err != nil {
			return owned, err
		}
		owned.Runnable, err = workflow.Adapt(raw.Runnable, func(input string) (any, error) { return input, nil }, func(_ string, final *builder.AgentOutput) (string, error) {
			if final == nil {
				return "", fmt.Errorf("角色 %s 的会话缺少输出", name)
			}
			text, ok := final.Result.(string)
			if !ok {
				return "", fmt.Errorf("角色 %s 的会话未返回文本", name)
			}
			return text, nil
		})
		return owned, err
	}
	tool, err := tools.NewRunnableToolFactory(factory, tools.RunnableToolConfig[string, string]{
		Name: name, Version: cfg.Version, Description: instruction, ModelInvoking: true, Resumable: true, MaxAttempts: 3,
		ArgsSchema: `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`,
		Decode:     func(args map[string]any) (string, error) { return instruction + "\n" + args["input"].(string), nil },
		Encode:     func(text string) (any, error) { return text, nil },
	})
	if err != nil {
		return nil, err
	}
	registry := tools.NewRegistry()
	if err = registry.Register(tool); err != nil {
		return nil, err
	}
	dispatcher, err := tools.NewDispatcher(registry, tools.DispatchConfig{})
	if err != nil {
		return nil, err
	}
	return &planningAgent{name: name, dispatcher: dispatcher}, nil
}

func (a *planningAgent) candidate(ctx context.Context, input, output any) error {
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	response, err := a.dispatcher.Invoke(ctx, tools.ToolRequest{Name: a.name, Input: &interfaces.ToolInput{CallID: a.name, Args: map[string]any{"input": string(data)}}})
	if err != nil {
		return err
	}
	raw, ok := response.Result.(string)
	if !ok {
		return fmt.Errorf("角色 %s 的日志响应不是文本", a.name)
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return invalid("候选须为单一JSON对象")
	}
	return DecodeCandidate([]byte(raw), output)
}
