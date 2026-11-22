package eval

import (
	"context"
	"fmt"
	"strings"

	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/tools"
)

const plannerPrompt = "Plan read-only tasks with new:<name> IDs and member_id reader. Each task needs instruction, criteria [{id,description,evidence_required:true}], criterion_ids [answer], outputs [{name,media_type:application/json,max_bytes:4096}]. Attach goal_input_ids for the needed sources. Mark the final output deliverable_id answer. Use depends_on and inputs [{dependency_id,output_name}] when consuming earlier outputs."
const reviewerPrompt = "Evaluate only the public goal and frozen evidence. For an accepted subject use verdict accept and criteria [{criterion_id,status:pass,evidence_ids:[artifact ID]}] covering its criteria. Use the supplied subject_digest."

func (t liveTrial) role(role, prompt string) p.AgentConfig {
	return p.AgentConfig{Version: LivePromptVersion, Factory: func(context.Context) (owned tools.OwnedRunnable[any, *builder.AgentOutput], err error) {
		a, err := t.agent(role, prompt)
		if err != nil {
			return owned, err
		}
		owned.Runnable = a.AsRunnable()
		owned.Close = a.Shutdown
		return owned, nil
	}}
}

func (t liveTrial) Execute(ctx context.Context, a p.Assignment) (p.TaskResult, error) {
	const instruction = "Return a JSON object with artifacts, an array containing one object per requested output with name, media_type, deliverable_id, content (JSON value), and sources [{source_id,locator_kind:json_pointer,locator}]. Cite only provided assignment sources or input artifacts. For the answer deliverable, content must meet the public answer format. Output no IDs or digests."
	raw, err := t.ask(ctx, "reader", instruction, a)
	if err != nil {
		return p.TaskResult{}, err
	}
	var candidate struct {
		Artifacts []p.Artifact `json:"artifacts"`
	}
	if err = p.DecodeCandidate([]byte(raw), &candidate); err != nil {
		return p.TaskResult{}, err
	}
	return p.TaskResult{TaskID: a.Task.ID, AttemptKey: a.AttemptKey, Outcome: "produced", Artifacts: candidate.Artifacts}, nil
}

func (t liveTrial) plan(ctx context.Context, store durable.Store, slots int) (string, error) {
	cfg := p.Config{
		Version: LivePromptVersion, Parallelism: slots, MaxTasks: 8, MaxWaves: 8,
		MaxTaskAttempts: 2, MaxReplans: 2, MaxArtifactBytes: 4096,
		Members: []p.Member{{ID: "reader", Version: LivePromptVersion,
			ManifestDigest: "eval-tool-free-reader-v1", EffectClass: "read_only",
			Capabilities: []string{"answer from supplied sources"}}},
	}
	planner, err := p.NewAgentPlanner(t.role("planner", plannerPrompt))
	if err != nil {
		return "", err
	}
	reviewer, err := p.NewAgentReviewer(t.role("reviewer", reviewerPrompt))
	if err != nil {
		return "", err
	}
	deps := p.Dependencies{Planner: planner, Executor: t, Reviewer: reviewer, PlannerVersion: LivePromptVersion, ReviewerVersion: LivePromptVersion}
	goal := p.GoalSpec{
		ID: "question", Revision: 1,
		Objective:    t.input.Request + "\n" + answerContract + " Fields: " + strings.Join(t.input.OutputFields, ", "),
		Scope:        p.Scope{Included: []string{"supplied sources"}},
		Output:       p.OutputProfile{Language: "zh", Format: "json"},
		Criteria:     []p.Criterion{{ID: "answer", Description: "Answer the question using supporting evidence, abstaining when needed", EvidenceRequired: true}},
		Deliverables: []p.Deliverable{{ID: "answer", Description: "Structured answer", MediaType: "application/json"}},
	}
	for _, s := range t.input.Sources {
		digest, err := p.SourceDigest(s.Content)
		if err != nil {
			return "", err
		}
		goal.Inputs = append(goal.Inputs, p.SourceInput{ID: s.ID, Title: s.Title, Origin: "dataset", MediaType: "application/json", Content: s.Content, Digest: digest})
	}
	initial, err := p.PrepareInitialState(p.State{Goal: goal}, cfg)
	if err != nil {
		return "", err
	}
	graph, err := p.NewGraph(cfg, deps)
	if err != nil {
		return "", err
	}
	config, err := p.DurableConfig(cfg, deps, store)
	if err != nil {
		return "", err
	}
	runner, err := graph.CompileDurable(ctx, config)
	if err != nil {
		return "", err
	}
	snap, err := runner.Run(ctx, t.input.RunID, initial, "")
	if err != nil {
		return "", err
	}
	outcome := snap.State.Outcome
	if outcome == nil {
		return "", fmt.Errorf("planning ended without outcome")
	}
	if outcome.Status != "succeeded" {
		return "", fmt.Errorf("planning %s: %s", outcome.Status, outcome.Reason)
	}
	for _, a := range snap.State.Accepted {
		if a.DeliverableID == "answer" {
			return string(a.Content), nil
		}
	}
	return "", fmt.Errorf("missing accepted answer")
}
