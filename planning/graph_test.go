package planning

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

type graphPlanner func(context.Context, PlanningInput) (PlanDraft, error)

func (f graphPlanner) Plan(c context.Context, i PlanningInput) (PlanDraft, error) { return f(c, i) }

type graphExecutor func(context.Context, Assignment) (TaskResult, error)

func (f graphExecutor) Execute(c context.Context, i Assignment) (TaskResult, error) { return f(c, i) }

type graphReviewer func(context.Context, ReviewInput) (Review, error)

func (f graphReviewer) Review(c context.Context, i ReviewInput) (Review, error) { return f(c, i) }
func graphFixture(t *testing.T) (State, Config, Dependencies) {
	s, c, a := allocationFixture(t)
	a.ID = "new:first"
	b := a.clone()
	b.ID = "new:second"
	b.DependsOn = []string{a.ID}
	b.Inputs = []InputRef{{a.ID, "out"}}
	deps := Dependencies{PlannerVersion: "p1", ReviewerVersion: "r1", Planner: graphPlanner(func(context.Context, PlanningInput) (PlanDraft, error) {
		return PlanDraft{Tasks: []TaskSpec{a, b}}, nil
	}), Executor: graphExecutor(func(_ context.Context, a Assignment) (TaskResult, error) {
		return TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "produced", Artifacts: []Artifact{{Name: "out", MediaType: "application/json", Content: []byte("1")}}}, nil
	}), Reviewer: graphReviewer(func(_ context.Context, in ReviewInput) (Review, error) {
		cs := in.Goal.Criteria
		if !in.Final {
			cs = in.Assignment.Task.Criteria
		}
		r := Review{SubjectDigest: in.SubjectDigest, Verdict: "accept", Reason: "已检查"}
		for _, c := range cs {
			r.Criteria = append(r.Criteria, CriterionResult{CriterionID: c.ID, Status: "pass", EvidenceIDs: []string{in.Results[0].Artifacts[0].ID}})
		}
		return r, nil
	})}
	return State{Goal: s.Goal}, c, deps
}

func runFixture(t *testing.T, s State, c Config, d Dependencies) State {
	g, e := NewGraph(c, d)
	require.NoError(t, e)
	store, e := durable.NewFileStore(t.TempDir())
	require.NoError(t, e)
	dc, e := DurableConfig(c, d, store)
	require.NoError(t, e)
	compiled, e := g.CompileDurable(context.Background(), dc)
	require.NoError(t, e)
	snap, e := compiled.Run(context.Background(), "root", s, "")
	require.NoError(t, e)
	require.True(t, snap.Done)
	return snap.State
}

func TestFixedGraphRunsDependencyWaves(t *testing.T) {
	s, c, d := graphFixture(t)
	out := runFixture(t, s, c, d)
	require.Equal(t, "succeeded", out.Outcome.Status, out.Outcome.Reason)
	require.Equal(t, 2, out.Waves)
	require.Len(t, out.Accepted, 2)
}

func TestGraphBoundsAllLoops(t *testing.T) {
	s, c, d := graphFixture(t)
	c.MaxReplans = 2
	d.Planner = graphPlanner(func(context.Context, PlanningInput) (PlanDraft, error) { return PlanDraft{}, nil })
	out := runFixture(t, s, c, d)
	require.Equal(t, "blocked", out.Outcome.Status)
	require.Equal(t, 2, out.Replans)
}

func compileFixture(t *testing.T, c Config, d Dependencies, store durable.Store) *workflow.DurableGraph[State] {
	t.Helper()
	g, e := NewGraph(c, d)
	require.NoError(t, e)
	dc, e := DurableConfig(c, d, store)
	require.NoError(t, e)
	compiled, e := g.CompileDurable(t.Context(), dc)
	require.NoError(t, e)
	return compiled
}

func independentTasks(d Dependencies) graphPlanner {
	return func(ctx context.Context, in PlanningInput) (PlanDraft, error) {
		p, e := d.Planner.Plan(ctx, in)
		p = p.clone()
		p.Tasks[1].DependsOn = nil
		p.Tasks[1].Inputs = nil
		return p, e
	}
}

func TestPartialWaitDoesNotReallocateWave(t *testing.T) {
	s, c, d := graphFixture(t)
	d.Planner = independentTasks(d)
	exec := d.Executor
	var success atomic.Int32
	d.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
		if a.Slot == 1 {
			_, err := durable.AwaitInput(ctx, "gate", []byte("{}"), nil)
			if err != nil {
				return TaskResult{}, err
			}
		} else {
			success.Add(1)
		}
		return exec.Execute(ctx, a)
	})
	store, e := durable.NewFileStore(t.TempDir())
	require.NoError(t, e)
	g := compileFixture(t, c, d, store)
	snap, e := g.Run(t.Context(), "root", s, "")
	require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
	require.Equal(t, 1, snap.State.Waves)
	require.Empty(t, snap.State.Accepted)
	require.Len(t, snap.Interrupts, 1)
	done, e := compileFixture(t, c, d, store).Resume(t.Context(), "root", workflow.ResumeRequest{Key: snap.Interrupts[0].Key, Input: []byte("true")})
	require.NoError(t, e)
	require.True(t, done.Done)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Equal(t, int32(1), success.Load())
	require.Equal(t, 1, done.State.Waves)
}

func TestRealFailureWinsOverWait(t *testing.T) {
	s, c, d := graphFixture(t)
	d.Planner = independentTasks(d)
	waited := make(chan struct{})
	failure := errors.New("执行服务不可用")
	d.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
		if a.Slot == 0 {
			_, err := durable.AwaitInput(ctx, "gate", []byte("{}"), nil)
			close(waited)
			return TaskResult{}, err
		}
		<-waited
		return TaskResult{}, failure
	})
	store, e := durable.NewFileStore(t.TempDir())
	require.NoError(t, e)
	g := compileFixture(t, c, d, store)
	_, e = g.Run(t.Context(), "root", s, "")
	require.ErrorIs(t, e, failure)
	require.NotErrorIs(t, e, agentErrors.ErrApprovalRequired)
	snap, e := g.Snapshot(t.Context(), "root")
	require.NoError(t, e)
	require.False(t, snap.Waiting)
	require.Empty(t, snap.State.Accepted)
	require.Equal(t, 1, snap.State.Waves)
}

func TestSingleMemberSingleSlotCompletesPlanning(t *testing.T) {
	s, c, d := graphFixture(t)
	c.Parallelism = 0
	original := d.Planner
	d.Planner = graphPlanner(func(ctx context.Context, in PlanningInput) (PlanDraft, error) {
		if in.RejectionReason == "" {
			return PlanDraft{}, nil
		}
		require.NotNil(t, in.RejectedDraft)
		return original.Plan(ctx, in)
	})
	exec := d.Executor
	var active, peak atomic.Int32
	d.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
		n := active.Add(1)
		defer active.Add(-1)
		peak.CompareAndSwap(0, n)
		if a.Task.ID == "t-000002" {
			_, e := durable.AwaitInput(ctx, "check", []byte("{}"), nil)
			if e != nil {
				return TaskResult{}, e
			}
		}
		return exec.Execute(ctx, a)
	})
	store, e := durable.NewFileStore(t.TempDir())
	require.NoError(t, e)
	snap, e := compileFixture(t, c, d, store).Run(t.Context(), "root", s, "")
	require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
	require.Len(t, snap.State.Accepted, 1)
	require.Equal(t, 1, snap.State.Replans)
	done, e := compileFixture(t, c, d, store).Resume(t.Context(), "root", workflow.ResumeRequest{Key: snap.Interrupts[0].Key, Input: []byte("true")})
	require.NoError(t, e)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Equal(t, int32(1), peak.Load())
	require.Len(t, done.State.Accepted, 2)
}

func TestPlanningGraphReplaysAgentPlannerAfterNodeCrash(t *testing.T) {
	s, c, d := graphFixture(t)
	f := newAdapterFixture(t)
	p, e := d.Planner.Plan(t.Context(), PlanningInput{})
	require.NoError(t, e)
	raw, e := json.Marshal(p)
	require.NoError(t, e)
	model := mocks.NewScriptedClient(mocks.TextReply("计划", string(raw)))
	build := func(crash bool) *workflow.DurableGraph[State] {
		a, e := NewAgentPlanner(f.config(model, nil))
		require.NoError(t, e)
		d.Planner = graphPlanner(func(ctx context.Context, in PlanningInput) (PlanDraft, error) {
			p, e := a.Plan(ctx, in)
			if e == nil && crash {
				return p, errAdapterCrash
			}
			return p, e
		})
		return compileFixture(t, c, d, f.store)
	}
	_, e = build(true).Run(f.ctx, "run", s, "")
	require.ErrorIs(t, e, errAdapterCrash)
	require.Equal(t, 1, model.Calls())
	require.NoError(t, f.budget.Reserve(f.ctx, "另一个模型请求"))
	done, e := build(false).Run(f.ctx, "run", s, "")
	require.NoError(t, e)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Equal(t, 1, model.Calls())
}

func TestGraphExecutorScopeAndCandidateReplay(t *testing.T) {
	s, c, d := graphFixture(t)
	reviewer := d.Reviewer
	planner := d.Planner
	exec := d.Executor
	var invalidated bool
	calls := 0
	d.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
		if durable.ExecutionKey(ctx) != a.AttemptKey {
			return TaskResult{}, errors.New("槽执行上下文未绑定冻结的尝试键")
		}
		return exec.Execute(ctx, a)
	})
	d.Reviewer = graphReviewer(func(ctx context.Context, in ReviewInput) (Review, error) {
		if !invalidated {
			invalidated = true
			return Review{}, invalid("审查 JSON 无效")
		}
		return reviewer.Review(ctx, in)
	})
	d.Planner = graphPlanner(func(ctx context.Context, in PlanningInput) (PlanDraft, error) {
		calls++
		if calls == 1 {
			return planner.Plan(ctx, in)
		}
		require.NotNil(t, in.Current)
		require.Contains(t, in.RejectionReason, "JSON")
		return PlanDraft{Tasks: in.Current.Tasks}, nil
	})
	out := runFixture(t, s, c, d)
	require.Equal(t, "succeeded", out.Outcome.Status)
	require.Equal(t, 1, out.Replans)
	require.Equal(t, "rejected", out.Attempts[0].Status)
	require.Equal(t, 2, calls)
}

func TestOversizeDraftDiscardedBeforeRevise(t *testing.T) {
	s, c, d := graphFixture(t)
	c.MaxStateBytes = 64 * 1024
	original := d.Planner
	calls := 0
	d.Planner = graphPlanner(func(ctx context.Context, in PlanningInput) (PlanDraft, error) {
		calls++
		if calls == 1 {
			return PlanDraft{Rationale: strings.Repeat("x", c.MaxStateBytes)}, nil
		}
		require.Nil(t, in.RejectedDraft)
		require.Contains(t, in.RejectionReason, "上限")
		return original.Plan(ctx, in)
	})
	out := runFixture(t, s, c, d)
	require.Equal(t, "succeeded", out.Outcome.Status)
	require.Equal(t, 2, calls)
}

func TestPlannerInputBoundBeforeInvocation(t *testing.T) {
	s, c, d := graphFixture(t)
	c.MaxStateBytes = 7000
	c.Members[0].Description = strings.Repeat("x", c.MaxStateBytes)
	calls := 0
	d.Planner = graphPlanner(func(context.Context, PlanningInput) (PlanDraft, error) { calls++; return PlanDraft{}, nil })
	out := runFixture(t, s, c, d)
	require.Equal(t, "blocked", out.Outcome.Status)
	require.Zero(t, calls)
}

func TestGraphStopsAtWaveAndAttemptBounds(t *testing.T) {
	s, c, d := graphFixture(t)
	t.Run("waves", func(t *testing.T) {
		limited := c
		limited.MaxWaves = 1
		out := runFixture(t, s, limited, d)
		require.Equal(t, "blocked", out.Outcome.Status)
		require.Equal(t, 1, out.Waves)
		require.Len(t, out.Accepted, 1)
	})
	t.Run("attempts", func(t *testing.T) {
		limited := c
		limited.MaxTaskAttempts = 2
		deps := d
		deps.Reviewer = graphReviewer(func(_ context.Context, in ReviewInput) (Review, error) {
			a := in.Assignment
			artifact := in.Results[0].Artifacts[0]
			return Review{SubjectDigest: in.SubjectDigest, Verdict: "retry", Reason: "数值错误", Criteria: []CriterionResult{{CriterionID: "local", Status: "fail"}}, Issues: []Issue{{ID: "i", Category: "value_mismatch", Severity: "error", TaskID: a.Task.ID, AttemptKey: a.AttemptKey, ArtifactID: artifact.ID, Location: "/", Reason: "需要重新计算"}}, Repair: &RepairDecision{Action: "retry", IssueIDs: []string{"i"}, TargetTaskIDs: []string{a.Task.ID}}}, nil
		})
		out := runFixture(t, s, limited, deps)
		require.Equal(t, "blocked", out.Outcome.Status)
		require.Len(t, out.Attempts, 2)
		require.Zero(t, out.Replans)
		require.Nil(t, out.Wave)
		require.Empty(t, out.Accepted)
	})
}
func TestGraphRejectsTypedNilDependencies(t *testing.T) {
	_, c, d := graphFixture(t)
	for _, change := range []func(*Dependencies){func(d *Dependencies) { d.Planner = graphPlanner(nil) }, func(d *Dependencies) { d.Reviewer = graphReviewer(nil) }, func(d *Dependencies) { d.Executor = graphExecutor(nil) }, func(d *Dependencies) { d.GoalResolver = graphResolver(nil); d.GoalResolverVersion = "1" }, func(d *Dependencies) { d.PlannerVersion = " " }, func(d *Dependencies) { d.ReviewerVersion = " " }} {
		bad := d
		change(&bad)
		_, e := NewGraph(c, bad)
		require.Error(t, e)
	}
}
func TestGraphChecksFrozenGoalContextBeforeCallingExecutor(t *testing.T) {
	s, c, _ := allocationFixture(t)
	c, e := NormalizeConfig(c)
	require.NoError(t, e)
	s, e = Allocate(s, c, "root")
	require.NoError(t, e)
	s.Wave.Assignments[0].GoalContext.Objective = "替换目标"
	calls := 0
	f := planningGraph{cfg: c, deps: Dependencies{Executor: graphExecutor(func(context.Context, Assignment) (TaskResult, error) { calls++; return TaskResult{}, nil })}}
	_, e = f.parallel().Branches[0].Run(t.Context(), s)
	require.Error(t, e)
	require.Zero(t, calls)
}
