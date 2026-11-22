package planning

import "slices"

// ValidateRepair checks that the proposed action addresses visible issues.
func ValidateRepair(in ReviewInput, r Review) error {
	s, e := inspectReview(in)
	if e != nil {
		return e
	}
	if in.SubjectDigest != r.SubjectDigest {
		return invalid("修复对象摘要不符")
	}
	return checkRepair(in, r, s)
}
func checkRepair(in ReviewInput, r Review, s reviewScope) error {
	if r.Verdict == "accept" {
		if len(r.Issues) > 0 || r.Repair != nil {
			return invalid("接受结果不能有待解决问题")
		}
		return nil
	}
	p := r.Repair
	if p == nil || p.Action != r.Verdict || !slices.Contains([]string{"retry", "replan", "block"}, r.Verdict) || len(r.Issues) == 0 {
		return invalid("修复动作与审查裁决不符")
	}
	if r.Verdict == "retry" {
		if in.Final {
			return invalid("最终审查不能重试")
		}
		for _, claim := range in.Assignment.Claims {
			if claim.Mode == "write" {
				return invalid("写任务不能自动重试")
			}
		}
	}
	issues := idSet{}
	related := idSet{}
	criteria := idSet{}
	for _, c := range s.criteria {
		criteria[c.ID] = true
	}
	for _, issue := range r.Issues {
		if !issues.add(issue.ID) || !slices.Contains([]string{"value_mismatch", "source_missing", "definition_mismatch", "format_missing", "execution_rejected"}, issue.Category) || !slices.Contains([]string{"error", "unknown"}, issue.Severity) || !nonempty(issue.Reason) {
			return invalid("问题%q类别、严重度或描述无效", issue.ID)
		}
		if issue.CriterionID != "" && !criteria[issue.CriterionID] || !refs(issue.RelatedIDs, s.visible) {
			return invalid("问题%q引用不可见对象", issue.ID)
		}
		result, ok := s.results[issue.TaskID]
		if !in.Final || issue.TaskID != "" || issue.AttemptKey != "" {
			if !ok || (!in.Final || issue.AttemptKey != "") && issue.AttemptKey != result.AttemptKey {
				return invalid("问题任务或尝试不可见")
			}
			related[issue.TaskID] = true
		}
		if issue.ArtifactID != "" {
			a, ok := s.artifacts[issue.ArtifactID]
			if !ok || !nonempty(issue.Location) || issue.TaskID != "" && s.owners[a.ID] != issue.TaskID {
				return invalid("问题产物或位置无效")
			}
			related[s.owners[a.ID]] = true
		} else if issue.TaskID != "" {
			found := false
			for _, o := range s.tasks[issue.TaskID].Outputs {
				if issue.Location == o.Name {
					found = true
					for _, a := range result.Artifacts {
						if a.Name == o.Name {
							return invalid("已存在的输出必须引用产物ID")
						}
					}
				}
			}
			if !found {
				return invalid("缺失产物的位置必须是约定输出名")
			}
		}
		for _, id := range issue.RelatedIDs {
			if owner := s.owners[id]; owner != "" {
				related[owner] = true
			}
		}
	}
	if !refs(p.IssueIDs, issues) || len(p.IssueIDs) != len(issues) || !refs(p.TargetTaskIDs, nil) {
		return invalid("修复问题或目标引用无效")
	}
	if p.Action != "block" && len(p.TargetTaskIDs) == 0 {
		return invalid("修复必须指定目标任务")
	}
	// 只有实际传递数据的边才能将重规划目标与问题关联。
	if p.Action == "replan" {
		neighbors := map[string][]string{}
		for _, t := range in.Plan.Tasks {
			for _, i := range t.Inputs {
				neighbors[t.ID] = append(neighbors[t.ID], i.DependencyID)
				neighbors[i.DependencyID] = append(neighbors[i.DependencyID], t.ID)
			}
		}
		original := make([]string, 0, len(related))
		for id := range related {
			original = append(original, id)
		}
		for _, id := range original {
			for _, adj := range neighbors[id] {
				related[adj] = true
			}
		}
	}
	for _, id := range p.TargetTaskIDs {
		if !related[id] || p.Action == "retry" && id != in.Assignment.Task.ID {
			return invalid("修复目标%q与问题无关", id)
		}
		if _, ok := s.tasks[id]; !ok && !slices.ContainsFunc(in.Plan.Tasks, func(t TaskSpec) bool { return t.ID == id }) {
			return invalid("修复目标不在当前计划")
		}
	}
	return nil
}
