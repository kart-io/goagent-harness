package planning

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTaskReviewAcceptsFrozenInputArtifactEvidence(t *testing.T) {
	a, r, c := assignmentFixture()
	upstream := a
	upstream.Task.ID = "upstream"
	upstream.AttemptKey = "upstream-attempt"
	prior := r
	prior.TaskID = upstream.Task.ID
	prior.AttemptKey = upstream.AttemptKey
	produced, err := ValidateTaskResult(upstream, prior, c)
	require.NoError(t, err)
	ref := InputRef{DependencyID: upstream.Task.ID, OutputName: "out"}
	a.Task.Inputs = []InputRef{ref}
	a.Task.DependsOn = []string{ref.DependencyID}
	a.Inputs = []ResolvedInput{{Ref: ref, Artifact: produced.Artifacts[0]}}
	a.Task.Criteria[0].EvidenceRequired = true
	current, err := ValidateTaskResult(a, r, c)
	require.NoError(t, err)
	in := ReviewInput{Assignment: &a, Results: []TaskResult{current}, SubjectDigest: "subject"}
	review := Review{SubjectDigest: "subject", Verdict: "accept", Reason: "引用已验证材料", Criteria: []CriterionResult{{CriterionID: "local", Status: "pass", EvidenceIDs: []string{a.Inputs[0].Artifact.ID}}}}
	_, err = ValidateReview(in, review)
	require.NoError(t, err)
	for _, id := range []string{"local", a.Task.ID} {
		review.Criteria[0].EvidenceIDs = []string{id}
		_, err = ValidateReview(in, review)
		require.Error(t, err)
	}
	review.Criteria[0].EvidenceIDs = []string{a.Inputs[0].Artifact.ID}
	a.Inputs[0].Artifact.Content = []byte("43")
	_, err = ValidateReview(in, review)
	require.Error(t, err)
}

func TestFinalRepairRecognizesRelatedVisibleTask(t *testing.T) {
	a, r, c := assignmentFixture()
	g, _, _ := validationFixture()
	v, e := ValidateTaskResult(a, r, c)
	require.NoError(t, e)
	in := ReviewInput{Final: true, Goal: g, Plan: Plan{Tasks: []TaskSpec{a.Task}}, Results: []TaskResult{v}, Accepted: v.Artifacts, SubjectDigest: "final"}
	rev := Review{Verdict: "replan", Reason: "补齐来源", SubjectDigest: "final", Criteria: []CriterionResult{{CriterionID: "g", Status: "fail"}}, Issues: []Issue{{ID: "i", CriterionID: "g", Severity: "error", Reason: "缺少来源", Category: "source_missing", RelatedIDs: []string{a.Task.ID}}}, Repair: &RepairDecision{Action: "replan", IssueIDs: []string{"i"}, TargetTaskIDs: []string{a.Task.ID}}}
	_, e = ValidateReview(in, rev)
	require.NoError(t, e)
	rev.Repair.TargetTaskIDs = []string{"unknown"}
	_, e = ValidateReview(in, rev)
	require.Error(t, e)
}
