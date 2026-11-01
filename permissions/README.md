# Scoped sandbox permissions

`Controller` resolves a trusted default/session policy and optionally approves a
strictly wider file mode for one call. Install `controller.Around()` as the
outermost Pipeline Around wrapper; ordinary `policy.Policy` rules and hard Guards
remain independent and run first. A sandbox approval cannot override a tool deny.

```go
controller, err := permissions.New(permissions.Config{
    AgentID: "coding-worker", // stable host identity, never tool arguments
    Default: sandbox.Policy{Mode: sandbox.ReadOnly, WorkspaceRoot: root},
    Ceiling: sandbox.Policy{Mode: sandbox.WorkspaceWrite, WorkspaceRoot: root},
    Approver: approver,
    Store: sessionStore, // optional host implementation
})
// Handle err before installing the controller.
pipeline.Around = append([]tools.Around{controller.Around()}, pipeline.Around...)
```

Use `DecorateTools` only for a host-selected slice of coding file/command tools
backed by the sandbox adapters. It preserves a tool's dispatch timeout and original
schema. Decorating unknown plugins or MCP tools does not add process isolation.
Escalation arguments are paired:

```json
{"path":"main.go","content":"...","sandbox_permissions":"workspace-write","justification":"Write the requested fix"}
```

The mode must be strictly wider than the current mode and within the configured
ceiling. The call keeps the same directory roots and network restriction. Stable
nonempty CallID and SessionID are required for escalation. Config.AgentID is used
as the approval identity; tool arguments cannot supply identities. Approval is
requested directly from `tools.Approver`, without Pipeline approval coalescing or
session grants. `AllowedSession` is deliberately consumed as one approval only.
Concurrent identical calls each require their own approval. The approval request
contains a copy of the call and a digest, revision and trusted identities in its
reason. Call changes while approval is pending are rejected.

The execution context carries a copied policy and is canceled when `next` returns.
Sandbox adapters must reject canceled contexts. Trusted host code must not retain
contexts or detach cancellation to reuse an elevated policy. The exported sandbox
`WithPolicy` helper is a trusted host API, not a model-facing authorization API.
Approvers that ignore cancellation cannot cause execution; their callback goroutine
can remain blocked until the host implementation returns. Approval is additionally
bounded by ten minutes and the enclosing dispatch context.

The host alone calls `SetSession(ctx, sessionID, mode)`. Every successful change,
including setting the same mode, increments a monotonic revision. Pending calls
recheck that revision immediately before starting. Already running calls retain
their snapshot. Session mode/revision live in `SessionStore`, separate from static
assembly manifests; individual approvals are never persisted. Store failures,
rollback, conflicting revisions and invalid recovered policies fail closed. Missing
store entries return zero state. Use one Controller per agent identity; hosts that
share a persistent store across processes must serialize mode updates themselves
(the minimal SessionStore API has no compare-and-swap operation).

`Resolve(ctx, sessionID)` returns a copied current policy without escalation.
`Snapshot(ctx, sessionID)` returns the policy plus an opaque fingerprint containing
the current monotonic revision and ancestor fingerprints. Configure a delegated
child with `Config.CeilingSnapshot: parent.Snapshot` so even an ancestor mode
change away and back invalidates its pending approvals. `Config.CeilingResolver`
remains available for live policy checks without revision tokens. The controller checks this extra ceiling before prompting and again
before starting execution. Resolver callbacks must not call the same controller or
form cycles. An inherited elevated context never replaces a child's own policy.

`RoleRules` offers optional `reader`, `reviewer`, `coordinator`, `coder`, and
`publisher` templates. Install with `policy.Default: policy.Ask` so unknown tools
require authorization. Reader/reviewer/coordinator allow dedicated read/search
operations and deny file mutations, arbitrary commands, git subprocesses and
linters. Coder permits dedicated file operations but asks for all command/git
execution and denies publishing/message actions. Publisher grants no automatic
external actions: host rules must bind recipients, repositories and contents.
Roles neither select sandbox modes nor grant identities, external resources or
child privileges. Explicit deny always wins.

`RuntimeContext(ctx, sessionID)` returns a fresh, authoritative text snapshot for
host injection immediately before each model request. Its JSON facts include the
effective mode, pinned canonical workspace, network policy, additional writable
roots and revision fingerprint. It describes current authority without granting
ordinary tool access. Resolution revalidates pinned default/ceiling roots and
rejects a later symlink replacement that would rebind a canonical workspace to a
different directory. This check does not eliminate concurrent filesystem races.
