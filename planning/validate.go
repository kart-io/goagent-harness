package planning

import (
	"slices"
	"strings"
)

type idSet map[string]bool

func nonempty(s string) bool { return strings.TrimSpace(s) != "" }
func (s idSet) add(id string) bool {
	if !nonempty(id) || s[id] {
		return false
	}
	s[id] = true
	return true
}
func refs(list []string, allowed idSet) bool {
	seen := idSet{}
	for _, id := range list {
		if !seen.add(id) || (allowed != nil && !allowed[id]) {
			return false
		}
	}
	return true
}
func criteriaSet(cs []Criterion) (idSet, error) {
	ids := idSet{}
	if len(cs) == 0 {
		return nil, invalid("缺少验收项")
	}
	for _, c := range cs {
		if !ids.add(c.ID) || !nonempty(c.Description) {
			return nil, invalid("验收项%q无效或重复", c.ID)
		}
	}
	return ids, nil
}

// ValidateGoal checks the complete confirmed goal and its source digests.
func ValidateGoal(g GoalSpec) error {
	if !nonempty(g.ID) || g.Revision < 1 || !nonempty(g.Objective) {
		return invalid("目标身份、版本或内容缺失")
	}
	if len(g.Scope.Included) == 0 || !refs(g.Scope.Included, nil) || !refs(g.Scope.Excluded, nil) || !nonempty(g.Output.Language) || !nonempty(g.Output.Format) {
		return invalid("目标范围或输出约定无效")
	}
	if _, err := criteriaSet(g.Criteria); err != nil {
		return err
	}
	ids := idSet{}
	for _, x := range g.Constraints {
		if !ids.add(x.ID) || !nonempty(x.Rule) {
			return invalid("目标约束%q无效", x.ID)
		}
	}
	if len(g.Deliverables) == 0 {
		return invalid("缺少交付物")
	}
	ids = idSet{}
	for _, x := range g.Deliverables {
		if !ids.add(x.ID) || !nonempty(x.Description) || !nonempty(x.MediaType) {
			return invalid("交付物%q无效", x.ID)
		}
	}
	ids = idSet{}
	for _, x := range g.Inputs {
		if !ids.add(x.ID) || !nonempty(x.Origin) || !nonempty(x.MediaType) || !nonempty(x.Title) {
			return invalid("来源%q无效", x.ID)
		}
		d, e := SourceDigest(x.Content)
		if e != nil || d != x.Digest {
			return invalid("来源%q正文或摘要无效", x.ID)
		}
	}
	ids = idSet{}
	for _, x := range g.Parameters {
		if !ids.add(x.Key) || !nonempty(x.Value) || !slices.Contains([]string{"user", "host", "inferred"}, x.Origin) {
			return invalid("参数%q无效", x.Key)
		}
	}
	ids = idSet{}
	for _, x := range g.Assumptions {
		if !ids.add(x.ID) || !nonempty(x.Statement) || !x.Confirmed {
			return invalid("假设%q无效或未经确认", x.ID)
		}
	}
	return nil
}
func defaultLimit(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}

// ValidateGoalPolicy checks the host's size limits and required parameters.
func ValidateGoalPolicy(g GoalSpec, c Config) error {
	remaining := defaultLimit(c.MaxArtifactTotalBytes, 1048576)
	for _, x := range g.Inputs {
		if len(x.Content) > defaultLimit(c.MaxArtifactBytes, 65536) || len(x.Content) > remaining {
			return invalid("来源%q超出大小限制", x.ID)
		}
		remaining -= len(x.Content)
	}
	if e := ValidateGoal(g); e != nil {
		return e
	}
	ids := idSet{}
	for _, p := range g.Parameters {
		ids[p.Key] = true
	}
	if !refs(c.RequiredGoalParameters, ids) {
		return invalid("必需目标参数缺失或重复")
	}
	return nil
}

// ValidateDraft checks task contracts, dependencies, groups, and goal coverage.
func ValidateDraft(g GoalSpec, p PlanDraft, c Config) error {
	limit := defaultLimit(c.MaxTasks, 64)
	if len(p.Tasks) == 0 || len(p.Tasks) > limit || len(p.Groups) > limit {
		return invalid("计划任务或分组数量无效")
	}
	groupDeps := map[string][]string{}
	for _, v := range p.Groups {
		if _, ok := groupDeps[v.ID]; ok || !nonempty(v.ID) || !nonempty(v.Title) {
			return invalid("分组%q无效", v.ID)
		}
		groupDeps[v.ID] = nil
		if v.ParentID != "" {
			groupDeps[v.ID] = []string{v.ParentID}
		}
	}
	if err := validateDAG(groupDeps); err != nil {
		return err
	}
	criteria, err := criteriaSet(g.Criteria)
	if err != nil {
		return err
	}
	deliverables := map[string]string{}
	for _, d := range g.Deliverables {
		deliverables[d.ID] = d.MediaType
	}
	sources := idSet{}
	for _, s := range g.Inputs {
		sources[s.ID] = true
	}
	capabilities := map[string]idSet{}
	for _, m := range c.Members {
		cs := idSet{}
		for _, v := range m.Capabilities {
			cs[v] = true
		}
		capabilities[m.ID] = cs
	}
	deps := map[string][]string{}
	outputs := map[string]idSet{}
	coveredCriteria, coveredOutputs := idSet{}, idSet{}
	for _, t := range p.Tasks {
		if _, ok := deps[t.ID]; ok || !nonempty(t.ID) || !nonempty(t.Instruction) || t.Priority < 0 || t.Priority > 100 {
			return invalid("任务%q身份、指令或优先级无效", t.ID)
		}
		if _, ok := groupDeps[t.GroupID]; t.GroupID != "" && !ok {
			return invalid("任务%q引用未知分组", t.ID)
		}
		cs, ok := capabilities[t.MemberID]
		if !ok || !refs(t.RequiredCapabilities, cs) {
			return invalid("任务%q成员或能力无效", t.ID)
		}
		if !refs(t.GoalInputIDs, sources) || len(t.CriterionIDs) == 0 || !refs(t.CriterionIDs, criteria) {
			return invalid("任务%q目标引用无效", t.ID)
		}
		for _, id := range t.CriterionIDs {
			coveredCriteria[id] = true
		}
		if _, err := criteriaSet(t.Criteria); err != nil {
			return err
		}
		if len(t.Outputs) == 0 {
			return invalid("任务%q缺少输出", t.ID)
		}
		names := idSet{}
		for _, o := range t.Outputs {
			if !names.add(o.Name) || !nonempty(o.MediaType) || o.MaxBytes < 0 || o.MaxBytes > defaultLimit(c.MaxArtifactBytes, 65536) {
				return invalid("任务%q输出契约无效", t.ID)
			}
			if o.DeliverableID != "" {
				if deliverables[o.DeliverableID] != o.MediaType {
					return invalid("任务%q交付物绑定无效", t.ID)
				}
				coveredOutputs[o.DeliverableID] = true
			}
		}
		outputs[t.ID] = names
		deps[t.ID] = t.DependsOn
	}
	if err := validateDAG(deps); err != nil {
		return err
	}
	for _, t := range p.Tasks {
		parents := idSet{}
		for _, id := range t.DependsOn {
			parents[id] = true
		}
		seen := map[InputRef]bool{}
		for _, r := range t.Inputs {
			if seen[r] || !parents[r.DependencyID] || !outputs[r.DependencyID][r.OutputName] {
				return invalid("任务%q输入引用无效", t.ID)
			}
			seen[r] = true
		}
	}
	if len(coveredOutputs) != len(deliverables) || len(coveredCriteria) != len(criteria) {
		return invalid("目标验收或交付物未完整覆盖")
	}
	return nil
}
func validateDAG(deps map[string][]string) error {
	next := map[string][]string{}
	counts := map[string]int{}
	var queue []string
	for id, parents := range deps {
		seen := idSet{}
		for _, p := range parents {
			if _, ok := deps[p]; !ok || p == id || !seen.add(p) {
				return invalid("%q依赖%q无效或重复", id, p)
			}
			next[p] = append(next[p], id)
		}
		counts[id] = len(parents)
		if len(parents) == 0 {
			queue = append(queue, id)
		}
	}
	for i := 0; i < len(queue); i++ {
		for _, child := range next[queue[i]] {
			counts[child]--
			if counts[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if len(queue) != len(deps) {
		return invalid("依赖存在环")
	}
	return nil
}
