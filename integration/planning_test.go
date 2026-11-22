package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/builder"
	corejson "github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/execution"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

const planningRoot = "planning-integration"
const privateSentinel = "MEM_RECALL_ONLY"

type jointPlanner func(context.Context, p.PlanningInput) (p.PlanDraft, error)

func (f jointPlanner) Plan(ctx context.Context, in p.PlanningInput) (p.PlanDraft, error) {
	return f(ctx, in)
}

type jointExecutor func(context.Context, p.Assignment) (p.TaskResult, error)

func (f jointExecutor) Execute(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
	return f(ctx, a)
}

type jointReviewer func(context.Context, p.ReviewInput) (p.Review, error)

func (f jointReviewer) Review(ctx context.Context, i p.ReviewInput) (p.Review, error) {
	return f(ctx, i)
}

func jointFixture(t *testing.T) (p.State, p.Config, p.TaskSpec) {
	t.Helper()
	source := []byte(`{"name":"BUS_SOURCE"}`)
	digest, err := p.SourceDigest(source)
	require.NoError(t, err)
	goal := p.GoalSpec{ID: "goal", Revision: 1, Objective: "编写咖啡报告", Scope: p.Scope{Included: []string{"咖啡"}}, Output: p.OutputProfile{Language: "zh", Format: "json"}, Criteria: []p.Criterion{{ID: "g", Description: "完整"}}, Deliverables: []p.Deliverable{{ID: "doc", Description: "报告", MediaType: "application/json"}}, Inputs: []p.SourceInput{{ID: "source", Title: "咖啡", MediaType: "application/json", Origin: "host", Digest: digest, Content: source}}}
	cfg := p.Config{Version: "1", MaxReplans: 1, Members: []p.Member{{ID: "m", Version: "1", ManifestDigest: "m-v1", EffectClass: "read_only"}}}
	task := p.TaskSpec{ID: "new:report", MemberID: "m", Instruction: "GEN_OUTPUT", GoalInputIDs: []string{"source"}, CriterionIDs: []string{"g"}, Criteria: []p.Criterion{{ID: "local", Description: "完成"}}, Outputs: []p.OutputSpec{{Name: "out", MediaType: "application/json", DeliverableID: "doc", MaxBytes: 100}}}
	initial, err := p.PrepareInitialState(p.State{Goal: goal}, cfg)
	require.NoError(t, err)
	return initial, cfg, task
}
func jointResult(a p.Assignment) p.TaskResult {
	return p.TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "produced", Artifacts: []p.Artifact{{Name: "out", MediaType: "application/json", Content: []byte(`"报告"`), Sources: []p.EvidenceRef{{SourceID: "source", LocatorKind: "json_pointer", Locator: "/name"}}}}}
}
func jointReview(in p.ReviewInput) p.Review {
	criteria := in.Goal.Criteria
	if !in.Final {
		criteria = in.Assignment.Task.Criteria
	}
	r := p.Review{SubjectDigest: in.SubjectDigest, Verdict: "accept", Reason: "已检查"}
	for _, c := range criteria {
		r.Criteria = append(r.Criteria, p.CriterionResult{CriterionID: c.ID, Status: "pass", EvidenceIDs: []string{in.Results[0].Artifacts[0].ID}})
	}
	return r
}

type jointHost struct {
	ctx        context.Context
	journal    durable.Store
	scope      *execution.Scope
	log        session.Store
	mem        *memory.Memory
	controller *budget.Controller
}

func openJointHost(t *testing.T, dir string) *jointHost {
	t.Helper()
	fs, e := durable.NewFileStore(dir + "/journal")
	require.NoError(t, e)
	ms, e := durable.NewFileStore(dir + "/memory")
	require.NoError(t, e)
	m, e := memory.New(memory.Config{Durable: ms})
	require.NoError(t, e)
	log, e := session.NewFileStore(dir + "/session")
	require.NoError(t, e)
	ctrl, e := budget.NewDurable(budget.Config{Version: "1", RootKey: planningRoot, MaxModelRequests: 16}, fs)
	require.NoError(t, e)
	scope, e := execution.NewScope(execution.Config{RunID: planningRoot, SessionID: "s", Journal: fs, Budget: ctrl})
	require.NoError(t, e)
	ctx, stop, e := scope.Bind(memory.WithNamespace(t.Context(), "tenant", "alice"))
	require.NoError(t, e)
	t.Cleanup(stop)
	return &jointHost{ctx, fs, scope, log, m, ctrl}
}
func (h *jointHost) role(id string, client *mocks.ScriptedClient) p.AgentConfig {
	return p.AgentConfig{Version: "1", Factory: func(ctx context.Context) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		var out tools.OwnedRunnable[any, *builder.AgentOutput]
		b, e := execution.ConfigureBuilder(builder.NewSimpleBuilder(client), h.scope, id)
		if e != nil {
			return out, e
		}
		a, e := b.WithSessionID(id).WithSessionLog(h.log).WithLongTermMemory(h.mem).Build()
		if e != nil {
			return out, e
		}
		out.Runnable = a.AsRunnable()
		out.Close = func(context.Context) error { return nil }
		return out, nil
	}}
}
func (h *jointHost) graph(t *testing.T, c p.Config, d p.Dependencies) *workflow.DurableGraph[p.State] {
	t.Helper()
	g, e := p.NewGraph(c, d)
	require.NoError(t, e)
	dc, e := p.DurableConfig(c, d, h.journal)
	require.NoError(t, e)
	dg, e := g.CompileDurable(h.ctx, dc)
	require.NoError(t, e)
	return dg
}
func jointJSON(t *testing.T, x any) string {
	t.Helper()
	b, e := json.Marshal(x)
	require.NoError(t, e)
	return string(b)
}
func jointPlanClient(t *testing.T, task p.TaskSpec) *mocks.ScriptedClient {
	reply := jointJSON(t, p.PlanDraft{Tasks: []p.TaskSpec{task}})
	return mocks.NewScriptedClient(mocks.TextReply("初始规划", reply), mocks.TextReply("修订规划", reply))
}
func jointReviewClient() *mocks.ScriptedClient {
	rule := mocks.ScriptedRule{Name: "审查", Respond: func(_ context.Context, req *llm.CompletionRequest) (mocks.ScriptedReply, error) {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			_, payload, ok := strings.Cut(req.Messages[i].Content, "\n")
			if !ok {
				continue
			}
			var in p.ReviewInput
			if json.Unmarshal([]byte(payload), &in) != nil || in.SubjectDigest == "" {
				continue
			}
			if len(in.Results) == 0 || len(in.Results[0].Artifacts) == 0 {
				return mocks.ScriptedReply{}, fmt.Errorf("审查缺少任务产物: %+v", in.Results)
			}
			data, e := json.Marshal(jointReview(in))
			return mocks.ScriptedReply{Text: string(data)}, e
		}
		return mocks.ScriptedReply{}, errors.New("未找到审查输入")
	}}
	return mocks.NewScriptedClient(rule, rule)
}
func jointDeps(t *testing.T, h *jointHost, pc, rc *mocks.ScriptedClient) p.Dependencies {
	planner, e := p.NewAgentPlanner(h.role("planner", pc))
	require.NoError(t, e)
	reviewer, e := p.NewAgentReviewer(h.role("reviewer", rc))
	require.NoError(t, e)
	return p.Dependencies{Planner: planner, Reviewer: reviewer, PlannerVersion: "1", ReviewerVersion: "1", Executor: jointExecutor(func(_ context.Context, a p.Assignment) (p.TaskResult, error) { return jointResult(a), nil })}
}
func jointSeed(t *testing.T, h *jointHost) string {
	t.Helper()
	r, e := h.mem.Remember(h.ctx, memory.Record{Content: "编写咖啡报告 " + privateSentinel, Importance: 1})
	require.NoError(t, e)
	return r.ID
}
func TestPlanningDistinguishesSourcesRecallAndGeneratedOutput(t *testing.T) {
	h := openJointHost(t, t.TempDir())
	id := jointSeed(t, h)
	s, c, task := jointFixture(t)
	pc, rc := jointPlanClient(t, task), jointReviewClient()
	d := jointDeps(t, h, pc, rc)
	planner := d.Planner
	var planKey string
	d.Planner = jointPlanner(func(ctx context.Context, in p.PlanningInput) (p.PlanDraft, error) {
		planKey = durable.ExecutionKey(ctx)
		data := jointJSON(t, in)
		require.Contains(t, data, "BUS_SOURCE")
		require.NotContains(t, data, privateSentinel)
		return planner.Plan(ctx, in)
	})
	g := h.graph(t, c, d)
	snap, e := g.Run(h.ctx, planningRoot, s, "")
	require.NoError(t, e)
	require.Equal(t, "succeeded", snap.State.Outcome.Status)
	require.Len(t, pc.Requests(), 1)
	require.Contains(t, jointJSON(t, pc.Requests()[0].Messages), privateSentinel)
	roleKey := durable.ChildKey(planKey, "tool/planning_plan")
	sessionKey := durable.ChildKey(roleKey, "agent/planner")
	for _, op := range []struct{ parent, id string }{{planKey, "tool/planning_plan"}, {sessionKey, "model/answer/0"}, {sessionKey, "memory/recall"}, {sessionKey, "memory/use/0"}} {
		rec, e := durable.InspectOperation(durable.WithExecutionKey(h.ctx, op.parent), op.id)
		require.NoError(t, e)
		require.NotNil(t, rec, op.id)
		require.NotEmpty(t, rec.InputHash)
		require.NotContains(t, jointJSON(t, rec), privateSentinel)
	}
	ids, e := h.log.List(h.ctx)
	require.NoError(t, e)
	require.NotEmpty(t, ids)
	for _, role := range ids {
		events, e := h.log.Read(h.ctx, role, 0)
		require.NoError(t, e)
		require.NotEmpty(t, events)
		require.NotContains(t, jointJSON(t, events), privateSentinel)
	}
	before := jointJSON(t, snap.State)
	require.Contains(t, before, "BUS_SOURCE")
	require.Contains(t, before, "GEN_OUTPUT")
	require.NotContains(t, before, privateSentinel)
	require.NoError(t, h.mem.Forget(h.ctx, id))
	after, e := g.Snapshot(h.ctx, planningRoot)
	require.NoError(t, e)
	require.Equal(t, before, jointJSON(t, after.State))
}
func TestPlanningForgetBetweenRecallAndNextRequest(t *testing.T) {
	dir := t.TempDir()
	h := openJointHost(t, dir)
	id := jointSeed(t, h)
	s, c, task := jointFixture(t)
	pc, rc := jointPlanClient(t, task), jointReviewClient()
	d := jointDeps(t, h, pc, rc)
	real := d.Planner
	fault := errors.New("候选已返回，节点尚未保存")
	d.Planner = jointPlanner(func(ctx context.Context, in p.PlanningInput) (p.PlanDraft, error) {
		out, e := real.Plan(ctx, in)
		if e != nil {
			return out, e
		}
		return out, fault
	})
	_, e := h.graph(t, c, d).Run(h.ctx, planningRoot, s, "")
	require.ErrorIs(t, e, fault)
	require.Len(t, pc.Requests(), 1)
	require.Contains(t, jointJSON(t, pc.Requests()[0].Messages), privateSentinel)
	require.Empty(t, rc.Requests())
	require.NoError(t, h.mem.Forget(h.ctx, id))
	h = openJointHost(t, dir)
	pc = mocks.NewScriptedClient()
	d = jointDeps(t, h, pc, rc)
	snap, e := h.graph(t, c, d).Run(h.ctx, planningRoot, s, "")
	require.NoError(t, e)
	require.Equal(t, "succeeded", snap.State.Outcome.Status)
	require.Empty(t, pc.Requests())
	require.Len(t, rc.Requests(), 2)
	for _, req := range rc.Requests() {
		require.Contains(t, jointJSON(t, req.Messages), "BUS_SOURCE")
		require.NotContains(t, jointJSON(t, req.Messages), privateSentinel)
	}
}
func TestRecalledMemoryCannotBecomeAuthorizedBusinessSource(t *testing.T) {
	h := openJointHost(t, t.TempDir())
	id := jointSeed(t, h)
	s, c, task := jointFixture(t)
	task.GoalInputIDs = []string{id}
	pc, rc := jointPlanClient(t, task), jointReviewClient()
	d := jointDeps(t, h, pc, rc)
	effects := 0
	d.Executor = jointExecutor(func(_ context.Context, a p.Assignment) (p.TaskResult, error) { effects++; return jointResult(a), nil })
	snap, e := h.graph(t, c, d).Run(h.ctx, planningRoot, s, "")
	require.NoError(t, e)
	require.Equal(t, "blocked", snap.State.Outcome.Status)
	require.NotEmpty(t, pc.Requests())
	require.Contains(t, jointJSON(t, pc.Requests()[0].Messages), privateSentinel)
	require.Contains(t, jointJSON(t, pc.Requests()[len(pc.Requests())-1].Messages), "目标引用无效")
	require.Zero(t, effects)
	require.Empty(t, snap.State.Attempts)
	require.Empty(t, rc.Requests())
	require.Equal(t, s.Goal.Inputs, snap.State.Goal.Inputs)
}

func TestForgetCannotReleaseUnknownWriteBarrier(t *testing.T) {
	dir := t.TempDir()
	h := openJointHost(t, dir)
	id := jointSeed(t, h)
	s, c, task := jointFixture(t)
	c.Members[0].EffectClass = "write"
	c.Members[0].Claims = []p.ResourceClaim{{Key: "report", Mode: "write"}}
	pc, rc := jointPlanClient(t, task), jointReviewClient()
	writes := 0
	var attempt string
	execute := jointExecutor(func(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
		if attempt != "" && attempt != a.AttemptKey {
			return p.TaskResult{}, fmt.Errorf("写尝试身份改变")
		}
		attempt = a.AttemptKey
		_, e := durable.Do(ctx, "commit-report", []byte(`{"name":"report"}`), durable.OperationPolicy{MaxAttempts: 1}, func(context.Context) (corejson.RawMessage, error) { writes++; return nil, agentErrors.ErrEffectUnknown })
		return p.TaskResult{}, e
	})
	run := func(h *jointHost) (p.State, error) {
		d := jointDeps(t, h, pc, rc)
		d.Executor = execute
		g := h.graph(t, c, d)
		_, e := g.Run(h.ctx, planningRoot, s, "")
		snap, se := g.Snapshot(h.ctx, planningRoot)
		require.NoError(t, se)
		return snap.State, e
	}
	before, e := run(h)
	require.Error(t, e)
	require.Equal(t, 1, writes)
	require.Len(t, before.Attempts, 1)
	require.Equal(t, attempt, before.Attempts[0].ExecutionKey)
	require.NoError(t, h.mem.Forget(h.ctx, id))
	h = openJointHost(t, dir)
	after, e := run(h)
	require.ErrorIs(t, e, agentErrors.ErrOperationAttention)
	require.Equal(t, before.Attempts, after.Attempts)
	require.Equal(t, before.Wave, after.Wave)
	require.Equal(t, 1, writes)
	require.Len(t, pc.Requests(), 1)
}
