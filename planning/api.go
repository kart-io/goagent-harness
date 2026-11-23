package planning

import (
	"context"

	"github.com/kart-io/goagent/multiagent"
)

// PlanningInput provides the committed context and candidate feedback to a Planner.
type PlanningInput struct {
	Goal            GoalSpec   `json:"goal"`
	Current         *Plan      `json:"current"`
	RejectedDraft   *PlanDraft `json:"rejected_draft"`
	RejectionReason string     `json:"rejection_reason"`
	Accepted        []Artifact `json:"accepted"`
	Feedback        []Review   `json:"feedback"`
	Members         []Member   `json:"members"`
}

// ReviewInput provides the frozen task or final goal subject to a Reviewer.
type ReviewInput struct {
	SubjectDigest string `json:"subject_digest"`
	// Assignment is present for task review and nil for final review.
	Assignment *Assignment  `json:"assignment"`
	Goal       GoalSpec     `json:"goal"`
	Plan       Plan         `json:"plan"`
	Tasks      []TaskSpec   `json:"tasks"`
	Results    []TaskResult `json:"results"`
	Accepted   []Artifact   `json:"accepted"`
	Final      bool         `json:"final"`
}

// Member describes a trusted member and its permitted resource access.
type Member struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	ManifestDigest string `json:"manifest_digest"`
	// EffectClass is read_only or write.
	EffectClass  string          `json:"effect_class"`
	Claims       []ResourceClaim `json:"claims"`
	Role         string          `json:"role"`
	Description  string          `json:"description"`
	Capabilities []string        `json:"capabilities"`
}

// Config sets planning bounds and the trusted member directory.
type Config struct {
	Version                string   `json:"version"`
	Parallelism            int      `json:"parallelism"`
	MaxTasks               int      `json:"max_tasks"`
	MaxWaves               int      `json:"max_waves"`
	MaxReplans             int      `json:"max_replans"`
	MaxTaskAttempts        int      `json:"max_task_attempts"`
	MaxArtifactBytes       int      `json:"max_artifact_bytes"`
	MaxArtifactTotalBytes  int      `json:"max_artifact_total_bytes"`
	MaxStateBytes          int      `json:"max_state_bytes"`
	MaxGoalRounds          int      `json:"max_goal_rounds"`
	RequiredGoalParameters []string `json:"required_goal_parameters"`
	Members                []Member `json:"members"`
}

// GoalResolveInput provides the request and committed clarification context for one round.
type GoalResolveInput struct {
	Request         GoalRequest     `json:"request"`
	Previous        *GoalResolution `json:"previous"`
	Answers         []GoalAnswer    `json:"answers"`
	Round           int             `json:"round"`
	RejectionReason string          `json:"rejection_reason"`
}

// Planner proposes a task graph for the current goal.
type Planner interface {
	Plan(context.Context, PlanningInput) (PlanDraft, error)
}

// Executor performs a frozen task assignment.
type Executor interface {
	Execute(context.Context, Assignment) (TaskResult, error)
}

// Reviewer assesses the supplied task or final goal subject.
type Reviewer interface {
	Review(context.Context, ReviewInput) (Review, error)
}

// GoalResolver proposes a structured goal or asks for clarification.
type GoalResolver interface {
	Resolve(context.Context, GoalResolveInput) (GoalResolution, error)
}

// Dependencies supplies the versioned implementations used by the graph.
type Dependencies struct {
	Planner             Planner
	Executor            Executor
	Reviewer            Reviewer
	PlannerVersion      string
	ReviewerVersion     string
	GoalResolver        GoalResolver
	GoalResolverVersion string
}

// AgentConfig identifies and supplies an agent used by a planning adapter.
type AgentConfig struct {
	Version string
	Factory multiagent.AgentFactory
}

// CandidateError reports rejection of an already recorded model response.
type CandidateError struct {
	Reason string `json:"reason"`
}

// Error returns the candidate rejection reason.
func (e *CandidateError) Error() string {
	return e.Reason
}
