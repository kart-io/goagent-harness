package planning

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
)

func TestPlanningTypesRoundTripAndClone(t *testing.T) {
	source := SourceInput{ID: "source", Title: "Notes", MediaType: "application/json", Origin: "host", Digest: "source-digest", Content: json.RawMessage(`{"page":1}`)}
	goal := GoalSpec{
		ID: "goal", Objective: "summarize the notes", Revision: 1,
		Constraints:  []Constraint{{ID: "limit", Rule: "draft only"}},
		Deliverables: []Deliverable{{ID: "summary", Description: "Summary", MediaType: "text/plain"}},
		Criteria:     []Criterion{{ID: "cite", Description: "Cites the source", EvidenceRequired: true}},
		Scope:        Scope{Included: []string{"notes"}, Excluded: []string{"publication"}},
		Inputs:       []SourceInput{source}, Parameters: []GoalParameter{{Key: "limit", Value: "one page", Origin: "user"}},
		Assumptions: []Assumption{{ID: "context", Statement: "Notes are current", Confirmed: true}},
		Output:      OutputProfile{Language: "en", Format: "text", Audience: "team"},
	}
	ref := InputRef{DependencyID: "t-000001", OutputName: "notes"}
	task := TaskSpec{
		ID: "t-000002", Instruction: "draft", MemberID: "writer", Generation: 1, Priority: 7,
		DependsOn: []string{ref.DependencyID}, Inputs: []InputRef{ref},
		Outputs:  []OutputSpec{{Name: "summary", MediaType: "text/plain", DeliverableID: "summary", MaxBytes: 100}},
		Criteria: []Criterion{{ID: "clear", Description: "Clear wording"}}, CriterionIDs: []string{"cite"},
		GroupID: "draft", RequiredCapabilities: []string{"writing"}, GoalInputIDs: []string{source.ID},
	}
	group := TaskGroup{ID: "draft", ParentID: "work", Title: "Drafting"}
	draft := PlanDraft{Tasks: []TaskSpec{task}, Groups: []TaskGroup{group}, Rationale: "review then summarize"}
	plan := Plan{Revision: 2, ParentRevision: 1, GoalDigest: "goal-digest", Tasks: draft.Tasks, Groups: draft.Groups}
	artifact := Artifact{
		ID: "draft-1", ProducerAttemptKey: "previous", Name: "notes", MediaType: "application/json",
		DeliverableID: "summary", Digest: "output-digest", Content: json.RawMessage(`"notes"`),
		Sources: []EvidenceRef{{SourceID: source.ID, SourceDigest: source.Digest, LocatorKind: "json_pointer", Locator: "/page", QuoteDigest: "quote"}},
	}
	review := Review{
		ID: "review", SubjectDigest: "subject-digest", Verdict: "replan", Reason: "fix citation",
		Criteria: []CriterionResult{{CriterionID: "cite", Status: "fail", Reason: "missing citation", EvidenceIDs: []string{artifact.ID}}},
		Issues:   []Issue{{ID: "i", Category: "evidence", Severity: "major", TaskID: task.ID, AttemptKey: "attempt", ArtifactID: artifact.ID, CriterionID: "cite", Location: "/page", Reason: "omitted", RelatedIDs: []string{"related"}}},
		Repair:   &RepairDecision{Action: "replan", IssueIDs: []string{"i"}, TargetTaskIDs: []string{task.ID}},
	}
	result := TaskResult{TaskID: task.ID, AttemptKey: "attempt", Outcome: "rejected", FailureKind: "evidence", Reason: "incomplete", Artifacts: []Artifact{artifact}}
	assignment := Assignment{
		Slot: 1, PlanRevision: 2, Task: task, GoalDigest: plan.GoalDigest, AttemptKey: result.AttemptKey,
		Fingerprint: "fingerprint", MemberVersion: "v1", ManifestDigest: "manifest",
		GoalContext: GoalContext{Objective: goal.Objective, Constraints: goal.Constraints, Scope: goal.Scope, Assumptions: goal.Assumptions, Criteria: goal.Criteria, Deliverables: goal.Deliverables},
		Inputs:      []ResolvedInput{{Ref: ref, Artifact: artifact}}, GoalInputs: goal.Inputs,
		Parameters: goal.Parameters, Output: goal.Output, Claims: []ResourceClaim{{Key: "notes", Mode: "read"}},
	}
	state := State{
		Request:     &GoalRequest{Text: goal.Objective, Inputs: goal.Inputs, Initial: &goal},
		Resolution:  &GoalResolution{Candidate: goal, Questions: []GoalQuestion{{ID: "q", Field: "format", Prompt: "Which format?", Options: []string{"text"}}}},
		GoalAnswers: []GoalAnswer{{QuestionID: "q", Value: "text", Inputs: goal.Inputs}},
		GoalRounds:  2, GoalResolutionError: "previous clarification", Goal: goal, GoalDigest: plan.GoalDigest,
		Candidate: &draft, Plan: &plan,
		Attempts: []Attempt{{TaskID: task.ID, ExecutionKey: result.AttemptKey, Fingerprint: assignment.Fingerprint, Status: "rejected", Generation: 1, Number: 2, Result: &result, Review: &review}},
		Accepted: []Artifact{artifact}, Feedback: []Review{review},
		Wave: &Wave{Number: 1, PlanRevision: 2, Assignments: []Assignment{assignment}}, WaveResults: []TaskResult{result},
		WriteFreeze: &WriteFreeze{ContractDigest: "contract", KnownOutputs: []FrozenOutput{{TaskID: task.ID, Name: artifact.Name, Digest: artifact.Digest}}},
		Replans:     1, Waves: 1, NextTaskID: 3, PendingAction: "replan", ValidationError: "citation",
		Outcome: &Outcome{Status: "blocked", Reason: "citation", ArtifactIDs: []string{artifact.ID}},
	}

	original := checkPlanningJSON(t, state, "goal_resolution_error", "write_freeze", "wave_results")
	checkPlanningJSON(t, PlanningInput{Goal: goal, Current: &plan, RejectedDraft: &draft, RejectionReason: "citation", Accepted: state.Accepted, Feedback: state.Feedback, Members: []Member{{ID: "writer", Version: "v1", ManifestDigest: "manifest", EffectClass: "read_only", Claims: assignment.Claims, Role: "author", Description: "drafts notes", Capabilities: []string{"writing"}}}}, "rejected_draft", "rejection_reason")
	checkPlanningJSON(t, ReviewInput{SubjectDigest: review.SubjectDigest, Assignment: &assignment, Goal: goal, Plan: plan, Tasks: plan.Tasks, Results: state.WaveResults, Accepted: state.Accepted}, "subject_digest", "assignment")
	checkPlanningJSON(t, GoalResolveInput{Request: *state.Request, Previous: state.Resolution, Answers: state.GoalAnswers, Round: 2, RejectionReason: "format"}, "previous", "rejection_reason")
	checkPlanningJSON(t, Config{Version: "v1", Parallelism: 1, MaxTasks: 64, MaxWaves: 256, MaxReplans: 3, MaxTaskAttempts: 3, MaxArtifactBytes: 65536, MaxArtifactTotalBytes: 1048576, MaxStateBytes: 8388608, MaxGoalRounds: 4, RequiredGoalParameters: []string{"limit"}}, "max_task_attempts", "max_artifact_total_bytes")

	checkPlanningJSON(t, SourceInput{})
	checkPlanningJSON(t, Artifact{Content: json.RawMessage(`null`)}, "content")

	clone := state.Clone()
	if !reflect.DeepEqual(state, clone) {
		t.Fatal("clone differs from its source")
	}
	clone.Request.Initial.Scope.Included[0] = "mutated"
	clone.Request.Inputs[0].Content[2] = 'X'
	clone.Resolution.Candidate.Assumptions[0].Statement = "mutated"
	clone.Resolution.Questions[0].Options[0] = "mutated"
	clone.GoalAnswers[0].Inputs[0].Content[2] = 'X'
	clone.Goal.Constraints[0].Rule = "mutated"
	clone.Candidate.Tasks[0].RequiredCapabilities[0] = "mutated"
	clone.Candidate.Groups[0].Title = "mutated"
	clone.Plan.Tasks[0].Outputs[0].Name = "mutated"
	clone.Attempts[0].Result.Artifacts[0].Content[1] = 'X'
	clone.Attempts[0].Review.Criteria[0].EvidenceIDs[0] = "mutated"
	clone.Attempts[0].Review.Issues[0].RelatedIDs[0] = "mutated"
	clone.Attempts[0].Review.Repair.IssueIDs[0] = "mutated"
	clone.Accepted[0].Sources[0].Locator = "mutated"
	clone.Feedback[0].Repair.TargetTaskIDs[0] = "mutated"
	clone.Wave.Assignments[0].GoalContext.Scope.Excluded[0] = "mutated"
	clone.Wave.Assignments[0].Inputs[0].Artifact.Content[1] = 'X'
	clone.Wave.Assignments[0].GoalInputs[0].Content[2] = 'X'
	clone.Wave.Assignments[0].Parameters[0].Value = "mutated"
	clone.Wave.Assignments[0].Claims[0].Key = "mutated"
	clone.WaveResults[0].Artifacts[0].Sources[0].Locator = "mutated"
	clone.WriteFreeze.KnownOutputs[0].Digest = "mutated"
	clone.Outcome.ArtifactIDs[0] = "mutated"
	if got := checkPlanningJSON(t, state); !bytes.Equal(original, got) {
		t.Fatal("mutating the clone changed the input checkpoint")
	}
	if got := (State{}).Clone(); !reflect.DeepEqual(got, State{}) {
		t.Fatal("clone changed zero-state shape")
	}
	empty := State{Accepted: []Artifact{}, Goal: GoalSpec{Inputs: []SourceInput{}}}
	if got := empty.Clone(); !reflect.DeepEqual(got, empty) {
		t.Fatal("clone changed allocated empty slices to nil")
	}
}

func checkPlanningJSON[T any](t *testing.T, value T, keys ...string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Errorf("missing JSON key %q for %T", key, value)
		}
	}
	var decoded T
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, value) {
		t.Errorf("round trip changed %T", value)
	}
	return data
}

func TestConfigDefaultsAndBounds(t *testing.T) {
	input := Config{Version: "v1", Members: []Member{{
		ID: "writer", Version: "v2", ManifestDigest: "manifest", EffectClass: "write",
		Claims: []ResourceClaim{{Key: "workspace", Mode: "write"}}, Capabilities: []string{"writing"},
	}}, RequiredGoalParameters: []string{"limit"}}
	normalized, err := NormalizeConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	want := input
	want.Parallelism, want.MaxTasks, want.MaxWaves = 1, 64, 256
	want.MaxReplans, want.MaxTaskAttempts, want.MaxGoalRounds = 3, 3, 4
	want.MaxArtifactBytes, want.MaxArtifactTotalBytes, want.MaxStateBytes = 65536, 1048576, 8388608
	if !reflect.DeepEqual(normalized, want) {
		t.Fatalf("normalized = %+v, want %+v", normalized, want)
	}
	if got := normalized.maxSteps(); got != 1064 {
		t.Errorf("maxSteps = %d, want 1064", got)
	}
	normalized.Members[0].Claims[0].Key = "mutated"
	normalized.Members[0].Capabilities[0] = "mutated"
	normalized.RequiredGoalParameters[0] = "mutated"
	if input.Members[0].Claims[0].Key != "workspace" || input.Members[0].Capabilities[0] != "writing" || input.RequiredGoalParameters[0] != "limit" {
		t.Fatal("normalized config shares mutable storage with caller")
	}

	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"parallelism", func(c *Config) { c.Parallelism = -1 }},
		{"tasks", func(c *Config) { c.MaxTasks = -1 }},
		{"waves", func(c *Config) { c.MaxWaves = -1 }},
		{"replans", func(c *Config) { c.MaxReplans = -1 }},
		{"attempts", func(c *Config) { c.MaxTaskAttempts = -1 }},
		{"goal rounds", func(c *Config) { c.MaxGoalRounds = -1 }},
		{"artifact bytes", func(c *Config) { c.MaxArtifactBytes = -1 }},
		{"artifact total", func(c *Config) { c.MaxArtifactTotalBytes = -1 }},
		{"state bytes", func(c *Config) { c.MaxStateBytes = -1 }},
		{"step product overflow", func(c *Config) { c.MaxWaves = math.MaxInt }},
		{"step sum overflow", func(c *Config) { c.MaxWaves = (math.MaxInt - 12) / 4 }},
		{"attempt capacity overflow", func(c *Config) { c.MaxTaskAttempts = math.MaxInt }},
		{"missing version", func(c *Config) { c.Version = " " }},
		{"missing members", func(c *Config) { c.Members = nil }},
		{"missing member id", func(c *Config) { c.Members[0].ID = "" }},
		{"duplicate member", func(c *Config) { c.Members = append(c.Members, c.Members[0]) }},
		{"missing member version", func(c *Config) { c.Members[0].Version = "" }},
		{"missing manifest", func(c *Config) { c.Members[0].ManifestDigest = "" }},
		{"missing effect", func(c *Config) { c.Members[0].EffectClass = "" }},
		{"unknown effect", func(c *Config) { c.Members[0].EffectClass = "unknown" }},
		{"read only member with write claim", func(c *Config) { c.Members[0].EffectClass = "read_only" }},
		{"missing write claim", func(c *Config) { c.Members[0].Claims = nil }},
		{"write member with read-only claims", func(c *Config) { c.Members[0].Claims[0].Mode = "read" }},
		{"missing claim key", func(c *Config) { c.Members[0].Claims[0].Key = " " }},
		{"unknown claim mode", func(c *Config) { c.Members[0].Claims[0].Mode = "append" }},
		{"duplicate resource claim", func(c *Config) {
			c.Members[0].Claims = append(c.Members[0].Claims, ResourceClaim{Key: "workspace", Mode: "read"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := input
			cfg.Members = append([]Member(nil), input.Members...)
			cfg.Members[0].Claims = append([]ResourceClaim(nil), input.Members[0].Claims...)
			tt.change(&cfg)
			if _, err := NormalizeConfig(cfg); err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}
	input.Members = []Member{{ID: "reader", Version: "v1", ManifestDigest: "manifest", EffectClass: "read_only"}}
	input.Parallelism, input.MaxGoalRounds, input.MaxReplans = 2, 1, 1
	input.MaxWaves = (math.MaxInt - 20) / 4
	got, err := NormalizeConfig(input)
	if err != nil || got.Parallelism != 2 || got.maxSteps() != math.MaxInt-3 {
		t.Fatalf("valid boundary config was changed or rejected: %+v, %v", got, err)
	}
}
