package planning

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/kart-io/goagent/core/durable"
)

// ordered copies a set before sorting it for hashing.
func ordered[T any](items []T, key func(T) string) []T {
	out := append([]T{}, items...)
	slices.SortFunc(out, func(a, b T) int { return strings.Compare(key(a), key(b)) })
	return out
}
func orderedStrings(s []string) []string { return ordered(s, func(v string) string { return v }) }
func compareRef(a, b InputRef) int {
	return cmp.Or(strings.Compare(a.DependencyID, b.DependencyID), strings.Compare(a.OutputName, b.OutputName))
}
func orderedCriteria(v []Criterion) []Criterion {
	return ordered(v, func(x Criterion) string { return x.ID })
}

// GoalDigest binds every field of the goal, including canonical source bodies.
func GoalDigest(g GoalSpec) (string, error) {
	g.Constraints = ordered(g.Constraints, func(x Constraint) string { return x.ID })
	g.Criteria = orderedCriteria(g.Criteria)
	g.Deliverables = ordered(g.Deliverables, func(x Deliverable) string { return x.ID })
	g.Scope.Included = orderedStrings(g.Scope.Included)
	g.Scope.Excluded = orderedStrings(g.Scope.Excluded)
	g.Inputs = ordered(g.Inputs, func(x SourceInput) string { return x.ID })
	for i, s := range g.Inputs {
		body, err := canonicalJSON(s.Content)
		if err != nil {
			return "", err
		}
		digest, err := SourceDigest(body)
		if err != nil || s.Digest != digest {
			return "", invalid("目标来源%q摘要不符", s.ID)
		}
		g.Inputs[i].Content = body
	}
	g.Parameters = ordered(g.Parameters, func(x GoalParameter) string { return x.Key })
	g.Assumptions = ordered(g.Assumptions, func(x Assumption) string { return x.ID })
	return digestJSON(g)
}

type taskContract struct {
	Instruction, MemberID, EffectClass, Version, ManifestDigest string
	DependsOn, Capabilities                                     []string
	Inputs                                                      []InputRef
	Sources                                                     []sourceIdentity
	Outputs                                                     []OutputSpec
	Criteria, GoalCriteria                                      []Criterion
	Claims                                                      []ResourceClaim
}
type sourceIdentity struct{ ID, Digest string }

func selectedSources(g GoalSpec, t TaskSpec) ([]SourceInput, error) {
	byID := map[string]SourceInput{}
	for _, s := range g.Inputs {
		byID[s.ID] = s
	}
	out := []SourceInput{}
	if !refs(t.GoalInputIDs, nil) {
		return nil, invalid("任务材料引用无效")
	}
	for _, id := range orderedStrings(t.GoalInputIDs) {
		s, ok := byID[id]
		if !ok {
			return nil, invalid("未知目标材料%q", id)
		}
		out = append(out, s)
	}
	return out, nil
}
func semanticContract(g GoalSpec, t TaskSpec, m Member) (taskContract, error) {
	var v taskContract
	if t.MemberID != m.ID {
		return v, invalid("任务成员%q与目录身份不匹配", t.MemberID)
	}
	if err := validateMemberConfig(m); err != nil {
		return v, err
	}
	src, err := selectedSources(g, t)
	if err != nil {
		return v, err
	}
	v = taskContract{Instruction: t.Instruction, MemberID: m.ID, EffectClass: m.EffectClass, Version: m.Version, ManifestDigest: m.ManifestDigest, DependsOn: orderedStrings(t.DependsOn), Capabilities: orderedStrings(t.RequiredCapabilities), Outputs: ordered(t.Outputs, func(x OutputSpec) string { return x.Name }), Criteria: orderedCriteria(t.Criteria), Claims: ordered(m.Claims, func(x ResourceClaim) string { return x.Key }), Inputs: append([]InputRef{}, t.Inputs...)}
	slices.SortFunc(v.Inputs, compareRef)
	for _, s := range src {
		v.Sources = append(v.Sources, sourceIdentity{s.ID, s.Digest})
	}
	cs := map[string]Criterion{}
	for _, c := range g.Criteria {
		cs[c.ID] = c
	}
	for _, id := range orderedStrings(t.CriterionIDs) {
		c, ok := cs[id]
		if !ok {
			return v, invalid("未知目标验收项%q", id)
		}
		v.GoalCriteria = append(v.GoalCriteria, c)
	}
	return v, nil
}

// Fingerprint binds the trusted execution contract and all actual input digests.
func Fingerprint(g GoalSpec, t TaskSpec, m Member, inputs []ResolvedInput) (string, error) {
	d, err := GoalDigest(g)
	if err != nil {
		return "", err
	}
	return fingerprint(d, g, t, m, inputs)
}
func fingerprint(d string, g GoalSpec, t TaskSpec, m Member, inputs []ResolvedInput) (string, error) {
	v, err := semanticContract(g, t, m)
	if err != nil {
		return "", err
	}
	if len(inputs) != len(v.Inputs) {
		return "", invalid("实际输入数量与任务不符")
	}
	type binding struct {
		Ref    InputRef
		Digest string
	}
	actual := map[InputRef]string{}
	for _, in := range inputs {
		a := in.Artifact
		h, err := artifactDigest(a)
		if err != nil || h != a.Digest || actual[in.Ref] != "" || !slices.Contains(t.DependsOn, in.Ref.DependencyID) || a.Name != in.Ref.OutputName || !nonempty(a.ProducerAttemptKey) || a.ID != durable.ChildKey(a.ProducerAttemptKey, "artifact/"+a.Name) {
			return "", invalid("输入%v的引用、身份或摘要无效", in.Ref)
		}
		actual[in.Ref] = h
	}
	bindings := []binding{}
	for _, r := range v.Inputs {
		h, ok := actual[r]
		if !ok {
			return "", invalid("任务输入%v缺失或重复", r)
		}
		bindings = append(bindings, binding{r, h})
		delete(actual, r)
	}
	return digestJSON(struct {
		Version, GoalDigest string
		Contract            taskContract
		Inputs              []binding
	}{"fingerprint-v1", d, v, bindings})
}

// AttemptKey derives the execution identity from the root run and task counters.
func AttemptKey(rootRunID, taskID string, generation, number int) (string, error) {
	if !nonempty(rootRunID) || !nonempty(taskID) || generation < 1 || number < 1 {
		return "", invalid("运行身份、任务身份或尝试序号无效")
	}
	return durable.ChildKey(rootRunID, fmt.Sprintf("planning-task/%s/g/%d/a/%d", taskID, generation, number)), nil
}
