package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/core/durable"
)

const mediaJSON = "application/json"

type offlineScenario string

func scenario(name string) (p.State, p.Config, p.Dependencies, error) {
	kind := offlineScenario(name)
	switch kind {
	case "dag", "replan", "wait", "sources":
	default:
		return p.State{}, p.Config{}, p.Dependencies{}, fmt.Errorf("unknown scenario %q", name)
	}
	cfg := p.Config{
		Version: "offline-" + name + "-v1", Parallelism: 1,
		MaxTasks: 8, MaxWaves: 8, MaxReplans: 2, MaxTaskAttempts: 2, MaxGoalRounds: 1,
		MaxArtifactBytes: 1024, MaxArtifactTotalBytes: 8192, MaxStateBytes: 128 * 1024,
		Members: []p.Member{{ID: "local", Version: "v1", ManifestDigest: "offline-read-only-v1", EffectClass: "read_only"}},
	}
	criterion := p.Criterion{ID: "complete", Description: "Verify the result against its declared inputs", EvidenceRequired: true}
	goal := p.GoalSpec{
		ID: name, Revision: 1, Objective: "Produce a checked local report",
		Scope:        p.Scope{Included: []string{"embedded synthetic materials and local confirmation"}},
		Output:       p.OutputProfile{Language: "en", Format: "json"},
		Criteria:     []p.Criterion{criterion},
		Deliverables: []p.Deliverable{{ID: "report", Description: "Checked report", MediaType: mediaJSON}},
	}
	var materials []p.SourceInput
	switch kind {
	case "sources", "replan":
		cfg.Parallelism = 2
		materials = []p.SourceInput{
			{ID: "north", Content: json.RawMessage(`{"station":"north","count":5}`)},
			{ID: "south", Content: json.RawMessage(`{"station":"south","count":7}`)},
		}
		if kind == "replan" {
			materials = append(materials, p.SourceInput{ID: "north-corrected", Content: json.RawMessage(`{"station":"north","count":8}`)})
		}
	case "dag":
		materials = []p.SourceInput{{ID: "north", Content: json.RawMessage(`{"station":"north","count":5}`)}}
	}
	for _, source := range materials {
		digest, err := p.SourceDigest(source.Content)
		if err != nil {
			return p.State{}, p.Config{}, p.Dependencies{}, err
		}
		source.Title, source.Origin, source.MediaType, source.Digest = source.ID, "embedded:station-counts", mediaJSON, digest
		goal.Inputs = append(goal.Inputs, source)
	}
	deps := p.Dependencies{Planner: kind, Executor: kind, Reviewer: kind, PlannerVersion: "offline-plan-v1", ReviewerVersion: "offline-review-v1"}
	return p.State{Goal: goal}, cfg, deps, nil
}

func demoTask(output, instruction string) p.TaskSpec {
	return p.TaskSpec{
		ID: "new:" + output, MemberID: "local", Instruction: instruction,
		Criteria:     []p.Criterion{{ID: "checked", Description: "Verify the output value", EvidenceRequired: true}},
		CriterionIDs: []string{"complete"},
		Outputs:      []p.OutputSpec{{Name: output, MediaType: mediaJSON, MaxBytes: 1024}},
	}
}

func (kind offlineScenario) Plan(_ context.Context, in p.PlanningInput) (p.PlanDraft, error) {
	if in.Current != nil {
		tasks := slices.Clone(in.Current.Tasks)
		if kind == "replan" && in.Current.Revision == 1 && len(in.Feedback) > 0 {
			repair := in.Feedback[len(in.Feedback)-1].Repair
			for i := range tasks {
				if repair != nil && slices.Contains(repair.TargetTaskIDs, tasks[i].ID) && tasks[i].Outputs[0].Name == "north" {
					tasks[i].GoalInputIDs = []string{"north-corrected"}
					return p.PlanDraft{Tasks: tasks, Rationale: "Read the corrected north material and rebuild its dependent report"}, nil
				}
			}
		}
		return p.PlanDraft{}, &p.CandidateError{Reason: "no further offline repair"}
	}
	report := demoTask("report", "Total the accepted station counts")
	report.Outputs[0].DeliverableID = "report"
	if kind == "wait" {
		report.Instruction = "Wait for confirmation, then record it"
		return p.PlanDraft{Tasks: []p.TaskSpec{report}}, nil
	}
	stations := []string{"north"}
	if kind != "dag" {
		stations = append(stations, "south")
	}
	var tasks []p.TaskSpec
	for _, station := range stations {
		extract := demoTask(station, "Extract the "+station+" station count")
		extract.GoalInputIDs = []string{station}
		report.DependsOn = append(report.DependsOn, extract.ID)
		report.Inputs = append(report.Inputs, p.InputRef{DependencyID: extract.ID, OutputName: station})
		tasks = append(tasks, extract)
	}
	return p.PlanDraft{Tasks: append(tasks, report)}, nil
}

type stationCount struct {
	Count int `json:"count"`
}

type stationTotal struct {
	Total int `json:"total"`
}

func count(raw json.RawMessage) (int, error) {
	var data struct {
		Count *int `json:"count"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return 0, err
	}
	if data.Count == nil || *data.Count < 0 {
		return 0, fmt.Errorf("expected a nonnegative station count")
	}
	return *data.Count, nil
}

func atCount(id string) p.EvidenceRef {
	return p.EvidenceRef{SourceID: id, LocatorKind: "json_pointer", Locator: "/count"}
}

func (kind offlineScenario) Execute(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
	output := a.Task.Outputs[0]
	artifact := p.Artifact{Name: output.Name, MediaType: output.MediaType, DeliverableID: output.DeliverableID}
	switch {
	case kind == "wait":
		info, err := json.Marshal(struct {
			Prompt       string          `json:"prompt"`
			Schema       json.RawMessage `json:"schema"`
			GoalDigest   string          `json:"goal_digest"`
			PlanRevision int             `json:"plan_revision"`
			AttemptKey   string          `json:"attempt_key"`
		}{"Record this local confirmation?", json.RawMessage(`{"type":"boolean","const":true}`), a.GoalDigest, a.PlanRevision, a.AttemptKey})
		if err != nil {
			return p.TaskResult{}, err
		}
		_, err = durable.AwaitInput(ctx, "confirm-report", info, func(raw json.RawMessage) error {
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("true")) {
				return fmt.Errorf("answer must be the JSON boolean true")
			}
			return nil
		})
		if err != nil {
			return p.TaskResult{}, err
		}
		artifact.Content = json.RawMessage(`{"confirmed":true}`)
	case output.Name == "report":
		total := 0
		for _, input := range a.Inputs {
			n, err := count(input.Artifact.Content)
			if err != nil {
				return p.TaskResult{}, err
			}
			total += n
			artifact.Sources = append(artifact.Sources, atCount(input.Artifact.ID))
		}
		var err error
		artifact.Content, err = json.Marshal(stationTotal{Total: total})
		if err != nil {
			return p.TaskResult{}, err
		}
	default:
		if len(a.GoalInputs) != 1 {
			return p.TaskResult{}, fmt.Errorf("expected one frozen station source")
		}
		input := a.GoalInputs[0]
		n, err := count(input.Content)
		if err != nil {
			return p.TaskResult{}, err
		}
		artifact.Content, err = json.Marshal(stationCount{Count: n})
		if err != nil {
			return p.TaskResult{}, err
		}
		artifact.Sources = []p.EvidenceRef{atCount(input.ID)}
	}
	return p.TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "produced", Artifacts: []p.Artifact{artifact}}, nil
}

func (kind offlineScenario) Review(_ context.Context, in p.ReviewInput) (p.Review, error) {
	review := p.Review{SubjectDigest: in.SubjectDigest, Verdict: "accept", Reason: "local output checks passed"}
	if !in.Final {
		artifact := in.Results[0].Artifacts[0]
		if err := checkOutput(kind, *in.Assignment, artifact); err != nil {
			return p.Review{}, &p.CandidateError{Reason: err.Error()}
		}
		review.Criteria = []p.CriterionResult{{CriterionID: "checked", Status: "pass", EvidenceIDs: []string{artifact.ID}}}
		return review, nil
	}
	for _, result := range in.Results {
		artifact := result.Artifacts[0]
		if kind == "replan" && artifact.Name == "north" && artifact.Sources[0].SourceID == "north" {
			return requestCorrection(in, result, artifact), nil
		}
		if artifact.DeliverableID == "report" {
			review.Criteria = []p.CriterionResult{{CriterionID: "complete", Status: "pass", EvidenceIDs: []string{artifact.ID}}}
		}
	}
	return review, nil
}

func checkOutput(kind offlineScenario, a p.Assignment, artifact p.Artifact) error {
	switch {
	case kind == "wait":
		var v struct {
			Confirmed bool `json:"confirmed"`
		}
		if err := json.Unmarshal(artifact.Content, &v); err != nil || !v.Confirmed {
			return fmt.Errorf("confirmation missing")
		}
	case artifact.Name == "report":
		expected := 2
		if kind == "dag" {
			expected = 1
		}
		if len(a.Inputs) != expected || len(artifact.Sources) != expected {
			return fmt.Errorf("report must cite every required accepted input")
		}
		var result struct {
			Total *int `json:"total"`
		}
		if err := json.Unmarshal(artifact.Content, &result); err != nil || result.Total == nil {
			return fmt.Errorf("report total missing")
		}
		var sum int
		for _, input := range a.Inputs {
			n, err := count(input.Artifact.Content)
			if err != nil || !slices.ContainsFunc(artifact.Sources, func(ref p.EvidenceRef) bool {
				return ref.SourceID == input.Artifact.ID && ref.LocatorKind == "json_pointer" && ref.Locator == "/count"
			}) {
				return fmt.Errorf("report is missing a cited input count")
			}
			sum += n
		}
		if *result.Total != sum {
			return fmt.Errorf("report total differs from cited counts")
		}
	default:
		if len(a.GoalInputs) != 1 || len(artifact.Sources) != 1 {
			return fmt.Errorf("extraction must cite its one frozen station source")
		}
		source, ref := a.GoalInputs[0], artifact.Sources[0]
		actual, actualErr := count(artifact.Content)
		expected, expectedErr := count(source.Content)
		if actualErr != nil || expectedErr != nil || actual != expected || ref.SourceID != source.ID ||
			ref.LocatorKind != "json_pointer" || ref.Locator != "/count" {
			return fmt.Errorf("extraction differs from its cited count")
		}
	}
	return nil
}

func requestCorrection(in p.ReviewInput, result p.TaskResult, north p.Artifact) p.Review {
	const reason = "The final report must use the corrected north station material"
	return p.Review{
		SubjectDigest: in.SubjectDigest, Verdict: "replan", Reason: reason,
		Criteria: []p.CriterionResult{{CriterionID: "complete", Status: "fail", Reason: reason}},
		Issues: []p.Issue{{
			ID: "north-correction", Category: "value_mismatch", Severity: "error", Reason: reason,
			TaskID: result.TaskID, AttemptKey: result.AttemptKey, ArtifactID: north.ID, Location: "/count", CriterionID: "complete",
		}},
		Repair: &p.RepairDecision{Action: "replan", IssueIDs: []string{"north-correction"}, TargetTaskIDs: []string{result.TaskID}},
	}
}
