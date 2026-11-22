package planning

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupsDoNotGrantExecutionDependencies(t *testing.T) {
	s, cfg, deps := graphFixture(t)
	draft, err := independentTasks(deps).Plan(t.Context(), PlanningInput{})
	require.NoError(t, err)
	draft.Groups = []TaskGroup{
		{ID: "parent", Title: "主分组"},
		{ID: "child", ParentID: "parent", Title: "子分组"},
	}
	draft.Tasks[0].GroupID = "parent"
	draft.Tasks[1].GroupID = "child"
	s.GoalDigest, err = GoalDigest(s.Goal)
	require.NoError(t, err)

	adopted, err := AdoptPlan(s, draft, cfg)
	require.NoError(t, err)
	require.Equal(t, draft.Groups, adopted.Plan.Groups)
	allocated, err := Allocate(adopted, cfg, "root")
	require.NoError(t, err)
	require.NotNil(t, allocated.Wave)
	require.Equal(t, 1, allocated.Wave.Number)
	require.Len(t, allocated.Wave.Assignments, 2)
	for i, assignment := range allocated.Wave.Assignments {
		require.Equal(t, draft.Tasks[i].GroupID, assignment.Task.GroupID)
		require.Empty(t, adopted.Plan.Tasks[i].DependsOn)
		require.Empty(t, assignment.Task.DependsOn)
	}

	draft.Groups[0].ParentID = "child"
	_, err = AdoptPlan(s, draft, cfg)
	require.ErrorAs(t, err, new(*CandidateError))
	require.ErrorContains(t, err, "存在环")
}

func TestCapabilityMismatchAndRoleCannotGrantAuthority(t *testing.T) {
	s, cfg, _ := allocationFixture(t)
	member := &cfg.Members[0]
	member.Role = "资料审阅员"
	member.Capabilities = []string{"read_report"}
	member.Claims = []ResourceClaim{{Key: "report", Mode: "read"}}
	s.Plan.Tasks[0].RequiredCapabilities = []string{"read_report"}
	before, err := Allocate(s, cfg, "root")
	require.NoError(t, err)
	require.NotNil(t, before.Wave)
	require.Len(t, before.Wave.Assignments, 1)
	require.Equal(t, member.Claims, before.Wave.Assignments[0].Claims)

	member.Role = "管理员"
	after, err := Allocate(s, cfg, "root")
	require.NoError(t, err)
	require.Equal(t, before.Wave, after.Wave)
	require.Nil(t, after.WriteFreeze)
	require.Equal(t, "read_only", after.Attempts[0].EffectClass)

	s.Plan.Tasks[0].RequiredCapabilities = []string{"write_report"}
	for _, role := range []string{"资料审阅员", "管理员"} {
		t.Run(role, func(t *testing.T) {
			member.Role = role
			rejected, err := Allocate(s, cfg, "root")
			require.ErrorAs(t, err, new(*CandidateError))
			require.ErrorContains(t, err, "成员或能力无效")
			require.Nil(t, rejected.Wave)
			require.Empty(t, rejected.Attempts)
		})
	}
}

func TestDraftRejectsBeforeAnyEffects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*State, *Config, *PlanDraft)
		reason string
	}{
		{"重复任务ID", func(_ *State, _ *Config, p *PlanDraft) {
			p.Tasks[1].ID = p.Tasks[0].ID
		}, "为空或重复"},
		{"未知任务引用", func(_ *State, _ *Config, p *PlanDraft) {
			p.Tasks[1].DependsOn[0] = "new:unknown"
			p.Tasks[1].Inputs[0].DependencyID = "new:unknown"
		}, "new:unknown"},
		{"缺少本地验收项", func(_ *State, _ *Config, p *PlanDraft) {
			p.Tasks[0].Criteria = nil
		}, "缺少验收项"},
		{"缺少目标验收覆盖", func(s *State, _ *Config, _ *PlanDraft) {
			s.Goal.Criteria = append(s.Goal.Criteria, Criterion{ID: "uncovered", Description: "核对引文"})
		}, "未完整覆盖"},
		{"缺少交付物覆盖", func(s *State, _ *Config, _ *PlanDraft) {
			s.Goal.Deliverables = append(s.Goal.Deliverables, Deliverable{
				ID: "uncovered", Description: "附录", MediaType: "application/json",
			})
		}, "未完整覆盖"},
		{"超出任务上限", func(_ *State, cfg *Config, _ *PlanDraft) {
			cfg.MaxTasks = 1
		}, "数量超出上限"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, deps := graphFixture(t)
			draft, err := deps.Planner.Plan(t.Context(), PlanningInput{})
			require.NoError(t, err)
			tc.mutate(&s, &cfg, &draft)
			cfg.MaxReplans = 1
			deps.Planner = graphPlanner(func(context.Context, PlanningInput) (PlanDraft, error) {
				return draft, nil
			})
			executor := deps.Executor
			var calls atomic.Int32
			deps.Executor = graphExecutor(func(ctx context.Context, a Assignment) (TaskResult, error) {
				calls.Add(1)
				return executor.Execute(ctx, a)
			})

			out := runFixture(t, s, cfg, deps)
			require.Zero(t, calls.Load())
			require.NotNil(t, out.Outcome)
			require.Equal(t, "blocked", out.Outcome.Status)
			require.Contains(t, out.Outcome.Reason, tc.reason)
			require.Nil(t, out.Plan)
			require.Empty(t, out.Attempts)
			require.Empty(t, out.Accepted)
		})
	}
}

func TestReviewRequiresExactCriterionCoverage(t *testing.T) {
	a, result, cfg := assignmentFixture()
	a.Task.Criteria[0].EvidenceRequired = true
	validated, err := ValidateTaskResult(a, result, cfg)
	require.NoError(t, err)
	in := ReviewInput{Assignment: &a, SubjectDigest: "subject", Results: []TaskResult{validated}}
	review := Review{
		SubjectDigest: in.SubjectDigest,
		Verdict:       "accept",
		Reason:        "已核对成果",
		Criteria: []CriterionResult{{
			CriterionID: a.Task.Criteria[0].ID,
			Status:      "pass",
			EvidenceIDs: []string{validated.Artifacts[0].ID},
		}},
	}
	accepted, err := ValidateReview(in, review)
	require.NoError(t, err)
	require.Equal(t, "accept", accepted.Verdict)
	for _, tc := range []struct {
		name   string
		mutate func(*Review)
		reason string
	}{
		{"未知验收项", func(r *Review) { r.Criteria[0].CriterionID = "unlisted" }, "验收结果"},
		{"缺少验收项", func(r *Review) { r.Criteria = nil }, "有缺项"},
		{"重复验收项", func(r *Review) { r.Criteria = append(r.Criteria, r.Criteria[0]) }, "验收结果"},
		{"缺少必需证据", func(r *Review) { r.Criteria[0].EvidenceIDs = nil }, "缺少证据"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := review.clone()
			tc.mutate(&candidate)
			out, err := ValidateReview(in, candidate)
			require.ErrorAs(t, err, new(*CandidateError))
			require.ErrorContains(t, err, tc.reason)
			require.NotEqual(t, "accept", out.Verdict)
		})
	}
}
