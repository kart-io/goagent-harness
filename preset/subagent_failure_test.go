package preset

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
)

func failingSubagent(err error) *subagentTool {
	return &subagentTool{name: "helper", run: func(context.Context, *Assembly, string) (string, error) { return "", err }}
}

// 子 Agent 自身失败是父 Agent 可以处理的结果，不应中止父 Agent。
func TestSubagentFailureReturnedToParent(t *testing.T) {
	out, err := failingSubagent(errors.New("模型调用达到 MaxIterations 上限")).Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"task": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := tools.RejectedResult(out)
	if !ok || !strings.Contains(msg, "MaxIterations") || !strings.Contains(msg, "helper") {
		t.Fatalf("got %#v", out)
	}
}

func TestSubagentFatalFailuresStillAbort(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]struct {
		ctx context.Context
		err error
	}{
		"budget":   {context.Background(), budget.ErrBudgetExceeded},
		"canceled": {canceled, context.Canceled},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := failingSubagent(c.err).Invoke(c.ctx, &interfaces.ToolInput{Args: map[string]any{"task": "x"}})
			if !errors.Is(err, c.err) {
				t.Fatalf("want %v, got %v", c.err, err)
			}
		})
	}
}
