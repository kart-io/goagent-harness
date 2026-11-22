package planning

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"

	json "github.com/kart-io/goagent/codec/json"
)

func memberDirectory(c Config) map[string]Member {
	m := map[string]Member{}
	for _, v := range c.Members {
		m[v.ID] = v
	}
	return m
}

// TaskReviewDigest binds the attempt identity and the named, verified result artifacts.
func TaskReviewDigest(a Attempt) (string, error) {
	if a.Result == nil || !nonempty(a.Fingerprint) || !nonempty(a.ExecutionKey) || a.Result.TaskID != a.TaskID || a.Result.AttemptKey != a.ExecutionKey {
		return "", invalid("审查尝试与结果身份不符")
	}
	artifacts := []sourceIdentity{}
	for _, v := range ordered(a.Result.Artifacts, func(x Artifact) string { return x.Name }) {
		d, err := artifactDigest(v)
		if err != nil || v.Digest != d {
			return "", invalid("审查产物%q摘要无效", v.Name)
		}
		artifacts = append(artifacts, sourceIdentity{v.Name, d})
	}
	return digestJSON(struct {
		Key, Fingerprint, Outcome, FailureKind, Reason string
		Artifacts                                      []sourceIdentity
	}{a.ExecutionKey, a.Fingerprint, a.Result.Outcome, a.Result.FailureKind, a.Result.Reason, artifacts})
}

func resolveInputs(t TaskSpec, accepted map[string]Attempt) ([]ResolvedInput, bool) {
	for _, id := range t.DependsOn {
		if _, ok := accepted[id]; !ok {
			return nil, false
		}
	}
	out := []ResolvedInput{}
	for _, r := range t.Inputs {
		artifacts := accepted[r.DependencyID].Result.Artifacts
		i := slices.IndexFunc(artifacts, func(a Artifact) bool { return a.Name == r.OutputName })
		if i < 0 {
			return nil, false
		}
		out = append(out, ResolvedInput{r, artifacts[i]})
	}
	return out, true
}
func freezeAssignment(s State, t TaskSpec, m Member, inputs []ResolvedInput) (Assignment, error) {
	fp, err := fingerprint(s.GoalDigest, s.Goal, t, m, inputs)
	if err != nil {
		return Assignment{}, err
	}
	sources, err := selectedSources(s.Goal, t)
	if err != nil {
		return Assignment{}, err
	}
	g := s.Goal
	a := Assignment{Task: t, PlanRevision: s.Plan.Revision, GoalDigest: s.GoalDigest, GoalContext: GoalContext{g.Objective, g.Constraints, g.Scope, g.Assumptions, g.Criteria, g.Deliverables}, GoalInputs: sources, Parameters: g.Parameters, Output: g.Output, MemberVersion: m.Version, ManifestDigest: m.ManifestDigest, Claims: m.Claims, Inputs: inputs, Fingerprint: fp}
	return a.clone(), nil
}

// currentAccepted verifies accepted attempts in dependency order before using their outputs.
func currentAccepted(s State, c Config) (map[string]Attempt, error) {
	members := memberDirectory(c)
	pending := map[string]Attempt{}
	accepted := map[string]Attempt{}
	tasks := map[string]TaskSpec{}
	for _, t := range s.Plan.Tasks {
		tasks[t.ID] = t
	}
	for _, a := range s.Attempts {
		t, ok := tasks[a.TaskID]
		if !ok || a.Status != "accepted" || a.Generation != t.Generation {
			continue
		}
		if _, dup := pending[t.ID]; dup {
			return nil, invalid("任务%q有多个有效尝试", t.ID)
		}
		pending[t.ID] = a
	}
	index := map[string]Artifact{}
	for _, a := range s.Accepted {
		if _, dup := index[a.ID]; dup {
			return nil, invalid("验收产物%q重复", a.ID)
		}
		index[a.ID] = a
	}
	for len(pending) > 0 {
		before := len(pending)
		for _, t := range s.Plan.Tasks {
			a, ok := pending[t.ID]
			if !ok {
				continue
			}
			inputs, ready := resolveInputs(t, accepted)
			if !ready {
				continue
			}
			frozen, err := freezeAssignment(s, t, members[t.MemberID], inputs)
			if err != nil {
				return nil, err
			}
			if a.Review == nil || a.Review.Verdict != "accept" || a.Result == nil || a.Result.Outcome != "produced" || a.Fingerprint != frozen.Fingerprint || len(a.Result.Artifacts) != len(t.Outputs) {
				return nil, invalid("任务%q验收记录不符", t.ID)
			}
			frozen.AttemptKey = a.ExecutionKey
			validated, err := ValidateTaskResult(frozen, *a.Result, c)
			if err != nil {
				return nil, err
			}
			if validated.Outcome != "produced" || !reflect.DeepEqual(validated, *a.Result) {
				return nil, invalid("任务%q已验收结果与冻结契约不符", t.ID)
			}
			subject, err := TaskReviewDigest(a)
			if err != nil {
				return nil, err
			}
			_, err = ValidateReview(ReviewInput{SubjectDigest: subject, Assignment: &frozen, Results: []TaskResult{*a.Result}}, *a.Review)
			if err != nil {
				return nil, err
			}
			for _, v := range a.Result.Artifacts {
				copy, ok := index[v.ID]
				if !ok || copy.Name != v.Name || copy.ProducerAttemptKey != a.ExecutionKey || copy.DeliverableID != v.DeliverableID || copy.Digest != v.Digest {
					return nil, invalid("验收产物%q未绑定当前尝试", v.ID)
				}
				d, e := artifactDigest(copy)
				if e != nil || d != v.Digest {
					return nil, invalid("验收产物%q正文不符", v.ID)
				}
				delete(index, v.ID)
			}
			accepted[t.ID] = a
			delete(pending, t.ID)
		}
		if len(pending) == before {
			return nil, invalid("验收任务缺少已验收上游")
		}
	}
	if len(index) != 0 {
		return nil, invalid("存在不属于当前验收尝试的产物")
	}
	return accepted, nil
}

type attemptHistory struct {
	last    int
	pending bool
}

func history(s State, root string, c Config) (map[string]attemptHistory, error) {
	h := map[string]attemptHistory{}
	for _, a := range s.Attempts {
		v := h[a.TaskID]
		key, err := AttemptKey(root, a.TaskID, a.Generation, a.Number)
		if err != nil || a.ExecutionKey != key || a.Number != v.last+1 || a.Number > c.MaxTaskAttempts {
			return nil, invalid("任务%q尝试身份或序号无效", a.TaskID)
		}
		v.last = a.Number
		switch a.Status {
		case "rejected", "superseded", "accepted":
		default:
			v.pending = true
		}
		h[a.TaskID] = v
	}
	return h, nil
}
func conflicts(claims []ResourceClaim, used map[string]string) bool {
	for _, claim := range claims {
		mode := used[claim.Key]
		if mode != "" && (mode == "write" || claim.Mode == "write") {
			return true
		}
	}
	return false
}
func blocked(s State, reason string) State {
	s.Outcome = &Outcome{Status: "blocked", Reason: reason}
	s.PendingAction = "stop"
	return s
}

// Allocate selects the next ready tasks and atomically freezes their execution contracts.
func Allocate(state State, c Config, root string) (State, error) {
	c, err := NormalizeConfig(c)
	if err != nil {
		return state, err
	}
	if state.Plan == nil || !nonempty(root) {
		return state, invalid("缺少已提交计划或根运行身份")
	}
	if err := ValidateGoalPolicy(state.Goal, c); err != nil {
		return state, err
	}
	d, err := GoalDigest(state.Goal)
	if err != nil || d != state.GoalDigest || d != state.Plan.GoalDigest {
		return state, invalid("目标摘要与计划不符")
	}
	if err := ValidateDraft(state.Goal, PlanDraft{Tasks: state.Plan.Tasks, Groups: state.Plan.Groups}, c); err != nil {
		return state, err
	}
	histories, err := history(state, root, c)
	if err != nil {
		return state, err
	}
	s := state.Clone()
	if s.Wave != nil {
		return s, nil
	}
	for id, h := range histories {
		if h.pending {
			return state, fmt.Errorf("任务 %q 有未决尝试但缺少已冻结批次，需要恢复执行检查点", id)
		}
	}
	if s.Outcome != nil {
		return s, nil
	}
	accepted, err := currentAccepted(s, c)
	if err != nil {
		return state, err
	}
	if s.WriteFreeze != nil {
		refreshed, err := buildWriteFreeze(s, c, accepted)
		if err != nil {
			return blocked(state.Clone(), err.Error()), nil
		}
		s.WriteFreeze = refreshed
	}
	if len(accepted) == len(s.Plan.Tasks) {
		s.PendingAction = "final_review"
		return s, nil
	}
	if s.Waves >= c.MaxWaves {
		return blocked(s, "批次数量已达上限"), nil
	}
	tasks := slices.Clone(s.Plan.Tasks)
	slices.SortFunc(tasks, func(a, b TaskSpec) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), strings.Compare(a.ID, b.ID))
	})
	members := memberDirectory(c)
	used := map[string]string{}
	wave := Wave{Number: s.Waves + 1, PlanRevision: s.Plan.Revision}
	needsFreeze := false
	for _, t := range tasks {
		if len(wave.Assignments) >= c.Parallelism || len(s.Attempts) >= c.MaxTaskAttempts*c.MaxTasks {
			break
		}
		h := histories[t.ID]
		m := members[t.MemberID]
		if _, ok := accepted[t.ID]; ok || h.pending || h.last >= c.MaxTaskAttempts || (m.EffectClass == "write" && h.last > 0) || conflicts(m.Claims, used) {
			continue
		}
		inputs, ready := resolveInputs(t, accepted)
		if !ready {
			continue
		}
		a, err := freezeAssignment(s, t, m, inputs)
		if err != nil {
			return state, err
		}
		a.AttemptKey, err = AttemptKey(root, t.ID, t.Generation, h.last+1)
		if err != nil {
			return state, err
		}
		a.Slot = len(wave.Assignments)
		wave.Assignments = append(wave.Assignments, a)
		s.Attempts = append(s.Attempts, Attempt{TaskID: t.ID, Generation: t.Generation, Number: h.last + 1, ExecutionKey: a.AttemptKey, Fingerprint: a.Fingerprint, Status: "running"})
		for _, claim := range m.Claims {
			used[claim.Key] = claim.Mode
		}
		needsFreeze = needsFreeze || m.EffectClass == "write"
	}
	if len(wave.Assignments) == 0 {
		return blocked(s, "没有可分配任务：依赖未验收、尝试未决或尝试额度耗尽"), nil
	}
	if needsFreeze && s.WriteFreeze == nil {
		freeze, err := buildWriteFreeze(s, c, accepted)
		if err != nil {
			return blocked(state.Clone(), err.Error()), nil
		}
		s.WriteFreeze = freeze
	}
	s.Wave = &wave
	s.Waves = wave.Number
	s.WaveResults = nil
	s.PendingAction = "execute"
	encoded, err := json.Marshal(s)
	if err != nil {
		return state, err
	}
	if len(encoded) > c.MaxStateBytes {
		return blocked(state.Clone(), "冻结批次超出状态大小上限"), nil
	}
	return s, nil
}
