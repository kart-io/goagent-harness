package planning

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/observe"
	"github.com/kart-io/goagent/workflow"
)

func missingDep(v any) bool {
	if v == nil {
		return true
	}
	x := reflect.ValueOf(v)
	switch x.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Interface, reflect.Chan, reflect.Slice:
		return x.IsNil()
	}
	return false
}

func graphConfig(c Config, d Dependencies) (Config, error) {
	c, e := NormalizeConfig(c)
	if e != nil {
		return c, e
	}
	if e = checkInitialCapacity(c); e != nil {
		return c, e
	}
	if missingDep(d.Executor) || missingDep(d.Planner) || missingDep(d.Reviewer) || !nonempty(d.ReviewerVersion) || !nonempty(d.PlannerVersion) {
		return c, configError("缺少规划、执行、审查依赖或版本")
	}
	if d.GoalResolver != nil && (missingDep(d.GoalResolver) || !nonempty(d.GoalResolverVersion)) || d.GoalResolver == nil && d.GoalResolverVersion != "" {
		return c, configError("目标解析器与版本不匹配")
	}
	return c, nil
}

// DurableConfig pins the state schema, host policy, member directory and component versions.
func DurableConfig(c Config, d Dependencies, store durable.Store) (workflow.DurableConfig, error) {
	c, e := graphConfig(c, d)
	if e != nil {
		return workflow.DurableConfig{}, e
	}
	members, e := digestJSON(c.Members)
	if e != nil {
		return workflow.DurableConfig{}, e
	}
	policy, e := digestJSON(c)
	if e != nil {
		return workflow.DurableConfig{}, e
	}
	components := map[string]string{"planner": d.PlannerVersion, "reviewer": d.ReviewerVersion, "members": members, "config": policy}
	if d.GoalResolver != nil {
		components["goal_resolver"] = d.GoalResolverVersion
	}
	return workflow.DurableConfig{Store: store, Version: c.Version, StateVersion: "planning-state-v1", Components: components}, nil
}

func boundedState(prev, next State, c Config) (State, error) {
	b, e := json.Marshal(next)
	if e != nil {
		return prev, e
	}
	if len(b) <= c.MaxStateBytes {
		return next, nil
	}
	stop := blocked(prev.Clone(), "状态大小上限")
	b, e = json.Marshal(stop)
	if e != nil {
		return prev, e
	}
	if len(b) > c.MaxStateBytes {
		return prev, fmt.Errorf("规划检查点超出状态大小上限 %d 字节", c.MaxStateBytes)
	}
	return stop, nil
}

// NewGraph builds a fixed, bounded graph over the supplied versioned dependencies.
func NewGraph(c Config, d Dependencies) (*workflow.Graph[State], error) {
	c, e := graphConfig(c, d)
	if e != nil {
		return nil, e
	}
	f := planningGraph{c, d}
	g := workflow.NewGraph[State](workflow.Config{MaxSteps: c.maxSteps(), MaxConcurrency: c.Parallelism})
	identity := func(_ context.Context, s State) (State, error) { return s, nil }
	nodes := map[string]workflow.Node[State]{
		"normalize": f.normalize, "resolve_goal": f.resolve, "clarify_goal": f.clarify, "confirm_goal": f.confirm,
		"plan": f.plan, "validate": f.validate, "select": f.selectWave,
		"review": f.reviewWave, "revise": f.revise, "final_review": f.finalReview,
		"finish": identity, "stop": identity,
	}
	for name, node := range nodes {
		if e = g.AddNode(name, func(ctx context.Context, s State) (State, error) {
			v, e := node(ctx, s.Clone())
			if e != nil {
				return s, e
			}
			return boundedState(s, v, c)
		}); e != nil {
			return nil, e
		}
	}
	if e = g.AddParallel("execute", f.parallel()); e != nil {
		return nil, e
	}
	for from, to := range map[string]string{constants.WorkflowStart: "normalize", "finish": constants.WorkflowEnd, "stop": constants.WorkflowEnd} {
		if e = g.AddEdge(from, to); e != nil {
			return nil, e
		}
	}
	routes := map[string][]string{
		"normalize":    {"plan", "resolve_goal"},
		"resolve_goal": {"resolve_goal", "clarify_goal", "confirm_goal"},
		"clarify_goal": {"resolve_goal"}, "confirm_goal": {"plan"},
		"plan": {"validate", "revise"}, "revise": {"validate", "revise"}, "validate": {"select", "revise"},
		"select": {"execute", "final_review"}, "execute": {"review"}, "review": {"select", "revise"},
		"final_review": {"finish", "revise"},
	}
	for from, targets := range routes {
		if e = g.AddBranch(from, func(_ context.Context, s State) (string, error) { return s.PendingAction, nil }, append(targets, "stop")...); e != nil {
			return nil, e
		}
	}
	return g, nil
}

func rejectCandidate(s State, e error) (State, error) {
	var b *CandidateError
	if !errors.As(e, &b) {
		return s, e
	}
	s.PendingAction = "revise"
	s.ValidationError = boundedText(b.Reason)
	return s, nil
}

func (f planningGraph) plan(ctx context.Context, s State) (State, error) {
	copy := s.Clone()
	cfg, e := NormalizeConfig(f.cfg)
	if e != nil {
		return s, e
	}
	input := PlanningInput{Goal: copy.Goal, Current: copy.Plan, RejectedDraft: copy.Candidate, RejectionReason: s.ValidationError, Accepted: copy.Accepted, Feedback: copy.Feedback, Members: cfg.Members}
	if e = withinStateLimit(input, f.cfg); e != nil {
		return blocked(s, e.Error()), nil
	}
	draft, e := f.deps.Planner.Plan(ctx, input)
	// 只有本次调用实际返回、并且能够进入检查点的候选可反馈给下一轮。
	s.Candidate = nil
	if e != nil {
		return rejectCandidate(s, e)
	}
	if e = withinStateLimit(draft, f.cfg); e != nil {
		return rejectCandidate(s, e)
	}
	draft = draft.clone()
	s.Candidate = &draft
	if e = withinStateLimit(s, f.cfg); e != nil {
		s.Candidate = nil
		return rejectCandidate(s, e)
	}
	s.PendingAction = "validate"
	return s, nil
}

func (f planningGraph) validate(_ context.Context, s State) (State, error) {
	if s.Candidate == nil {
		return s, fmt.Errorf("缺少已保存的候选计划")
	}
	n, e := AdoptPlan(s, *s.Candidate, f.cfg)
	if e != nil {
		return rejectCandidate(s, e)
	}
	return n, nil
}

func withinStateLimit(v any, c Config) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(raw) > c.MaxStateBytes {
		return invalid("规划输入或候选超出状态大小上限")
	}
	return nil
}

func (f planningGraph) selectWave(ctx context.Context, s State) (State, error) {
	return Allocate(s, f.cfg, observe.RunIDFromContext(ctx))
}

func (f planningGraph) revise(ctx context.Context, s State) (State, error) {
	if s.Replans >= f.cfg.MaxReplans {
		return blocked(s, "重规划次数已达上限："+s.ValidationError), nil
	}
	s.Replans++
	return f.plan(ctx, s)
}
