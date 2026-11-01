package permissions

import (
	"context"
	"encoding/json"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"testing"
	"time"
)

type timedTool struct{ *tools.BaseTool }

func (t timedTool) DispatchTimeout() time.Duration { return time.Hour }
func TestDecorateToolsSchemaAndTimeout(t *testing.T) {
	b := tools.NewBaseTool("run_command", "run", `{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`, func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) { return nil, nil })
	input := []interfaces.Tool{timedTool{b}}
	out := DecorateTools(input)
	if out[0] == input[0] {
		t.Fatal("no wrapper")
	}
	if out[0].(tools.DispatchTimeoutProvider).DispatchTimeout() != time.Hour {
		t.Fatal("lost timeout")
	}
	var schema map[string]any
	if e := json.Unmarshal([]byte(out[0].ArgsSchema()), &schema); e != nil {
		t.Fatal(e)
	}
	p := schema["properties"].(map[string]any)
	if p["sandbox_permissions"] == nil || p["justification"] == nil || p["command"] == nil {
		t.Fatal(p)
	}
	if b.ArgsSchema() == out[0].ArgsSchema() {
		t.Fatal("mutated source")
	}
}
func TestSetSessionRejectsEmptyMode(t *testing.T) {
	c := fixture(t, nil, nil)
	if e := c.SetSession(context.Background(), "s", ""); e == nil {
		t.Fatal("empty mode accepted")
	}
}
func TestExecutionContextExpires(t *testing.T) {
	c := fixture(t, nil, nil)
	var saved context.Context
	_, e := c.Around()(context.Background(), &tools.Call{}, func(ctx context.Context) (*interfaces.ToolOutput, error) { saved = ctx; return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	if saved.Err() == nil {
		t.Fatal("context remains live")
	}
}
func TestApproverCannotMutateCall(t *testing.T) {
	c := fixture(t, tools.ApproverFunc(func(_ context.Context, r tools.ApprovalRequest) (tools.ApprovalResult, error) {
		r.Call.Args["path"] = "evil"
		return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
	}), nil)
	call := escalation()
	_, e := c.Around()(context.Background(), call, func(context.Context) (*interfaces.ToolOutput, error) {
		if call.Args["path"] != "a" {
			t.Fatal("aliased approval args")
		}
		return nil, nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
