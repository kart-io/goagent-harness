package planning

import (
	"context"
	"strings"
	"testing"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/core/durable"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
)

func TestGoalAnswerBindsRequestAndQuestionSet(t *testing.T) {
	s, c, _ := allocationFixture(t)
	s.Request = &GoalRequest{Text: "目标"}
	s.GoalRounds = 1
	s.Resolution = &GoalResolution{Questions: []GoalQuestion{{ID: "1/scope", Field: "scope", Prompt: "范围？"}}}
	d, e := digestJSON(s.Request)
	require.NoError(t, e)
	_, e = parseAnswers([]byte(`{"request_digest":"wrong","round":1,"answers":[]}`), s, c, d)
	require.Error(t, e)
	_, e = parseAnswers([]byte(`{"request_digest":"`+d+`","round":1,"answers":[{"question_id":"1/scope","value":"A"}]}`), s, c, d)
	require.NoError(t, e)
}

type graphResolver func(context.Context, GoalResolveInput) (GoalResolution, error)

func (f graphResolver) Resolve(ctx context.Context, in GoalResolveInput) (GoalResolution, error) {
	return f(ctx, in)
}

func TestGoalClarificationConfirmationAcrossRestarts(t *testing.T) {
	s, c, d := graphFixture(t)
	goal := s.Goal
	req := &GoalRequest{Text: "写报告", Inputs: goal.Inputs, Initial: &GoalSpec{ID: "host-goal", Revision: 7}}
	calls := 0
	d.GoalResolverVersion = "resolver1"
	d.GoalResolver = graphResolver(func(_ context.Context, in GoalResolveInput) (GoalResolution, error) {
		calls++
		require.Equal(t, calls, in.Round)
		res := GoalResolution{Candidate: goal.clone()}
		res.Candidate.ID = "model"
		res.Candidate.Revision = 99
		if calls == 1 {
			res.Questions = []GoalQuestion{{ID: "model-wait-key", Field: "scope", Prompt: "覆盖什么范围？"}}
		} else {
			require.Len(t, in.Answers, 1)
			require.NotNil(t, in.Previous)
		}
		return res, nil
	})
	store, e := durable.NewFileStore(t.TempDir())
	require.NoError(t, e)
	snap, e := compileFixture(t, c, d, store).Run(t.Context(), "run", State{Request: req}, "")
	require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
	q := snap.Interrupts[0]
	require.Equal(t, "goal/questions/1", q.ID)
	require.Equal(t, 1, snap.State.GoalRounds)
	require.Equal(t, "1/scope", snap.State.Resolution.Questions[0].ID)
	_, e = compileFixture(t, c, d, store).Resume(t.Context(), "run", workflow.ResumeRequest{Key: q.Key, Input: []byte("{}")})
	var rejected *workflow.ResumeRejectedError
	require.ErrorAs(t, e, &rejected)
	require.Equal(t, 1, calls)
	hash, e := digestJSON(req)
	require.NoError(t, e)
	raw, e := json.Marshal(map[string]any{"request_digest": hash, "round": 1, "answers": []GoalAnswer{{QuestionID: "1/scope", Value: "报告"}}})
	require.NoError(t, e)
	snap, e = compileFixture(t, c, d, store).Resume(t.Context(), "run", workflow.ResumeRequest{Key: q.Key, Input: raw})
	require.ErrorIs(t, e, agentErrors.ErrApprovalRequired)
	require.Equal(t, 2, calls)
	cand := snap.State.Resolution.Candidate
	require.Equal(t, "host-goal", cand.ID)
	require.Equal(t, 7, cand.Revision)
	require.False(t, cand.Assumptions[0].Confirmed)
	digest, e := GoalDigest(cand)
	require.NoError(t, e)
	raw, e = json.Marshal(map[string]any{"approved": true, "candidate_digest": digest})
	require.NoError(t, e)
	done, e := compileFixture(t, c, d, store).Resume(t.Context(), "run", workflow.ResumeRequest{Key: snap.Interrupts[0].Key, Input: raw})
	require.NoError(t, e)
	require.Equal(t, "succeeded", done.State.Outcome.Status)
	require.Equal(t, 2, calls)
	require.True(t, done.State.Goal.Assumptions[0].Confirmed)
}

func TestResolutionCandidateErrorsStopAtBound(t *testing.T) {
	_, c, d := graphFixture(t)
	d.GoalResolverVersion = "v"
	calls := 0
	d.GoalResolver = graphResolver(func(_ context.Context, in GoalResolveInput) (GoalResolution, error) {
		calls++
		require.Equal(t, calls, in.Round)
		return GoalResolution{}, invalid("缺少截止时间")
	})
	out := runFixture(t, State{Request: &GoalRequest{Text: "报告"}}, c, d)
	require.Equal(t, 4, calls)
	require.Equal(t, 4, out.GoalRounds)
	require.Contains(t, out.Outcome.Reason, "截止时间")
}
func TestGoalResolutionListsMissingHostParameters(t *testing.T) {
	s, c, d := graphFixture(t)
	c.RequiredGoalParameters = []string{"release_cutoff"}
	c.MaxGoalRounds = 1
	d.GoalResolverVersion = "v"
	d.GoalResolver = graphResolver(func(context.Context, GoalResolveInput) (GoalResolution, error) {
		return GoalResolution{Candidate: s.Goal}, nil
	})
	out := runFixture(t, State{Request: &GoalRequest{Text: "报告", Inputs: s.Goal.Inputs}}, c, d)
	require.Contains(t, out.Outcome.Reason, "release_cutoff")
}

func TestOversizeFreshRequestStopsWithoutResolving(t *testing.T) {
	_, c, d := graphFixture(t)
	c.MaxStateBytes = 2048
	calls := 0
	d.GoalResolverVersion = "v"
	d.GoalResolver = graphResolver(func(context.Context, GoalResolveInput) (GoalResolution, error) { calls++; return GoalResolution{}, nil })
	initial := State{Request: &GoalRequest{Text: strings.Repeat("x", c.MaxStateBytes+1)}}
	stopped := runFixture(t, initial, c, d)
	require.Equal(t, "blocked", stopped.Outcome.Status)
	require.Nil(t, stopped.Request)
	require.Zero(t, calls)
}
func TestPrepareInitialStatePreservesCommittedAuthority(t *testing.T) {
	s, c, _ := allocationFixture(t)
	c.MaxStateBytes = 2048
	s, e := Allocate(s, Config{Version: c.Version, Members: c.Members}, "root")
	require.NoError(t, e)
	require.NotNil(t, s.Wave)
	s.Request = &GoalRequest{Text: strings.Repeat("x", c.MaxStateBytes+1)}
	before := s.Clone()
	out, e := PrepareInitialState(s, c)
	require.Error(t, e)
	require.Equal(t, before, out)
	require.Equal(t, before, s)
}
func TestPrepareInitialStateBoundsFirstCheckpoint(t *testing.T) {
	_, c, d := graphFixture(t)
	c.MaxStateBytes = 2048
	initial := State{Request: &GoalRequest{Text: strings.Repeat("secret", c.MaxStateBytes)}}
	out, e := PrepareInitialState(initial, c)
	require.NoError(t, e)
	require.Nil(t, out.Request)
	require.Equal(t, "blocked", out.Outcome.Status)
	raw, e := json.Marshal(out)
	require.NoError(t, e)
	require.LessOrEqual(t, len(raw), c.MaxStateBytes)
	done := runFixture(t, out, c, d)
	require.Equal(t, out, done)
	c.MaxStateBytes = 1
	_, e = NewGraph(c, d)
	require.Error(t, e)
	_, e = PrepareInitialState(initial, c)
	require.Error(t, e)
}

func TestFreshRequestAtCapacityStopsAfterNormalization(t *testing.T) {
	_, cfg, deps := graphFixture(t)
	initial := State{Request: &GoalRequest{Text: strings.Repeat("x", 2048)}}
	raw, err := json.Marshal(initial)
	require.NoError(t, err)
	cfg.MaxStateBytes = len(raw)
	deps.GoalResolverVersion = "v1"
	calls := 0
	deps.GoalResolver = graphResolver(func(context.Context, GoalResolveInput) (GoalResolution, error) { calls++; return GoalResolution{}, nil })
	prepared, err := PrepareInitialState(initial, cfg)
	require.NoError(t, err)
	require.Equal(t, initial, prepared)
	done := runFixture(t, prepared, cfg, deps)
	require.Equal(t, initialCapacityStop(), done)
	require.Zero(t, calls)
}
