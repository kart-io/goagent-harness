package planning

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	json "github.com/kart-io/goagent/codec/json"
)

// AdoptPlan commits a validated candidate and retains only accepted results whose
// execution contracts and transitive inputs still match the committed plan.
func AdoptPlan(state State, draft PlanDraft, cfg Config) (State, error) {
	c, err := NormalizeConfig(cfg)
	if err != nil {
		return state, err
	}
	if err = ValidateGoalPolicy(state.Goal, c); err != nil {
		return state, err
	}
	d, err := GoalDigest(state.Goal)
	if err != nil || d != state.GoalDigest {
		return state, invalid("已提交目标摘要不符")
	}
	old := map[string]TaskSpec{}
	var accepted map[string]Attempt
	s := state.Clone()
	if s.Plan != nil {
		if s.Plan.GoalDigest != d || s.Plan.Revision <= 0 || s.Plan.Revision == math.MaxInt {
			return state, invalid("原计划身份无效")
		}
		if err = ValidateDraft(s.Goal, PlanDraft{Tasks: s.Plan.Tasks, Groups: s.Plan.Groups}, c); err != nil {
			return state, err
		}
		for _, t := range s.Plan.Tasks {
			old[t.ID] = t
		}
		accepted, err = currentAccepted(s, c)
		if err != nil {
			return state, err
		}

	} else if len(s.Attempts) > 0 || len(s.Accepted) > 0 || s.WriteFreeze != nil {
		return state, invalid("缺少历史计划")
	}
	if s.Wave != nil || len(s.WaveResults) > 0 {
		return state, invalid("已冻结批次须先完成审查")
	}
	if s.Outcome != nil {
		return state, invalid("运行已经终止")
	}
	cursor := s.NextTaskID
	if cursor == 0 && s.Plan == nil {
		cursor = 1
	}
	if cursor < 1 || cursor-1 > c.MaxTasks {
		return state, invalid("历史任务编号超出额度")
	}
	validID := func(id string) bool {
		n, e := strconv.Atoi(strings.TrimPrefix(id, "t-"))
		return e == nil && n > 0 && n < cursor && fmt.Sprintf("t-%06d", n) == id
	}
	for id, t := range old {
		if !validID(id) || t.Generation < 1 {
			return state, invalid("历史任务%q身份无效", id)
		}
	}
	members := memberDirectory(c)
	if s.Plan != nil {
		histories, err := history(s, "", c)
		if err != nil {
			return state, err
		}
		for id, h := range histories {
			if !validID(id) || h.pending {
				return state, invalid("任务%q历史身份无效或存在未决尝试", id)
			}
		}
	}
	if s.WriteFreeze != nil {
		s.WriteFreeze, err = buildWriteFreeze(s, c, accepted)
		if err != nil {
			return adoptionStop(state, c, err.Error())
		}
		for _, a := range s.Attempts {
			task, ok := old[a.TaskID]
			if !ok || a.EffectClass != "write" {
				continue
			}
			inputs, ready := resolveInputs(task, accepted)
			if !ready || task.Generation != a.Generation {
				return adoptionStop(state, c, "已分配写任务的输入或代次无效")
			}
			fp, err := fingerprint(d, s.Goal, task, members[task.MemberID], inputs)
			if err != nil || fp != a.Fingerprint {
				return adoptionStop(state, c, "已分配写任务的执行指纹已改变")
			}
		}
	}
	seen := idSet{}
	for _, t := range draft.Tasks {
		if !seen.add(t.ID) {
			return state, invalid("候选任务身份%q为空或重复", t.ID)
		}
		if _, ok := old[t.ID]; ok {
			continue
		}
		label, local := strings.CutPrefix(t.ID, "new:")
		if strings.HasPrefix(t.ID, "t-") || (s.Plan != nil && !local) || (local && !nonempty(label)) {
			return state, invalid("非法候选任务身份%q", t.ID)
		}
	}
	if len(draft.Tasks) > c.MaxTasks || len(draft.Groups) > c.MaxTasks {
		return adoptionStop(state, c, "候选任务或分组数量超出上限")
	}
	if err = ValidateDraft(s.Goal, draft, c); err != nil {
		return state, err
	}
	p := draft.clone()
	ids := map[string]string{}
	for _, t := range p.Tasks {
		if _, ok := old[t.ID]; ok {
			ids[t.ID] = t.ID
			continue
		}
		if cursor > c.MaxTasks || cursor == math.MaxInt {
			return adoptionStop(state, c, "历史任务总数已达上限")
		}
		ids[t.ID] = fmt.Sprintf("t-%06d", cursor)
		cursor++
	}
	for i := range p.Tasks {
		t := &p.Tasks[i]
		t.ID = ids[t.ID]
		t.Generation = 1
		for j, v := range t.DependsOn {
			t.DependsOn[j] = ids[v]
		}
		for j, v := range t.Inputs {
			t.Inputs[j].DependencyID = ids[v.DependencyID]
		}
	}
	parent := 0
	if s.Plan != nil {
		parent = s.Plan.Revision
	}
	s.Plan = &Plan{Revision: parent + 1, ParentRevision: parent, GoalDigest: d, Tasks: p.Tasks, Groups: p.Groups}
	keep, err := reconcileTasks(s, old, accepted, members)
	if err != nil {
		return state, err
	}
	if s.WriteFreeze != nil {
		s.WriteFreeze, err = buildWriteFreeze(s, c, keep)
		if err != nil {
			return adoptionStop(state, c, err.Error())
		}
	}
	keys := map[string]bool{}
	for _, a := range keep {
		keys[a.ExecutionKey] = true
	}
	for i, a := range s.Attempts {
		if a.Status == "accepted" && !keys[a.ExecutionKey] {
			s.Attempts[i].Status = "superseded"
		}
	}
	s.Accepted = slices.DeleteFunc(s.Accepted, func(a Artifact) bool { return !keys[a.ProducerAttemptKey] })
	s.NextTaskID = cursor
	s.Candidate = nil
	s.ValidationError = ""
	s.PendingAction = "select"
	encoded, err := json.Marshal(s)
	if err != nil {
		return state, err
	}
	if len(encoded) > c.MaxStateBytes {
		return adoptionStop(state, c, "状态大小上限")
	}
	return s, nil
}

func adoptionStop(s State, c Config, reason string) (State, error) {
	stop := blocked(s.Clone(), reason)
	encoded, err := json.Marshal(stop)
	if err != nil {
		return s, err
	}
	if len(encoded) > c.MaxStateBytes {
		return s, invalid("规划检查点超出状态大小上限 %d 字节", c.MaxStateBytes)
	}
	return stop, nil
}

// 候选已通过 DAG 校验；只有全部直接上游都检查完，才判定当前任务的代次和复用资格。
func reconcileTasks(s State, old map[string]TaskSpec, accepted map[string]Attempt, members map[string]Member) (map[string]Attempt, error) {
	keep := map[string]Attempt{}
	done := map[string]bool{}
	changed := map[string]bool{}
	for len(done) < len(s.Plan.Tasks) {
		for i := range s.Plan.Tasks {
			t := &s.Plan.Tasks[i]
			if done[t.ID] || slices.ContainsFunc(t.DependsOn, func(id string) bool { return !done[id] }) {
				continue
			}
			done[t.ID] = true
			prev, ok := old[t.ID]
			if !ok {
				changed[t.ID] = true
				continue
			}
			t.Generation = prev.Generation
			before, err := semanticContract(s.Goal, prev, members[prev.MemberID])
			if err != nil {
				return nil, err
			}
			after, err := semanticContract(s.Goal, *t, members[t.MemberID])
			if err != nil {
				return nil, err
			}
			different := !reflect.DeepEqual(before, after) || slices.ContainsFunc(t.DependsOn, func(id string) bool { return changed[id] })
			if a, ok := accepted[t.ID]; ok && !different {
				inputs, ready := resolveInputs(*t, keep)
				if ready {
					fp, err := fingerprint(s.GoalDigest, s.Goal, *t, members[t.MemberID], inputs)
					if err != nil {
						return nil, err
					}
					different = fp != a.Fingerprint
					if !different {
						keep[t.ID] = a
					}
				} else {
					different = true
				}
			}
			if different {
				if t.Generation == math.MaxInt {
					return nil, invalid("任务代次溢出")
				}
				t.Generation++
				changed[t.ID] = true
			}
		}
	}
	return keep, nil
}
