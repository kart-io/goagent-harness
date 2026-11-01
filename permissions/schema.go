package permissions

import (
	"encoding/json"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"time"
)

type decoratedTool struct {
	interfaces.Tool
	schema string
}

func (t decoratedTool) ArgsSchema() string { return t.schema }

type decoratedTimedTool struct {
	decoratedTool
	timeout time.Duration
}

func (t decoratedTimedTool) DispatchTimeout() time.Duration { return t.timeout }

type decoratedDurableTool struct {
	decoratedTool
	policy tools.DurablePolicyProvider
}

func (t decoratedDurableTool) DurableOperationPolicy() durable.OperationPolicy {
	return t.policy.DurableOperationPolicy()
}

type decoratedDurableTimedTool struct {
	decoratedDurableTool
	timeout time.Duration
}

func (t decoratedDurableTimedTool) DispatchTimeout() time.Duration { return t.timeout }

// DecorateTools adds escalation arguments to a caller-selected set of supported
// file/command execution tools. The caller must never pass arbitrary MCP tools:
// schema decoration cannot create an enforcement boundary. Invalid/non-object
// schemas remain unchanged and therefore cannot advertise escalation.
func DecorateTools(input []interfaces.Tool) []interfaces.Tool {
	output := append([]interfaces.Tool(nil), input...)
	for i, t := range input {
		var schema map[string]any
		if t == nil || json.Unmarshal([]byte(t.ArgsSchema()), &schema) != nil || schema["type"] != "object" {
			continue
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			properties = map[string]any{}
			schema["properties"] = properties
		}
		properties["sandbox_permissions"] = map[string]any{"type": "string", "enum": []string{"workspace-write", "danger-full-access"}, "description": "Request a strictly wider file sandbox for this call only; requires justification and host approval. Does not widen network or directory access."}
		properties["justification"] = map[string]any{"type": "string", "minLength": 1, "description": "Why this call needs the requested sandbox mode; supply together with sandbox_permissions."}
		// Include both directions without replacing dependencies supplied by the tool.
		paired := map[string]any{"dependentRequired": map[string]any{"sandbox_permissions": []string{"justification"}, "justification": []string{"sandbox_permissions"}}}
		existing, _ := schema["allOf"].([]any)
		schema["allOf"] = append(existing, paired)
		encoded, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		wrapper := decoratedTool{Tool: t, schema: string(encoded)}
		if policy, ok := t.(tools.DurablePolicyProvider); ok {
			wrapped := decoratedDurableTool{decoratedTool: wrapper, policy: policy}
			if timeout, ok := t.(tools.DispatchTimeoutProvider); ok {
				output[i] = decoratedDurableTimedTool{decoratedDurableTool: wrapped, timeout: timeout.DispatchTimeout()}
			} else {
				output[i] = wrapped
			}
		} else if timeout, ok := t.(tools.DispatchTimeoutProvider); ok {
			output[i] = decoratedTimedTool{decoratedTool: wrapper, timeout: timeout.DispatchTimeout()}
		} else {
			output[i] = wrapper
		}
	}
	return output
}
