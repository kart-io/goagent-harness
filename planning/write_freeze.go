package planning

import (
	"cmp"
	"slices"
)

func writeContract(g GoalSpec, p Plan, c Config) (string, map[string]bool, error) {
	members := memberDirectory(c)
	tasks := map[string]TaskSpec{}
	closure := map[string]bool{}
	var queue []string
	for _, t := range p.Tasks {
		tasks[t.ID] = t
		if members[t.MemberID].EffectClass == "write" {
			queue = append(queue, t.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if closure[id] {
			continue
		}
		closure[id] = true
		t, ok := tasks[id]
		if !ok {
			return "", nil, invalid("冻结任务%q不存在", id)
		}
		queue = append(queue, t.DependsOn...)
	}
	type entry struct {
		TaskID   string
		Contract taskContract
	}
	entries := []entry{}
	for _, t := range ordered(p.Tasks, func(t TaskSpec) string { return t.ID }) {
		if closure[t.ID] {
			contract, err := semanticContract(g, t, members[t.MemberID])
			if err != nil {
				return "", nil, err
			}
			entries = append(entries, entry{t.ID, contract})
		}
	}
	d, err := digestJSON(entries)
	return d, closure, err
}
func buildWriteFreeze(s State, c Config, accepted map[string]Attempt) (*WriteFreeze, error) {
	d, closure, err := writeContract(s.Goal, *s.Plan, c)
	if err != nil {
		return nil, err
	}
	known := map[InputRef]string{}
	for id, a := range accepted {
		if closure[id] {
			for _, v := range a.Result.Artifacts {
				known[InputRef{id, v.Name}] = v.Digest
			}
		}
	}
	if prior := s.WriteFreeze; prior != nil {
		if d != prior.ContractDigest {
			return nil, invalid("写任务或其上游契约已改变")
		}
		for _, v := range prior.KnownOutputs {
			if known[InputRef{v.TaskID, v.Name}] != v.Digest {
				return nil, invalid("已冻结产物%q/%q缺失或改变", v.TaskID, v.Name)
			}
		}
	}
	f := &WriteFreeze{ContractDigest: d}
	for r, h := range known {
		f.KnownOutputs = append(f.KnownOutputs, FrozenOutput{r.DependencyID, r.OutputName, h})
	}
	slices.SortFunc(f.KnownOutputs, func(a, b FrozenOutput) int {
		return cmp.Or(cmp.Compare(a.TaskID, b.TaskID), cmp.Compare(a.Name, b.Name))
	})
	return f, nil
}
