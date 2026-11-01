package permissions

import (
	"context"
	"encoding/json"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
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

type durableSchemaTool struct {
	*tools.BaseTool
	policy durable.OperationPolicy
}

func (t durableSchemaTool) DurableOperationPolicy() durable.OperationPolicy { return t.policy }

type timedDurableSchemaTool struct{ durableSchemaTool }

func (t timedDurableSchemaTool) DispatchTimeout() time.Duration { return 17 * time.Second }
func TestDecorateToolsPreservesExplicitDurablePolicy(t *testing.T) {
	reconcile := func(context.Context) (json.RawMessage, bool, error) { return json.RawMessage(`"confirmed"`), true, nil }
	policy := durable.OperationPolicy{MaxAttempts: 7, RetryFailed: true, Resumable: true, Reconcile: reconcile}
	base := tools.NewBaseTool("custom_effect", "effect", `{"type":"object","properties":{}}`, func(context.Context, *interfaces.ToolInput) (*interfaces.ToolOutput, error) { return nil, nil })
	for _, tool := range []interfaces.Tool{durableSchemaTool{base, policy}, timedDurableSchemaTool{durableSchemaTool{base, policy}}} {
		decorated := DecorateTools([]interfaces.Tool{tool})[0]
		provider, ok := decorated.(tools.DurablePolicyProvider)
		require.True(t, ok, "decoration must retain an explicit durable policy")
		got := provider.DurableOperationPolicy()
		require.Equal(t, policy.MaxAttempts, got.MaxAttempts)
		require.Equal(t, policy.RetryFailed, got.RetryFailed)
		require.Equal(t, policy.Resumable, got.Resumable)
		require.NotNil(t, got.Reconcile)
		value, found, err := got.Reconcile(context.Background())
		require.NoError(t, err)
		require.True(t, found)
		require.JSONEq(t, `"confirmed"`, string(value))
		if timeout, ok := tool.(tools.DispatchTimeoutProvider); ok {
			require.Equal(t, timeout.DispatchTimeout(), decorated.(tools.DispatchTimeoutProvider).DispatchTimeout())
		}
	}
	plain := DecorateTools([]interfaces.Tool{base})[0]
	_, ok := plain.(tools.DurablePolicyProvider)
	require.False(t, ok, "do not invent a durable policy for unrelated tools")
}
