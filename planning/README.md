# Durable planning

[简体中文](README_CN.md) · [Offline examples](../examples/planning-agent/README.md)

`planning` is an experimental host-controlled planning loop on GoAgent's existing durable graph. A fixed graph resolves the goal, validates a candidate DAG, allocates frozen assignments, executes ready tasks, reviews results and checks the final goal. It does not add a second scheduler or grant permissions from a model-generated role name.

## Host integration

1. Create a trusted `Config.Members` directory. Each member needs a stable ID, semantic version, manifest digest, effect class and resource claims. The host, not the planner, supplies these values.
2. Implement `Planner`, `Executor` and `Reviewer`, and optionally `GoalResolver`. Supply component versions in `Dependencies`. `NewAgentGoalResolver`, `NewAgentPlanner` and `NewAgentReviewer` adapt independently journaled builder agents. Factories must provide fresh owned sessions, stable agent identities/scopes and read-only tools for planning/review. These are host obligations.
3. Call `NormalizeConfig` and `NewGraph`. Obtain `DurableConfig` with the same durable store. It pins the state schema, component versions, member directory and host policy. Pin the parent preset assembly separately when using its `PlanningExecutor`; a child's manifest does not cover the entire parent.
4. Bind an `execution.Scope` with a stable run ID, journal and durable root budget. Planner, execution and review share that root. Reconstructing the host must not reset it.
5. Before the first `Run`, call `PrepareInitialState`: the underlying graph stores its initial checkpoint before normalization. This prevents oversized uncommitted input from entering that checkpoint. Existing authoritative history is preserved on capacity errors.
6. On a wait, persist/return the leaf interrupt's key and schema. Reopen the same store and resume that key with validated input. Do not answer the parent graph key, allocate a replacement attempt or clear unknown-effect records.

Use `State.Goal` for an already authorized structured goal, or `State.Request` with a resolver for natural-language input. Required parameters, clarification rounds and candidate confirmation precede execution. Host-provided source material remains frozen in `Goal.Inputs`; the model cannot mint an authorized source.

## Contracts and limits

`Parallelism` defaults to one. Opt into several slots explicitly and keep their count fixed throughout a run. The default bounds are 64 current tasks, 256 waves, 3 replans, 3 attempts per task, 4 goal-resolution rounds, 64 KiB per artifact, 1 MiB summed artifact content per TaskResult and 8 MiB for the entire serialized State, including history and repeated references. Goal.Inputs source bodies are separately bounded by the same MaxArtifactTotalBytes value. For these planning fields zero selects a default; negative values are rejected. Output-specific byte caps are enforced in addition to global caps. These limits do not replace the shared model/token budget.

Only accepted upstream artifacts satisfy dependencies. Replanning reuses accepted output only when its semantic fingerprint is unchanged; a changed input invalidates dependent output transitively. A waiting attempt keeps its business identity. Write tasks freeze their contract and ancestry; uncertain or already performed effects cannot be bypassed by changing a task ID or generation.

The executor receives the full frozen `GoalContext` and only selected `GoalInputs` plus verified upstream artifacts. Results are untrusted candidates. The host validates identity, size, output shape and sources, assigns artifact identities and digests, and validates task/final reviews. A JSON-pointer evidence locator must resolve inside the assignment's authorized source or artifact. The host fills omitted digests; incorrect nonempty digests are rejected. Valid references establish traceability, while factual correctness still requires review and independent evaluation.

`Progress(State)` is a display projection in plan order. It can show several running tasks. It is not an input to scheduling, approval or recovery.

## Preset members and effects

`planning-members` uses `Host.PlanningMembers` and `Host.PlanningAgentFactory`; `Assembly.PlanningExecutor()` exposes the executor. `PlanningBuilderFactory` preserves the journal and leaves interrupted model resend disabled. Members inherit the parent's pre-hooks, guards, current permissions, sandbox/network ceiling and environment restrictions.

This release requires host-owned persistent native workspaces for workspace members, rejects nested subagents, and rejects `PreSubstitute` in planning members before approval or execution. Ordinary non-planning subagents keep their existing behavior. Unknown tools return an error. Give the planner an already registered authorized tool rather than relying on runtime substitution.

Approvals bind the goal digest, plan revision, attempt and exact action. Permission revocation is checked before a new write. Undeclared non-idempotent writes use one physical attempt; a lost receipt requires business-key reconciliation or host attention. Model retry and external-effect retry are different contracts. A custom factory must not silently enable interrupted model resend or retry writes without a durable policy.

## Memory and recovery

Automatic recall is revocable context, not an authorized business source. Do not copy it into `Goal.Inputs` or task instructions to bypass deletion. Forget removes eligible recall from subsequent physical model requests; it does not erase business inputs, artifacts, approvals or budgets already recorded by the host. Apply the host's data-retention policy to those stores separately.

Restore the same graph/state versions, configuration, member manifests and budget roots. Changed manifests or incompatible checkpoints fail recovery. Back up stores and deletion logs before upgrades; memory `/4` cannot be downgraded by replacing only the binary. Existing legacy plain-chat turns preserve their original request intent.

The examples run without a model by default. Their deterministic reviewers demonstrate control contracts and are not production quality judges. The evaluation CLI preregisters direct/single-slot/four-slot trials with equal limits and keeps failures in the denominator. Only an explicitly configured live backend makes model calls; offline passes do not establish better answers, lower costs or faster execution.
