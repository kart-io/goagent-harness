package planning

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/workflow"
)

func (f planningGraph) parallel() workflow.ParallelConfig[State] {
	p := workflow.ParallelConfig[State]{Clone: State.Clone, Merge: func(_ context.Context, s State, branches []State) (State, error) {
		if s.Wave == nil || len(s.Wave.Assignments) > len(branches) {
			return s, fmt.Errorf("冻结批次与槽数不符")
		}
		n := s.Clone()
		for slot, b := range branches {
			if slot >= len(s.Wave.Assignments) {
				if len(b.WaveResults) != 0 {
					return s, fmt.Errorf("空槽出现结果")
				}
				continue
			}
			a := s.Wave.Assignments[slot]
			if len(b.WaveResults) != 1 || a.Slot != slot || a.Task.ID != b.WaveResults[0].TaskID || a.AttemptKey != b.WaveResults[0].AttemptKey {
				return s, fmt.Errorf("槽结果与冻结身份不符")
			}
			n.WaveResults = append(n.WaveResults, b.WaveResults[0])
		}
		n.PendingAction = "review"
		return boundedState(s, n, f.cfg)
	}}
	for slot := range f.cfg.Parallelism {
		p.Branches = append(p.Branches, workflow.Branch[State]{Name: fmt.Sprintf("slot-%d", slot), Run: func(ctx context.Context, s State) (State, error) {
			if s.Wave == nil {
				return State{}, fmt.Errorf("执行缺少冻结批次")
			}
			if slot >= len(s.Wave.Assignments) {
				return State{}, nil
			}
			a := s.Wave.Assignments[slot]
			if e := f.checkAssignment(s, a, slot); e != nil {
				return State{}, e
			}
			r, e := f.deps.Executor.Execute(durable.WithExecutionKey(ctx, a.AttemptKey), a.clone())
			if e == nil {
				r, e = ValidateTaskResult(a, r, f.cfg)
			}
			if e != nil {
				var bad *CandidateError
				if !errors.As(e, &bad) {
					return State{}, e
				}
				r = TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "rejected", FailureKind: "invalid_result", Reason: boundedText(e.Error())}
			}
			result := State{WaveResults: []TaskResult{r}}
			if err := withinStateLimit(result, f.cfg); err != nil {
				result.WaveResults[0] = oversizeResult(a)
				if err = withinStateLimit(result, f.cfg); err != nil {
					return s, err
				}
			}
			return result, nil
		}})
	}
	return p
}

func (f planningGraph) checkAssignment(s State, a Assignment, slot int) error {
	if s.Plan == nil || a.Slot != slot || a.PlanRevision != s.Plan.Revision {
		return fmt.Errorf("冻结任务槽或计划版本无效")
	}
	d, e := GoalDigest(s.Goal)
	if e != nil || d != s.GoalDigest || d != a.GoalDigest {
		return fmt.Errorf("冻结目标摘要无效")
	}
	i := slices.IndexFunc(s.Plan.Tasks, func(t TaskSpec) bool { return t.ID == a.Task.ID })
	if i < 0 {
		return fmt.Errorf("冻结任务不在当前计划中")
	}
	task := s.Plan.Tasks[i]
	accepted, e := currentAccepted(s, f.cfg)
	if e != nil {
		return e
	}
	inputs, ready := resolveInputs(task, accepted)
	if !ready {
		return fmt.Errorf("冻结任务的上游结果尚未验收")
	}
	m := memberDirectory(f.cfg)[task.MemberID]
	expected, e := freezeAssignment(s, task, m, inputs)
	if e != nil {
		return e
	}
	expected.Slot = slot
	expected.AttemptKey = a.AttemptKey
	if !reflect.DeepEqual(expected, a) {
		return fmt.Errorf("冻结任务与执行契约不符")
	}
	if !slices.ContainsFunc(s.Attempts, func(at Attempt) bool {
		return at.Status == "running" && at.ExecutionKey == a.AttemptKey && at.Fingerprint == a.Fingerprint && at.TaskID == a.Task.ID && at.EffectClass == m.EffectClass
	}) {
		return fmt.Errorf("冻结尝试与执行契约不符")
	}
	return nil
}

func (f planningGraph) reviewWave(ctx context.Context, s State) (State, error) {
	if s.Wave == nil || len(s.WaveResults) != len(s.Wave.Assignments) {
		return s, fmt.Errorf("批次结果缺失")
	}
	action := "select"
	reason := ""
	for i, a := range s.Wave.Assignments {
		idx := slices.IndexFunc(s.Attempts, func(at Attempt) bool { return at.ExecutionKey == a.AttemptKey })
		if idx < 0 {
			return s, fmt.Errorf("批次尝试不存在")
		}
		at := &s.Attempts[idx]
		r := s.WaveResults[i].clone()
		at.Result = &r
		d, e := TaskReviewDigest(*at)
		if e != nil {
			return s, e
		}
		in := ReviewInput{Goal: s.Goal.clone(), Plan: s.Plan.clone(), Assignment: clonePointer(&a, Assignment.clone), SubjectDigest: d, Tasks: []TaskSpec{a.Task.clone()}, Results: []TaskResult{r.clone()}}
		rev, e := f.recordReview(durable.WithExecutionKey(ctx, durable.ChildKey(a.AttemptKey, "review")), in, s.Waves)
		verdict := "replan"
		why := ""
		if e != nil {
			var bad *CandidateError
			if !errors.As(e, &bad) {
				return s, e
			}
			why = boundedText(e.Error())
		} else {
			at.Review = &rev
			s.Feedback = append(s.Feedback, rev)
			verdict = rev.Verdict
			why = rev.Reason
		}
		if verdict == "accept" {
			at.Status = "accepted"
			s.Accepted = append(s.Accepted, r.Artifacts...)
			continue
		}
		at.Status = "rejected"
		if verdict == "block" || at.EffectClass == "write" {
			action = "stop"
			reason = why
		} else if verdict == "replan" && action != "stop" {
			action = "revise"
			reason = why
		}
	}
	s.Wave = nil
	s.WaveResults = nil
	if action == "stop" {
		return blocked(s, reason), nil
	}
	s.PendingAction = action
	s.ValidationError = reason
	return s, nil
}
