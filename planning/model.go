package planning

import (
	"encoding/json"
	"slices"
)

// Constraint records a requirement on how the goal must be pursued.
type Constraint struct {
	ID   string `json:"id"`
	Rule string `json:"rule"`
}

// Criterion defines an acceptance condition and whether it requires evidence.
type Criterion struct {
	ID               string `json:"id"`
	Description      string `json:"description"`
	EvidenceRequired bool   `json:"evidence_required"`
}

// Deliverable specifies an expected goal output.
type Deliverable struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	MediaType   string `json:"media_type"`
}

// Scope records the explicitly included and excluded work.
type Scope struct {
	Included []string `json:"included"`
	Excluded []string `json:"excluded"`
}

// GoalParameter records a confirmed parameter and its origin.
type GoalParameter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Origin is user, host, or inferred.
	Origin string `json:"origin"`
}

// Assumption records an assumption and its host confirmation status.
type Assumption struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
	Confirmed bool   `json:"confirmed"`
}

// SourceInput contains host-provided, bounded JSON source material and its digest.
type SourceInput struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	MediaType string          `json:"media_type"`
	Origin    string          `json:"origin"`
	Digest    string          `json:"digest"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// OutputProfile sets the language, format, and audience of the output.
type OutputProfile struct {
	Language string `json:"language"`
	Format   string `json:"format"`
	Audience string `json:"audience"`
}

// GoalSpec is the structured contract for a planning run.
type GoalSpec struct {
	ID           string          `json:"id"`
	Objective    string          `json:"objective"`
	Revision     int             `json:"revision"`
	Constraints  []Constraint    `json:"constraints"`
	Deliverables []Deliverable   `json:"deliverables"`
	Criteria     []Criterion     `json:"criteria"`
	Scope        Scope           `json:"scope"`
	Inputs       []SourceInput   `json:"inputs"`
	Parameters   []GoalParameter `json:"parameters"`
	Assumptions  []Assumption    `json:"assumptions"`
	Output       OutputProfile   `json:"output"`
}

// OutputSpec declares a task output and its size bound.
type OutputSpec struct {
	Name          string `json:"name"`
	MediaType     string `json:"media_type"`
	DeliverableID string `json:"deliverable_id"`
	MaxBytes      int    `json:"max_bytes"`
}

// InputRef identifies an output by dependency task and output name.
type InputRef struct {
	DependencyID string `json:"dependency_id"`
	OutputName   string `json:"output_name"`
}

// ResourceClaim declares read or write access to a shared resource.
type ResourceClaim struct {
	Key string `json:"key"`
	// Mode is read or write.
	Mode string `json:"mode"`
}

// TaskSpec describes one task and its execution and acceptance requirements.
type TaskSpec struct {
	ID          string       `json:"id"`
	Instruction string       `json:"instruction"`
	MemberID    string       `json:"member_id"`
	Generation  int          `json:"generation"`
	Priority    int          `json:"priority"`
	DependsOn   []string     `json:"depends_on"`
	Inputs      []InputRef   `json:"inputs"`
	Outputs     []OutputSpec `json:"outputs"`
	// Criteria define acceptance conditions local to this task.
	Criteria []Criterion `json:"criteria"`
	// CriterionIDs refer to goal criteria to which the task contributes.
	CriterionIDs         []string `json:"criterion_ids"`
	GroupID              string   `json:"group_id"`
	RequiredCapabilities []string `json:"required_capabilities"`
	GoalInputIDs         []string `json:"goal_input_ids"`
}

// TaskGroup describes a non-executing group in the plan.
type TaskGroup struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Title    string `json:"title"`
}

// PlanDraft contains a proposed task graph and its rationale.
type PlanDraft struct {
	Tasks     []TaskSpec  `json:"tasks"`
	Groups    []TaskGroup `json:"groups"`
	Rationale string      `json:"rationale"`
}

// Plan is a committed task graph for a particular goal and revision.
type Plan struct {
	Revision       int         `json:"revision"`
	ParentRevision int         `json:"parent_revision"`
	GoalDigest     string      `json:"goal_digest"`
	Tasks          []TaskSpec  `json:"tasks"`
	Groups         []TaskGroup `json:"groups"`
}

// EvidenceRef locates evidence within a source or input artifact.
type EvidenceRef struct {
	SourceID     string `json:"source_id"`
	SourceDigest string `json:"source_digest"`
	LocatorKind  string `json:"locator_kind"`
	Locator      string `json:"locator"`
	QuoteDigest  string `json:"quote_digest"`
}

// Artifact is a task output with inline JSON content and source references.
type Artifact struct {
	ID                 string          `json:"id"`
	ProducerAttemptKey string          `json:"producer_attempt_key"`
	Name               string          `json:"name"`
	MediaType          string          `json:"media_type"`
	DeliverableID      string          `json:"deliverable_id"`
	Digest             string          `json:"digest"`
	Content            json.RawMessage `json:"content,omitempty"`
	Sources            []EvidenceRef   `json:"sources"`
}

// ResolvedInput pairs an input reference with the frozen artifact it identifies.
type ResolvedInput struct {
	Ref      InputRef `json:"ref"`
	Artifact Artifact `json:"artifact"`
}

// CriterionResult records a pass, fail, or unknown assessment with evidence.
type CriterionResult struct {
	CriterionID string   `json:"criterion_id"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	EvidenceIDs []string `json:"evidence_ids"`
}

// Review records a verdict for a frozen subject and any required repair.
type Review struct {
	ID            string `json:"id"`
	SubjectDigest string `json:"subject_digest"`
	// Verdict is accept, retry, replan, or block.
	Verdict  string            `json:"verdict"`
	Reason   string            `json:"reason"`
	Criteria []CriterionResult `json:"criteria"`
	Issues   []Issue           `json:"issues"`
	Repair   *RepairDecision   `json:"repair"`
}

// Issue identifies a finding tied to a task, artifact, or criterion.
type Issue struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Severity    string   `json:"severity"`
	TaskID      string   `json:"task_id"`
	AttemptKey  string   `json:"attempt_key"`
	ArtifactID  string   `json:"artifact_id"`
	CriterionID string   `json:"criterion_id"`
	Location    string   `json:"location"`
	Reason      string   `json:"reason"`
	RelatedIDs  []string `json:"related_ids"`
}

// RepairDecision links a repair action to the issues and tasks it addresses.
type RepairDecision struct {
	Action        string   `json:"action"`
	IssueIDs      []string `json:"issue_ids"`
	TargetTaskIDs []string `json:"target_task_ids"`
}

// TaskResult records the business outcome and outputs of one execution.
type TaskResult struct {
	TaskID     string `json:"task_id"`
	AttemptKey string `json:"attempt_key"`
	// Outcome is produced or rejected.
	Outcome     string     `json:"outcome"`
	FailureKind string     `json:"failure_kind"`
	Reason      string     `json:"reason"`
	Artifacts   []Artifact `json:"artifacts"`
}

// Attempt records the identity, execution result, and review of a task attempt.
type Attempt struct {
	TaskID string `json:"task_id"`
	// ExecutionKey identifies the frozen execution of this attempt.
	ExecutionKey string      `json:"execution_key"`
	Fingerprint  string      `json:"fingerprint"`
	Status       string      `json:"status"`
	Generation   int         `json:"generation"`
	Number       int         `json:"number"`
	Result       *TaskResult `json:"result"`
	Review       *Review     `json:"review"`
}

// GoalContext carries the goal requirements shared with an executor.
type GoalContext struct {
	Objective   string       `json:"objective"`
	Constraints []Constraint `json:"constraints"`
	// Scope preserves the confirmed boundary of the task.
	Scope        Scope         `json:"scope"`
	Assumptions  []Assumption  `json:"assumptions"`
	Criteria     []Criterion   `json:"criteria"`
	Deliverables []Deliverable `json:"deliverables"`
}

// Assignment freezes the execution contract and inputs for one attempt.
type Assignment struct {
	Slot           int             `json:"slot"`
	PlanRevision   int             `json:"plan_revision"`
	Task           TaskSpec        `json:"task"`
	GoalDigest     string          `json:"goal_digest"`
	GoalContext    GoalContext     `json:"goal_context"`
	AttemptKey     string          `json:"attempt_key"`
	Fingerprint    string          `json:"fingerprint"`
	MemberVersion  string          `json:"member_version"`
	ManifestDigest string          `json:"manifest_digest"`
	Inputs         []ResolvedInput `json:"inputs"`
	GoalInputs     []SourceInput   `json:"goal_inputs"`
	Parameters     []GoalParameter `json:"parameters"`
	Output         OutputProfile   `json:"output"`
	Claims         []ResourceClaim `json:"claims"`
}

// Wave contains the assignments committed together for execution.
type Wave struct {
	Number       int          `json:"number"`
	PlanRevision int          `json:"plan_revision"`
	Assignments  []Assignment `json:"assignments"`
}

// FrozenOutput records the digest of an output protected by a write freeze.
type FrozenOutput struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// WriteFreeze preserves the committed write contract and known output digests.
type WriteFreeze struct {
	ContractDigest string         `json:"contract_digest"`
	KnownOutputs   []FrozenOutput `json:"known_outputs"`
}

// Outcome records the business status and resulting artifacts of a planning run.
type Outcome struct {
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	ArtifactIDs []string `json:"artifact_ids"`
}

// State is the JSON checkpoint owned by the planning graph.
type State struct {
	Request             *GoalRequest    `json:"request"`
	Resolution          *GoalResolution `json:"resolution"`
	GoalAnswers         []GoalAnswer    `json:"goal_answers"`
	GoalRounds          int             `json:"goal_rounds"`
	GoalResolutionError string          `json:"goal_resolution_error"`
	Goal                GoalSpec        `json:"goal"`
	GoalDigest          string          `json:"goal_digest"`
	Candidate           *PlanDraft      `json:"candidate"`
	Plan                *Plan           `json:"plan"`
	Attempts            []Attempt       `json:"attempts"`
	Accepted            []Artifact      `json:"accepted"`
	Feedback            []Review        `json:"feedback"`
	Wave                *Wave           `json:"wave"`
	WaveResults         []TaskResult    `json:"wave_results"`
	WriteFreeze         *WriteFreeze    `json:"write_freeze"`
	Replans             int             `json:"replans"`
	Waves               int             `json:"waves"`
	NextTaskID          int             `json:"next_task_id"`
	PendingAction       string          `json:"pending_action"`
	ValidationError     string          `json:"validation_error"`
	Outcome             *Outcome        `json:"outcome"`
}

// GoalRequest contains natural-language input, trusted sources, and optional initial goal fields.
type GoalRequest struct {
	Text    string        `json:"text"`
	Inputs  []SourceInput `json:"inputs"`
	Initial *GoalSpec     `json:"initial"`
}

// GoalQuestion requests clarification of one candidate goal field.
type GoalQuestion struct {
	ID      string   `json:"id"`
	Field   string   `json:"field"`
	Prompt  string   `json:"prompt"`
	Options []string `json:"options"`
}

// GoalAnswer records the response and trusted materials supplied for a question.
type GoalAnswer struct {
	QuestionID string        `json:"question_id"`
	Value      string        `json:"value"`
	Inputs     []SourceInput `json:"inputs"`
}

// GoalResolution contains a candidate goal and any questions that remain open.
type GoalResolution struct {
	Candidate GoalSpec       `json:"candidate"`
	Questions []GoalQuestion `json:"questions"`
}

func (value Scope) clone() Scope {
	value.Included = slices.Clone(value.Included)
	value.Excluded = slices.Clone(value.Excluded)
	return value
}

func (value SourceInput) clone() SourceInput {
	value.Content = slices.Clone(value.Content)
	return value
}

func (value GoalSpec) clone() GoalSpec {
	value.Constraints = slices.Clone(value.Constraints)
	value.Deliverables = slices.Clone(value.Deliverables)
	value.Criteria = slices.Clone(value.Criteria)
	value.Scope = value.Scope.clone()
	value.Inputs = cloneSlice(value.Inputs, SourceInput.clone)
	value.Parameters = slices.Clone(value.Parameters)
	value.Assumptions = slices.Clone(value.Assumptions)
	return value
}

func (value TaskSpec) clone() TaskSpec {
	value.DependsOn = slices.Clone(value.DependsOn)
	value.Inputs = slices.Clone(value.Inputs)
	value.Outputs = slices.Clone(value.Outputs)
	value.Criteria = slices.Clone(value.Criteria)
	value.CriterionIDs = slices.Clone(value.CriterionIDs)
	value.RequiredCapabilities = slices.Clone(value.RequiredCapabilities)
	value.GoalInputIDs = slices.Clone(value.GoalInputIDs)
	return value
}

func (value PlanDraft) clone() PlanDraft {
	value.Tasks = cloneSlice(value.Tasks, TaskSpec.clone)
	value.Groups = slices.Clone(value.Groups)
	return value
}

func (value Plan) clone() Plan {
	value.Tasks = cloneSlice(value.Tasks, TaskSpec.clone)
	value.Groups = slices.Clone(value.Groups)
	return value
}

func (value Artifact) clone() Artifact {
	value.Content = slices.Clone(value.Content)
	value.Sources = slices.Clone(value.Sources)
	return value
}

func (value ResolvedInput) clone() ResolvedInput {
	value.Artifact = value.Artifact.clone()
	return value
}

func (value CriterionResult) clone() CriterionResult {
	value.EvidenceIDs = slices.Clone(value.EvidenceIDs)
	return value
}

func (value Review) clone() Review {
	value.Criteria = cloneSlice(value.Criteria, CriterionResult.clone)
	value.Issues = cloneSlice(value.Issues, Issue.clone)
	value.Repair = clonePointer(value.Repair, RepairDecision.clone)
	return value
}

func (value Issue) clone() Issue {
	value.RelatedIDs = slices.Clone(value.RelatedIDs)
	return value
}

func (value RepairDecision) clone() RepairDecision {
	value.IssueIDs = slices.Clone(value.IssueIDs)
	value.TargetTaskIDs = slices.Clone(value.TargetTaskIDs)
	return value
}

func (value TaskResult) clone() TaskResult {
	value.Artifacts = cloneSlice(value.Artifacts, Artifact.clone)
	return value
}

func (value Attempt) clone() Attempt {
	value.Result = clonePointer(value.Result, TaskResult.clone)
	value.Review = clonePointer(value.Review, Review.clone)
	return value
}

func (value GoalContext) clone() GoalContext {
	value.Constraints = slices.Clone(value.Constraints)
	value.Scope = value.Scope.clone()
	value.Assumptions = slices.Clone(value.Assumptions)
	value.Criteria = slices.Clone(value.Criteria)
	value.Deliverables = slices.Clone(value.Deliverables)
	return value
}

func (value Assignment) clone() Assignment {
	value.Task = value.Task.clone()
	value.GoalContext = value.GoalContext.clone()
	value.Inputs = cloneSlice(value.Inputs, ResolvedInput.clone)
	value.GoalInputs = cloneSlice(value.GoalInputs, SourceInput.clone)
	value.Parameters = slices.Clone(value.Parameters)
	value.Claims = slices.Clone(value.Claims)
	return value
}

func (value Wave) clone() Wave {
	value.Assignments = cloneSlice(value.Assignments, Assignment.clone)
	return value
}

func (value WriteFreeze) clone() WriteFreeze {
	value.KnownOutputs = slices.Clone(value.KnownOutputs)
	return value
}

func (value Outcome) clone() Outcome {
	value.ArtifactIDs = slices.Clone(value.ArtifactIDs)
	return value
}

// Clone returns an independent checkpoint and preserves nil and empty collections.
func (value State) Clone() State {
	value.Request = clonePointer(value.Request, GoalRequest.clone)
	value.Resolution = clonePointer(value.Resolution, GoalResolution.clone)
	value.GoalAnswers = cloneSlice(value.GoalAnswers, GoalAnswer.clone)
	value.Goal = value.Goal.clone()
	value.Candidate = clonePointer(value.Candidate, PlanDraft.clone)
	value.Plan = clonePointer(value.Plan, Plan.clone)
	value.Attempts = cloneSlice(value.Attempts, Attempt.clone)
	value.Accepted = cloneSlice(value.Accepted, Artifact.clone)
	value.Feedback = cloneSlice(value.Feedback, Review.clone)
	value.Wave = clonePointer(value.Wave, Wave.clone)
	value.WaveResults = cloneSlice(value.WaveResults, TaskResult.clone)
	value.WriteFreeze = clonePointer(value.WriteFreeze, WriteFreeze.clone)
	value.Outcome = clonePointer(value.Outcome, Outcome.clone)
	return value
}

func (value GoalRequest) clone() GoalRequest {
	value.Inputs = cloneSlice(value.Inputs, SourceInput.clone)
	value.Initial = clonePointer(value.Initial, GoalSpec.clone)
	return value
}

func (value GoalQuestion) clone() GoalQuestion {
	value.Options = slices.Clone(value.Options)
	return value
}

func (value GoalAnswer) clone() GoalAnswer {
	value.Inputs = cloneSlice(value.Inputs, SourceInput.clone)
	return value
}

func (value GoalResolution) clone() GoalResolution {
	value.Candidate = value.Candidate.clone()
	value.Questions = cloneSlice(value.Questions, GoalQuestion.clone)
	return value
}

func cloneSlice[T any](items []T, clone func(T) T) []T {
	copied := slices.Clone(items)
	for i := range copied {
		copied[i] = clone(copied[i])
	}
	return copied
}

func clonePointer[T any](item *T, clone func(T) T) *T {
	if item == nil {
		return nil
	}
	copied := clone(*item)
	return &copied
}
