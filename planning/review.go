package planning

import (
	"slices"
	"strconv"

	"github.com/kart-io/goagent/core/durable"
)

type reviewScope struct {
	tasks     map[string]TaskSpec
	results   map[string]TaskResult
	artifacts map[string]Artifact
	owners    map[string]string
	criteria  []Criterion
	visible   idSet
}

func inspectReview(in ReviewInput) (reviewScope, error) {
	s := reviewScope{tasks: map[string]TaskSpec{}, results: map[string]TaskResult{}, artifacts: map[string]Artifact{}, owners: map[string]string{}, visible: idSet{}}
	if !nonempty(in.SubjectDigest) || in.Final != (in.Assignment == nil) {
		return s, invalid("审查对象无效")
	}
	if in.Final {
		s.criteria = in.Goal.Criteria
		for _, t := range in.Plan.Tasks {
			s.tasks[t.ID] = t
		}
	} else {
		a := in.Assignment
		if len(in.Results) != 1 || in.Results[0].TaskID != a.Task.ID || in.Results[0].AttemptKey != a.AttemptKey {
			return s, invalid("结果与受审任务不符")
		}
		s.criteria = a.Task.Criteria
		s.tasks[a.Task.ID] = a.Task
		for _, v := range a.GoalInputs {
			s.visible[v.ID] = true
		}
		for _, v := range a.Inputs {
			s.visible[v.Artifact.ID] = true
			s.owners[v.Artifact.ID] = v.Ref.DependencyID
		}
	}
	for _, c := range s.criteria {
		s.visible[c.ID] = true
	}
	for _, r := range in.Results {
		t, ok := s.tasks[r.TaskID]
		if !ok || s.results[r.TaskID].TaskID != "" || !nonempty(r.AttemptKey) {
			return s, invalid("审查结果任务无效")
		}
		if r.Outcome != "produced" && r.Outcome != "rejected" {
			return s, invalid("审查结果状态无效")
		}
		s.results[r.TaskID] = r
		s.visible[r.TaskID] = true
		s.visible[r.AttemptKey] = true
		outputs := map[string]OutputSpec{}
		for _, o := range t.Outputs {
			outputs[o.Name] = o
		}
		seen := idSet{}
		for _, a := range r.Artifacts {
			o, ok := outputs[a.Name]
			if !ok || !seen.add(a.Name) || a.MediaType != o.MediaType || a.DeliverableID != o.DeliverableID || a.ProducerAttemptKey != r.AttemptKey || a.ID != durable.ChildKey(r.AttemptKey, "artifact/"+a.Name) {
				return s, invalid("受审产物身份或契约不符")
			}
			d, e := artifactDigest(a)
			if e != nil || d != a.Digest {
				return s, invalid("受审产物摘要不符")
			}
			if !in.Final {
				for _, ref := range a.Sources {
					if e := ValidateEvidence(ref, in.Assignment.GoalInputs, directArtifacts(*in.Assignment)); e != nil {
						return s, e
					}
				}
			}
			if _, ok := s.artifacts[a.ID]; ok {
				return s, invalid("受审产物ID重复")
			}
			s.artifacts[a.ID] = a
			s.owners[a.ID] = t.ID
			s.visible[a.ID] = true
		}
	}
	if in.Final {
		seen := idSet{}
		for _, a := range in.Accepted {
			v, ok := s.artifacts[a.ID]
			d, e := artifactDigest(a)
			if !ok || !seen.add(a.ID) || e != nil || d != v.Digest || a.Digest != d || a.Name != v.Name || a.DeliverableID != v.DeliverableID || a.ProducerAttemptKey != v.ProducerAttemptKey {
				return s, invalid("最终产物集合与当前结果不符")
			}
		}
		if len(seen) != len(s.artifacts) {
			return s, invalid("最终产物集合不完整")
		}
	}
	return s, nil
}

// ValidateReview checks the frozen subject and rewrites local issue references.
func ValidateReview(in ReviewInput, r Review) (Review, error) {
	if r.SubjectDigest != in.SubjectDigest || !nonempty(r.Reason) {
		return Review{}, invalid("审查摘要不符或缺少原因")
	}
	s, e := inspectReview(in)
	if e != nil {
		return Review{}, e
	}
	expected, e := criteriaSet(s.criteria)
	if e != nil {
		return Review{}, e
	}
	required := idSet{}
	for _, c := range s.criteria {
		required[c.ID] = c.EvidenceRequired
	}
	seen := idSet{}
	for _, c := range r.Criteria {
		if !expected[c.CriterionID] || !seen.add(c.CriterionID) || !slices.Contains([]string{"pass", "fail", "unknown"}, c.Status) {
			return Review{}, invalid("验收结果%q无效", c.CriterionID)
		}
		if !refs(c.EvidenceIDs, nil) {
			return Review{}, invalid("证据ID重复或为空")
		}
		for _, id := range c.EvidenceIDs {
			if _, ok := s.artifacts[id]; !ok {
				return Review{}, invalid("验收引用不可见产物%q", id)
			}
		}
		if c.Status == "pass" && required[c.CriterionID] && len(c.EvidenceIDs) == 0 {
			return Review{}, invalid("验收通过缺少证据")
		}
		if r.Verdict == "accept" && c.Status != "pass" {
			return Review{}, invalid("不能接受未通过的验收项")
		}
	}
	if len(seen) != len(expected) {
		return Review{}, invalid("验收结果有缺项")
	}
	if r.Verdict == "accept" {
		covered := idSet{}
		if len(s.tasks) == 0 || len(s.results) != len(s.tasks) {
			return Review{}, invalid("受审任务未完成")
		}
		for id, t := range s.tasks {
			result := s.results[id]
			if result.Outcome != "produced" || len(result.Artifacts) != len(t.Outputs) {
				return Review{}, invalid("不能接受被拒绝或缺少输出的结果")
			}
			for _, a := range result.Artifacts {
				covered[a.DeliverableID] = true
			}
		}
		if in.Final {
			for _, d := range in.Goal.Deliverables {
				if !covered[d.ID] {
					return Review{}, invalid("交付物%q缺少产物", d.ID)
				}
			}
		}
	}
	if e := checkRepair(in, r, s); e != nil {
		return Review{}, e
	}
	out := r.clone()
	out.ID = ""
	ids := map[string]string{}
	for i, issue := range out.Issues {
		id := durable.ChildKey(in.SubjectDigest, "issue/"+strconv.Itoa(i+1))
		ids[issue.ID] = id
		out.Issues[i].ID = id
	}
	if out.Repair != nil {
		for i, id := range out.Repair.IssueIDs {
			out.Repair.IssueIDs[i] = ids[id]
		}
	}
	return out, nil
}
