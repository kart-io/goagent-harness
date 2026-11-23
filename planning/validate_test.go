package planning

import (
	"errors"
	"github.com/stretchr/testify/require"
	"testing"
)

func validationFixture() (GoalSpec, Config, TaskSpec) {
	g := GoalSpec{ID: "goal", Revision: 1, Objective: "发布", Scope: Scope{Included: []string{"报告"}}, Output: OutputProfile{Language: "zh", Format: "json"}, Criteria: []Criterion{{ID: "g", Description: "内容完整"}}, Deliverables: []Deliverable{{ID: "doc", Description: "报告", MediaType: "application/json"}}}
	cfg := Config{MaxTasks: 4, MaxArtifactBytes: 256, MaxArtifactTotalBytes: 512, Members: []Member{{ID: "m", EffectClass: "read_only"}}}
	task := TaskSpec{ID: "a", Instruction: "编写", MemberID: "m", CriterionIDs: []string{"g"}, Criteria: []Criterion{{ID: "local", Description: "完成"}}, Outputs: []OutputSpec{{Name: "out", MediaType: "application/json", DeliverableID: "doc", MaxBytes: 100}}}
	return g, cfg, task
}
func TestRejectCycleAndUnknownDependency(t *testing.T) {
	g, cfg, a := validationFixture()
	b := a
	b.ID = "b"
	b.DependsOn = []string{"a"}
	check := func() error { return ValidateDraft(g, PlanDraft{Tasks: []TaskSpec{a, b}}, cfg) }
	require.NoError(t, check())
	a.DependsOn = []string{"b"}
	require.Error(t, check())
	a.DependsOn = []string{"unknown"}
	require.Error(t, check())
}
func TestGoalSourceAndPolicy(t *testing.T) {
	g, cfg, _ := validationFixture()
	cfg.RequiredGoalParameters = []string{"version"}
	require.Error(t, ValidateGoalPolicy(g, cfg))
	g.Parameters = []GoalParameter{{Key: "version", Value: "1", Origin: "host"}}
	d, e := SourceDigest([]byte("42"))
	require.NoError(t, e)
	g.Inputs = []SourceInput{{ID: "s", Title: "材料", Origin: "host", MediaType: "application/json", Content: []byte("42"), Digest: d}}
	require.NoError(t, ValidateGoalPolicy(g, cfg))
	g.Inputs[0].Content = []byte("43")
	require.Error(t, ValidateGoal(g))
}
func TestStrictCanonicalCandidate(t *testing.T) {
	b, e := canonicalJSON([]byte(`{"z":9007199254740993,"a":1.0}`))
	require.NoError(t, e)
	require.Equal(t, `{"a":1.0,"z":9007199254740993}`, string(b))
	for _, s := range []string{`{"x":1,"x":2}`, `{} {}`, string([]byte{34, 255, 34})} {
		_, e = canonicalJSON([]byte(s))
		require.Error(t, e)
	}
	for _, s := range []string{`{"ID":"x"}`, `{"extra":0}`} {
		require.Error(t, DecodeCandidate([]byte(s), &Criterion{}))
	}
}
func assignmentFixture() (Assignment, TaskResult, Config) {
	_, cfg, task := validationFixture()
	a := Assignment{Task: task, AttemptKey: "k"}
	return a, TaskResult{TaskID: "a", AttemptKey: "k", Outcome: "produced", Artifacts: []Artifact{{Name: "out", MediaType: "application/json", Content: []byte("42")}}}, cfg
}
func TestUndeclaredSourceArtifactIsRejected(t *testing.T) {
	a, r, c := assignmentFixture()
	r.Artifacts[0].Sources = []EvidenceRef{{SourceID: "secret", LocatorKind: "json_pointer"}}
	_, e := ValidateTaskResult(a, r, c)
	require.ErrorAs(t, e, new(*CandidateError))
}
func TestEvidencePointer(t *testing.T) {
	s := SourceInput{ID: "s", Content: []byte(`{"a/b":{"~":[9007199254740993]}}`)}
	s.Digest, _ = SourceDigest(s.Content)
	q, _ := SourceDigest([]byte("9007199254740993"))
	ref := EvidenceRef{SourceID: "s", SourceDigest: s.Digest, QuoteDigest: q, LocatorKind: "json_pointer", Locator: "/a~1b/~0/0"}
	check := func() error { return ValidateEvidence(ref, []SourceInput{s}, nil) }
	require.NoError(t, check())
	for _, p := range []string{"/a~1b/~0/00", "/a~1b/~0/1", "/a~2b", "/a~"} {
		ref.Locator = p
		require.Error(t, check())
	}
}
func rejectedFixture() (ReviewInput, Review, Config) {
	a, r, c := assignmentFixture()
	r.Outcome = "rejected"
	r.Reason = "缺少正文"
	r.FailureKind = "format_missing"
	r.Artifacts = nil
	in := ReviewInput{Assignment: &a, SubjectDigest: "subject", Results: []TaskResult{r}, Plan: Plan{Tasks: []TaskSpec{a.Task}}}
	review := Review{SubjectDigest: "subject", Verdict: "retry", Reason: "补齐正文", Criteria: []CriterionResult{{CriterionID: "local", Status: "fail", Reason: "缺少正文"}}, Issues: []Issue{{ID: "i", Category: "execution_rejected", Severity: "error", TaskID: "a", AttemptKey: "k", CriterionID: "local", Location: "out", Reason: "缺少正文"}}, Repair: &RepairDecision{Action: "retry", IssueIDs: []string{"i"}, TargetTaskIDs: []string{"a"}}}
	return in, review, c
}
func TestRejectedWithoutArtifactsReachesReview(t *testing.T) {
	in, r, c := rejectedFixture()
	v, e := ValidateTaskResult(*in.Assignment, in.Results[0], c)
	require.NoError(t, e)
	in.Results[0] = v
	validated, e := ValidateReview(in, r)
	require.NoError(t, e)
	require.NotEqual(t, "i", validated.Issues[0].ID)
	require.Equal(t, validated.Issues[0].ID, validated.Repair.IssueIDs[0])
}
func TestOversizeResultIsBoundedRejection(t *testing.T) {
	a, r, c := assignmentFixture()
	c.MaxArtifactBytes = 1
	v, e := ValidateTaskResult(a, r, c)
	require.NoError(t, e)
	require.Equal(t, "rejected", v.Outcome)
	require.Empty(t, v.Artifacts)
	r.TaskID = "foreign"
	_, e = ValidateTaskResult(a, r, c)
	require.Error(t, e)
}
func TestWriteRejectionDoesNotProveNoEffect(t *testing.T) {
	in, r, _ := rejectedFixture()
	in.Assignment.Claims = []ResourceClaim{{Key: "workspace", Mode: "write"}}
	_, e := ValidateReview(in, r)
	require.Error(t, e)
}
func TestRepairTargetsMustMatchVisibleIssue(t *testing.T) {
	in, r, _ := rejectedFixture()
	in.Plan.Tasks = append(in.Plan.Tasks, TaskSpec{ID: "other"})
	r.Repair.TargetTaskIDs = []string{"other"}
	require.Error(t, ValidateRepair(in, r))
}
func TestAcceptRejectsFailedUnknownAndRejectedResult(t *testing.T) {
	in, r, c := rejectedFixture()
	r.Repair = nil
	r.Issues = nil
	r.Verdict = "accept"
	r.Criteria[0].Status = "pass"
	_, e := ValidateReview(in, r)
	require.Error(t, e)
	a, result, _ := assignmentFixture()
	in.Results[0], e = ValidateTaskResult(a, result, c)
	require.NoError(t, e)
	_, e = ValidateReview(in, r)
	require.NoError(t, e)
	for _, s := range []string{"fail", "unknown"} {
		r.Criteria[0].Status = s
		_, e = ValidateReview(in, r)
		require.True(t, errors.As(e, new(*CandidateError)))
	}
}

func TestHostArtifactBindingAndFinalCoverage(t *testing.T) {
	a, r, c := assignmentFixture()
	art := &r.Artifacts[0]
	art.ID = "伪造"
	art.Digest = "错误"
	art.ProducerAttemptKey = "其他"
	art.DeliverableID = "其他"
	v, e := ValidateTaskResult(a, r, c)
	require.NoError(t, e)
	b := v.Artifacts[0]
	require.NotEqual(t, art.ID, b.ID)
	require.NotEmpty(t, b.Digest)
	require.Equal(t, "doc", b.DeliverableID)
	require.Equal(t, "k", b.ProducerAttemptKey)
	g, _, _ := validationFixture()
	g.Criteria[0].EvidenceRequired = true
	in := ReviewInput{Final: true, Goal: g, Plan: Plan{Tasks: []TaskSpec{a.Task}}, Results: []TaskResult{v}, Accepted: v.Artifacts, SubjectDigest: "final"}
	review := Review{Verdict: "accept", Reason: "已确认", SubjectDigest: "final", Criteria: []CriterionResult{{CriterionID: "g", Status: "pass", Reason: "覆盖交付物", EvidenceIDs: []string{b.ID}}}}
	_, e = ValidateReview(in, review)
	require.NoError(t, e)
	in.Goal.Deliverables = append(in.Goal.Deliverables, Deliverable{ID: "missing"})
	_, e = ValidateReview(in, review)
	require.Error(t, e)
	in.Goal = g
	review.Criteria[0].EvidenceIDs = nil
	_, e = ValidateReview(in, review)
	require.Error(t, e)
	review.Criteria[0].EvidenceIDs = []string{b.ID}
	in.Accepted = []Artifact{b}
	in.Accepted[0].Content = []byte("41")
	_, e = ValidateReview(in, review)
	require.Error(t, e)
}
func TestRepairCannotRelabelExistingArtifact(t *testing.T) {
	in, review, c := rejectedFixture()
	a, r, _ := assignmentFixture()
	v, e := ValidateTaskResult(a, r, c)
	require.NoError(t, e)
	in.Results = []TaskResult{v}
	_, e = ValidateReview(in, review)
	require.Error(t, e)
	review.Issues[0].ArtifactID = v.Artifacts[0].ID
	_, e = ValidateReview(in, review)
	require.NoError(t, e)
}
func TestArtifactDigestCoversSourcesAndMediaType(t *testing.T) {
	a, r, c := assignmentFixture()
	v, e := ValidateTaskResult(a, r, c)
	require.NoError(t, e)
	art := v.Artifacts[0]
	ref := EvidenceRef{SourceID: art.ID, SourceDigest: art.Digest, LocatorKind: "json_pointer"}
	ref.QuoteDigest, _ = SourceDigest(art.Content)
	require.NoError(t, ValidateEvidence(ref, nil, []Artifact{art}))
	altered := art
	altered.MediaType = "text/plain"
	require.Error(t, ValidateEvidence(ref, nil, []Artifact{altered}))
	altered = art
	altered.Sources = []EvidenceRef{ref}
	require.Error(t, ValidateEvidence(ref, nil, []Artifact{altered}))
}
