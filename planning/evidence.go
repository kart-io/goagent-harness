package planning

import (
	"slices"
	"strconv"
	"strings"

	"github.com/kart-io/goagent/core/durable"
)

// ValidateEvidence verifies a JSON pointer and its quote against frozen sources.
func ValidateEvidence(r EvidenceRef, inputs []SourceInput, artifacts []Artifact) error {
	if r.SourceDigest == "" || r.QuoteDigest == "" {
		return invalid("持久引用的来源或定位摘要缺失")
	}
	_, err := resolveEvidence(r, inputs, artifacts)
	return err
}

func resolveEvidence(r EvidenceRef, inputs []SourceInput, artifacts []Artifact) (EvidenceRef, error) {
	if r.LocatorKind != "json_pointer" {
		return EvidenceRef{}, invalid("定位类型%q不可验真", r.LocatorKind)
	}
	var body []byte
	var digest string
	matches := 0
	for _, s := range inputs {
		if s.ID == r.SourceID {
			matches++
			body = s.Content
			d, e := SourceDigest(body)
			if e != nil || d != s.Digest {
				return EvidenceRef{}, invalid("来源正文与摘要不一致")
			}
			digest = d
		}
	}
	for _, a := range artifacts {
		if a.ID == r.SourceID {
			matches++
			body = a.Content
			d, e := artifactDigest(a)
			if e != nil || d != a.Digest {
				return EvidenceRef{}, invalid("产物正文与摘要不一致")
			}
			digest = d
		}
	}
	if matches != 1 || !nonempty(r.SourceID) {
		return EvidenceRef{}, invalid("来源%q不可见或重复", r.SourceID)
	}
	var value any
	if err := DecodeCandidate(body, &value); err != nil {
		return EvidenceRef{}, err
	}
	if r.Locator != "" {
		if r.Locator[0] != '/' {
			return EvidenceRef{}, invalid("定位必须为JSON指针")
		}
		for _, raw := range strings.Split(r.Locator[1:], "/") {
			for i := 0; i < len(raw); i++ {
				if raw[i] == '~' {
					i++
					if i == len(raw) || (raw[i] != '0' && raw[i] != '1') {
						return EvidenceRef{}, invalid("JSON指针转义无效")
					}
				}
			}
			key := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
			switch node := value.(type) {
			case map[string]any:
				var ok bool
				value, ok = node[key]
				if !ok {
					return EvidenceRef{}, invalid("JSON指针字段不存在")
				}
			case []any:
				idx, e := strconv.Atoi(key)
				if e != nil || idx < 0 || strconv.Itoa(idx) != key || idx >= len(node) {
					return EvidenceRef{}, invalid("JSON指针数组下标无效")
				}
				value = node[idx]
			default:
				return EvidenceRef{}, invalid("JSON指针指向标量之外")
			}
		}
	}
	d, e := digestJSON(value)
	if e != nil {
		return EvidenceRef{}, e
	}
	if r.SourceDigest != "" && r.SourceDigest != digest || r.QuoteDigest != "" && r.QuoteDigest != d {
		return EvidenceRef{}, invalid("来源或定位内容的摘要不符")
	}
	r.SourceDigest = digest
	r.QuoteDigest = d
	return r, nil
}

func directArtifacts(a Assignment) []Artifact {
	items := make([]Artifact, len(a.Inputs))
	for i, r := range a.Inputs {
		items[i] = r.Artifact
	}
	return items
}
func oversizeResult(a Assignment) TaskResult {
	return TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "rejected", FailureKind: "size_limit", Reason: "产物正文超出大小限制"}
}

// ValidateTaskResult checks a result and assigns host-owned artifact identities.
func ValidateTaskResult(a Assignment, r TaskResult, c Config) (TaskResult, error) {
	if !nonempty(a.Task.ID) || !nonempty(a.AttemptKey) || a.Task.ID != r.TaskID || a.AttemptKey != r.AttemptKey {
		return TaskResult{}, invalid("结果与冻结任务身份不符")
	}
	if r.Outcome != "produced" && r.Outcome != "rejected" {
		return TaskResult{}, invalid("结果状态无效")
	}
	if r.Outcome == "rejected" && !nonempty(r.Reason) {
		return TaskResult{}, invalid("拒绝结果必须说明原因")
	}
	specs := map[string]OutputSpec{}
	for _, s := range a.Task.Outputs {
		specs[s.Name] = s
	}
	result := TaskResult{TaskID: r.TaskID, AttemptKey: r.AttemptKey, Outcome: r.Outcome, Reason: boundedText(r.Reason), FailureKind: boundedText(r.FailureKind)}
	seen := idSet{}
	remaining := defaultLimit(c.MaxArtifactTotalBytes, 1048576)
	inputs := directArtifacts(a)
	for _, out := range r.Artifacts {
		s, ok := specs[out.Name]
		if !ok || !seen.add(out.Name) || out.MediaType != s.MediaType {
			return TaskResult{}, invalid("输出%q未声明、重复或类型不符", out.Name)
		}
		limit := defaultLimit(c.MaxArtifactBytes, 65536)
		if s.MaxBytes > 0 {
			limit = min(limit, s.MaxBytes)
		}
		if len(out.Content) > min(limit, remaining) {
			return oversizeResult(a), nil
		}
		b, e := canonicalJSON(out.Content)
		if e != nil {
			return TaskResult{}, e
		}
		if len(b) > min(limit, remaining) {
			return oversizeResult(a), nil
		}
		remaining -= len(b)
		out.Content = b
		out.Sources = slices.Clone(out.Sources)
		for i, ref := range out.Sources {
			resolved, err := resolveEvidence(ref, a.GoalInputs, inputs)
			if err != nil {
				return TaskResult{}, err
			}
			out.Sources[i] = resolved
		}
		out.ID = durable.ChildKey(a.AttemptKey, "artifact/"+out.Name)
		out.ProducerAttemptKey = a.AttemptKey
		out.DeliverableID = s.DeliverableID
		out.Digest, e = artifactDigest(out)
		if e != nil {
			return TaskResult{}, e
		}
		result.Artifacts = append(result.Artifacts, out)
	}
	if r.Outcome == "produced" && len(seen) != len(specs) {
		return TaskResult{}, invalid("任务未提供全部约定输出")
	}
	return result, nil
}
