package integration_test

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/observe"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/execution"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

type planningCompatibilityState struct {
	Results []string
}

// TestPlanningCompatibility composes existing public APIs before adding planning:
// fixed slots share one durable root budget, replay agents, and resume a leaf wait.
func TestPlanningCompatibility(t *testing.T) {
	const runID = "planning-compatibility"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	policy := budget.Config{RootKey: runID, Version: "compatibility-budget-v1", MaxModelRequests: 2, MaxAgentCalls: 2}
	var calls [2]atomic.Int32
	var keys [2]atomic.Value
	var merges atomic.Int32
	var entered atomic.Int32
	bothModels := make(chan struct{})
	labels := []string{"slot-a", "slot-b"}

	// Every reconstruction opens a new store/controller/scope/graph/agent set.
	// Only the first set has model replies; any later physical request is an error.
	build := func(first bool) (context.Context, *workflow.DurableGraph[planningCompatibilityState], *budget.Controller, []*mocks.ScriptedClient, durable.Store) {
		store, err := durable.NewFileStore(dir)
		require.NoError(t, err)
		controller, err := budget.NewDurable(policy, store)
		require.NoError(t, err)
		scope, err := execution.NewScope(execution.Config{RunID: runID, Journal: store, Budget: controller})
		require.NoError(t, err)
		bound, stop, err := scope.Bind(ctx)
		require.NoError(t, err)
		t.Cleanup(stop)
		models := make([]*mocks.ScriptedClient, 2)
		branches := make([]workflow.Branch[planningCompatibilityState], 2)
		for i, label := range labels {
			models[i] = mocks.NewScriptedClient()
			if first {
				models[i] = mocks.NewScriptedClient(mocks.ScriptedRule{
					Name:  label,
					Match: mocks.RequestContains("frozen assignment for " + label),
					Respond: func(ctx context.Context, _ *llm.CompletionRequest) (mocks.ScriptedReply, error) {
						if entered.Add(1) == 2 {
							close(bothModels)
						}
						select {
						case <-bothModels:
							return mocks.ScriptedReply{Text: label + " result"}, nil
						case <-ctx.Done():
							return mocks.ScriptedReply{}, ctx.Err()
						}
					},
				})
			}
			configured, err := execution.ConfigureBuilder(builder.NewSimpleBuilder(models[i]), scope, label)
			require.NoError(t, err)
			agent, err := configured.WithMaxIterations(1).Build()
			require.NoError(t, err)
			branches[i] = workflow.Branch[planningCompatibilityState]{Name: label, Run: func(ctx context.Context, in planningCompatibilityState) (planningCompatibilityState, error) {
				calls[i].Add(1)
				key := durable.ExecutionKey(ctx)
				if key == "" || key == runID {
					return in, fmt.Errorf("%s did not derive its execution key", label)
				}
				if old := keys[i].Load(); old == nil {
					keys[i].Store(key)
				} else if old != key {
					return in, fmt.Errorf("%s execution key changed after reconstruction", label)
				}
				if inherited, ok := budget.FromContext(ctx); !ok || inherited != controller || inherited.Configuration() != policy || observe.RunIDFromContext(ctx) != runID || !durable.HasJournal(ctx) {
					return in, fmt.Errorf("%s lost root scope, journal or budget", label)
				}
				out, err := agent.AsRunnable().Invoke(ctx, "frozen assignment for "+label)
				if err != nil {
					return in, err
				}
				if i == 1 {
					_, err = durable.AwaitInput(ctx, "accept-result", json.RawMessage(`{"slot":"slot-b","question":"Accept this result?"}`), func(raw json.RawMessage) error {
						var approved bool
						if json.Unmarshal(raw, &approved) != nil || !approved {
							return fmt.Errorf("approval must be true")
						}
						return nil
					})
					if err != nil {
						return in, err
					}
				}
				in.Results[i] = fmt.Sprint(out.Result)
				return in, nil
			}}
		}
		g := workflow.NewGraph[planningCompatibilityState](workflow.Config{MaxConcurrency: 2})
		err = g.AddParallel("execute", workflow.ParallelConfig[planningCompatibilityState]{
			Branches: branches,
			Clone: func(in planningCompatibilityState) planningCompatibilityState {
				return planningCompatibilityState{Results: slices.Clone(in.Results)}
			},
			Merge: func(_ context.Context, in planningCompatibilityState, outputs []planningCompatibilityState) (planningCompatibilityState, error) {
				merges.Add(1)
				if len(outputs) != 2 || outputs[0].Results[1] != "" || outputs[1].Results[0] != "" {
					return in, fmt.Errorf("fixed slot outputs were not isolated")
				}
				for i := range outputs {
					in.Results[i] = outputs[i].Results[i]
				}
				return in, nil
			},
		})
		require.NoError(t, err)
		require.NoError(t, g.AddEdge(constants.WorkflowStart, "execute"))
		require.NoError(t, g.AddEdge("execute", constants.WorkflowEnd))
		run, err := g.CompileDurable(bound, workflow.DurableConfig{Store: store, Version: "compatibility-v1", StateVersion: "compatibility-state-v1", Components: map[string]string{"members": "slot-a/1,slot-b/1"}})
		require.NoError(t, err)
		return bound, run, controller, models, store
	}

	bound, run, controller, models, store := build(true)
	initial := planningCompatibilityState{Results: make([]string, 2)}
	waiting, err := run.Run(bound, runID, initial, "")
	require.ErrorIs(t, err, agentErrors.ErrApprovalRequired)
	require.True(t, waiting.Waiting)
	require.False(t, waiting.Done)
	require.Len(t, waiting.Interrupts, 1)
	point := waiting.Interrupts[0]
	require.Equal(t, "accept-result", point.ID)
	require.NotEqual(t, waiting.Key, point.Key, "the host must answer the leaf interrupt")
	require.Equal(t, []string{"", ""}, waiting.State.Results, "waiting branches do not merge partial results")
	require.Equal(t, []string{"", ""}, initial.Results, "slot writes must not mutate the caller's state")
	require.Zero(t, merges.Load())
	require.EqualValues(t, 1, calls[0].Load(), "a waiting slot must not cancel its sibling")
	require.EqualValues(t, 1, calls[1].Load())
	require.EqualValues(t, 2, entered.Load(), "both fixed slots must actually execute concurrently")
	require.NotEqual(t, keys[0].Load(), keys[1].Load())
	for i, model := range models {
		require.Equal(t, 1, model.Calls())
		scoped := durable.WithJournal(durable.WithExecutionKey(bound, durable.ChildKey(keys[i].Load().(string), "agent/"+labels[i])), store)
		record, err := durable.InspectOperation(scoped, "model/answer/0")
		require.NoError(t, err)
		require.Equal(t, "completed", record.Status, "each agent must persist its actual model result")
	}
	before, err := controller.Snapshot(bound)
	require.NoError(t, err)
	require.Equal(t, 2, before.ReservedRequests)
	require.Equal(t, 2, before.KnownUsageEntries)
	require.Equal(t, 4, before.KnownTokens.TotalTokens)
	require.Zero(t, before.UnknownUsageEntries)

	bound, run, controller, models, _ = build(false)
	restored, err := run.Run(bound, runID, planningCompatibilityState{Results: []string{"ignored", "ignored"}}, "")
	require.ErrorIs(t, err, agentErrors.ErrApprovalRequired)
	require.Equal(t, point, restored.Interrupts[0])
	require.EqualValues(t, 1, calls[0].Load())
	require.EqualValues(t, 1, calls[1].Load(), "reopening an unanswered wait must not enter either slot")
	invalid, err := run.Resume(bound, runID, workflow.ResumeRequest{Key: point.Key, Input: json.RawMessage(`false`)})
	var rejected *workflow.ResumeRejectedError
	require.ErrorAs(t, err, &rejected)
	require.True(t, invalid.Waiting)
	require.True(t, invalid.Interrupts[0].Rejected)
	require.Equal(t, point.Key, invalid.Interrupts[0].Key, "invalid input must preserve the same wait")
	require.Zero(t, merges.Load())
	for _, model := range models {
		require.Zero(t, model.Calls(), "the waiting slot must replay its completed agent call")
	}

	bound, run, controller, models, _ = build(false)
	done, err := run.Resume(bound, runID, workflow.ResumeRequest{Key: point.Key, Input: json.RawMessage(`true`)})
	require.NoError(t, err)
	require.True(t, done.Done)
	require.False(t, done.Waiting)
	require.Empty(t, done.Interrupts)
	require.Equal(t, []string{"slot-a result", "slot-b result"}, done.State.Results)
	require.EqualValues(t, 1, calls[0].Load(), "the completed slot must not rerun after reconstruction")
	require.EqualValues(t, 3, calls[1].Load(), "only the waiting slot reenters for invalid and valid answers")
	require.EqualValues(t, 1, merges.Load())
	for _, model := range models {
		require.Zero(t, model.Calls())
	}
	after, err := controller.Snapshot(bound)
	require.NoError(t, err)
	require.Equal(t, policy, controller.Configuration())
	require.Equal(t, before, after, "replay and human input must not reset or consume the root budget")

	bound, run, controller, models, _ = build(false)
	replayed, err := run.Run(bound, runID, planningCompatibilityState{}, "")
	require.NoError(t, err)
	require.True(t, replayed.Done)
	require.Equal(t, done.State, replayed.State)
	require.EqualValues(t, 1, calls[0].Load())
	require.EqualValues(t, 3, calls[1].Load())
	require.EqualValues(t, 1, merges.Load())
	for _, model := range models {
		require.Zero(t, model.Calls(), "a completed run must not issue physical model requests")
	}
	after, err = controller.Snapshot(bound)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
