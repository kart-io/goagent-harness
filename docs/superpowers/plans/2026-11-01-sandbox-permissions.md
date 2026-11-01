# DSH sandbox and Agent permissions implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Deliver the approved DSH-style policy/enforcement split and scoped Agent permissions without changing existing Docker defaults.

**Architecture:** Harness-owned sandbox adapters implement GoAgent env interfaces. An independent permission controller resolves per-call immutable policy, performs single-call escalation and checks parent/host ceilings. Preset integrates both without changing GoAgent.

**Tech Stack:** Go 1.25+, existing GoAgent v0.23.0; Docker, macOS Seatbelt, Linux Bubblewrap.

**Spec:** `docs/design/2026-11-01-sandbox-agent-permissions.md` (user approved 2026-11-01).

## Global constraints

- Preserve Docker default, network false, legacy local meaning and existing relative FS paths.
- No upstream GoAgent checkout changes, secret access, or automatic unrestricted fallback.
- Native Windows and Landlock are explicitly unavailable/deferred, not advertised as implemented.
- Use `env -u GOROOT GOWORK=off go ...` because inherited GOROOT points to a different toolchain.
- Work on feature branch; no push or merge requested in this implementation turn.

## Review focus

- Concurrent approvals must not share an escalation or mutate shared Workspace mode (task 2 tests).
- An allowed go test can execute arbitrary code: OS boundary, not command-prefix parsing, must confine writes (tasks 1/3).
- A readonly coordinator must not gain writable child privileges (task 3).
- Native unavailable/network inability must fail closed with no side effects (task 1).
- Session mode changes, policy revisions and recovery invalidate old approval (task 2).

## Shared API contract

Package `sandbox`:
`type Mode string` with `ReadOnly`, `WorkspaceWrite`, `DangerFullAccess`.
`type Network string` with `NetworkDeny`, `NetworkInherit`.
`type Policy struct { Mode Mode; WorkspaceRoot string; AdditionalWritableRoots []string; Network Network }`.
`Normalize(Policy) (Policy,error)` canonicalizes existing directories/defaults; rejects invalid/symlink extra roots.
`Within(ceiling, requested Policy) error` validates mode, directory inclusion and network; an empty workspace is invalid.
`WithPolicy(context.Context, Policy) context.Context`; `FromContext(context.Context) (Policy,bool)` copies values.
`type FileSystem struct { env.FileSystem; Policy Policy }` implements WriteFile/Remove using context policy with static fallback, preserving FS path scope.
`type Executor struct { Policy Policy; Backend string; Image string; EnvAllowlist []string }` implements env.Executor.
`(*Executor).Probe(context.Context) (Report,error)` validates actual OS/file/network enforcement for requested policy.
`type Report struct { Backend string; Enforcement string; Network Network; WritableRoots []string }`.
Backend names docker/native/local; local means unrestricted host execution and requires danger-full-access/network inherit.
`ErrUnavailable` and `ErrDenied` are sentinel errors. Mode has exported `Rank() int`.

Package `permissions`:
`type Config struct { AgentID string; Default sandbox.Policy; Ceiling sandbox.Policy; Approver tools.Approver; Store SessionStore }`.
`New(Config) (*Controller,error)`, `(*Controller).Around() tools.Around`, `(*Controller).SetSession(ctx, sessionID string, mode sandbox.Mode) error`.
`type SessionState struct { Mode sandbox.Mode; Revision uint64 }`.
`type SessionStore interface { Load(ctx context.Context, agentID, sessionID string) (SessionState,error); Save(ctx context.Context, agentID, sessionID string, state SessionState) error }`.
Missing state is zero value; host owns persistent implementation; Controller serializes state access and compares revision before executing.
Tool schema helper `DecorateTools([]interfaces.Tool) []interfaces.Tool` adds paired sandbox_permissions/justification to schemas while preserving DispatchTimeoutProvider.
Escalation approval uses tools.Approver directly, outside Pipeline approval coalescing, requires AllowedOnce (AllowedSession treated as once); immutable policy lives only in execution context.
`RoleRules(role string) ([]policy.Rule,error)` supplies reader/reviewer/coder/coordinator/publisher templates; explicit deny always wins. These are optional presets, not identities or privileges.

### Task 1: Sandbox policies and OS execution

Files: new `sandbox/*.go`, `sandbox/*_test.go`, `sandbox/README.md`.
Consumes GoAgent env interfaces; produces the sandbox API above.

- [x] Write failing policy/FS tests for write denial, child scope/network widening and context copies; run `go test ./sandbox` and observe missing API failure.
- [x] Implement normalization, containment, FS fence and immutable context handling; rerun tests.
- [x] Write backend tests for unavailable platform, ordinary nonzero exit, path/network deny and secret env exclusion; observe failure.
- [x] Implement Docker/native/local adapters with bounded output/process cleanup and explicit report. Validate macOS real writes/network and Linux via gated platform tests; unsupported host reports skip/unverified.
- [x] Run sandbox tests and race checks; document implementation/platform limits.

### Task 2: Scoped permission controller

Files: new `permissions/*.go`, `permissions/*_test.go`, `permissions/README.md`.
Consumes sandbox policy/context; produces controller, session store and schema decorators above.

- [x] Write failing tests proving no preapproval side effects, denial, no session-wide escalation, paired args and exact identity isolation.
- [x] Implement controller with independent per-call approval and revision recheck. Never allow args to supply Agent/Session identity. Reject missing identity when escalation requested.
- [x] Write concurrency, SetSession, store recovery and role restriction tests before implementation.
- [x] Implement host-owned session switch/persistence and roles. No roles auto-grant external action resources.
- [x] Run `go test -race ./permissions` and inspect every failure.

### Task 3: Preset and coding integration

Files: `preset/{builtin,build,source,subagent}.go`, new `preset/sandbox.go`, tests and coding error adaptation if needed.
Consumes APIs from tasks 1/2. WorkspaceConfig gains additionalWritableRoots, maxSandbox, envAllowlist; native is explicit. Host gains SessionStore and AgentID configuration.

- [x] Write failing integration tests for native readonly commands, per-call file escalation, never approval, Docker backward compatibility and child network/directory ceilings.
- [x] Wire policy FS/Executor, controller Around and schema decoration without shared mutable Workspace mode. Retain readonly writes hidden unless an explicit maxSandbox permits elevation.
- [x] Extend untrusted config Needs/Trust expansion accounting for extra writable roots, maxSandbox and environment pass-through.
- [x] Check parent scopes by full policies rather than rank alone; propagated hard Guard denies unauthorized escalation.
- [x] Pin static policy/backend contract in manifest; session revisions remain independent. Run all existing tests.

### Task 4: Documentation and final review

Files: README, preset/README, docs/MIGRATION, runnable example and completed plan status.

- [x] Document role matrices, native selection, per-call escalation and host session persistence examples; explicitly state partial platform parity.
- [x] Run `go test ./...`, `go test -race ./sandbox ./permissions ./preset ./tools/coding ./integration`, `go vet ./...`, `go build ./...` with the toolchain environment above.
- [x] Fresh reviewer audits all code against spec and security boundaries. Fix important findings with regression tests, rerun affected checks.
- [x] Record supported vs unverified platforms accurately. No claim of Linux validation from macOS unit tests.

## Execution ledger

- Baseline: preset/coding/integration passed before implementation.
- Ruling: use feature branch in the existing clean checkout; avoid another approval round for reversible workspace setup.
- Ruling: implement independent package tasks concurrently, then integrate on fixed shared interfaces. Parent does not edit worker-owned files until handoff.

- Task 1 complete: policy, named file scopes, Docker/macOS/Linux adapters; native macOS and Docker real boundaries verified. Linux filter decisions executed in Alpine arm64; full Bubblewrap runtime remains unverified, enforced by deployment Probe.
- Task 2 complete: permission controller, roles, immutable call contexts, independent approvals, durable host-store interface, parent revision tokens and runtime policy facts; race tests pass.
- Task 3 complete: Preset wiring, hard host ceiling, parent directory/network/environment intersection, scoped linter paths and manifest identity/contract binding; regression tests pass.
- Task 4 complete: runnable keyless demo, migration docs, full suite/race/vet/build and local GoAgent compatibility checks pass; fresh security review reports no remaining important findings.
- Ruling: Docker profiles derive only stricter rules from pinned Apache-2.0 Moby baseline; avoid replacing daemon defaults with an allow-all syscall policy. Constrained Linux/Docker deny socketpair and Unix sockets, which can affect programs requiring anonymous Unix IPC; macOS preserves anonymous pairs through pathname-specific enforcement.
- Ruling: inherited GOROOT is removed only for verification subprocesses; no global shell configuration changes.
- Ruling: no full Linux runtime RED/GREEN claim: Linux-only seccomp interpreter tests execute in a local Alpine container and both supported architectures compile; live Bubblewrap still needs an appropriate Linux host.

- Follow-up verification: Ubuntu 24.04 ARM64 Colima VM tested with a temporary extracted, checksum-verified Bubblewrap 0.9.0; no system package/security configuration changed. Eight focused groups passed ten rounds in a privileged test process. Ordinary-user namespace setup is denied and Probe fails closed; amd64 runtime remains unverified.
- Verification fix RED→GREEN: TCP/Unix HTTP probes must wait for a request before replying; former premature response/close reproduced curl Broken pipe on Linux. Regression fails deterministically against the old server.
- Verification fix RED→GREEN: internal command-start acknowledgement distinguishes runner startup failure from an ordinary command returning the identical text/exit125. Returned output contains no marker. Contract bumped to v2.
- Environment finding: running native root-remount tests concurrently with Docker overlay mount churn can make Bubblewrap startup fail. This is an unavailable backend, never evidence of policy denial; Linux boundary validation was repeated without concurrent Docker fixture changes.
