# Harness merge code report

Initial code handoff resolved conflicts without staging; root subsequently staged the integrated tree for independent review. Main DSH implementation remains preset/sandbox.go, and all original main tests remain preset/sandbox_test.go. Feature session assembly/tests are now preset/managed_sandbox.go and preset/managed_sandbox_test.go. No vendor adapter or fake product implementation added.

## Managed policy boundary

- Host.Root remains the local, normalized authorization domain used by main's sandbox.Policy, permissions.Controller, SandboxCeiling, parent ceilings, live session policy, and approval processing. The managed environment's remote Root is workspace identity/display and is accessed through its bound filesystem. Remote paths are never passed to host Normalize as though they were local directories.
- executor=sandbox supports trusted registry/provider/session ownership, retained reconnect identity, lifecycle cancellation/cleanup, and provider/version/environment identity in the manifest.
- A managed backend with commands enabled MUST implement sandbox.PolicyExecutor on Environment.Executor(): CheckPolicy(context.Context, Policy) error and RunWithPolicy(context.Context, env.Command, Policy) (env.Result,error). CheckPolicy is called for both initial policy and maximum ceiling at assembly, then again with the effective invocation policy before each command. It must validate/enforce mode, filesystem confinement and descendant execution/network restrictions, and explicitly reject policies it cannot implement. Isolated/Container/ReadOnlyRoot capability booleans do not grant this authority.
- Both read-only->workspace-write one-call escalation and host session mode changes retain the main permissions controller, approval semantics and effective policy propagation. File operations use sandbox.FileSystem so unauthorized/expired mutations fail closed.
- Without PolicyExecutor, only sandbox=read-only,maxSandbox=read-only is supported: file tools use a read-only view; no commands, git command tools or linters are enabled. Read-only managed configurations with linters are explicitly rejected. With maxSandbox wider than read-only, PolicyExecutor is required even if the selected tool list currently only reads files.
- AdditionalWritableRoots and EnvAllowlist are explicitly unsupported for every managed backend and rejected before allocation: there is no trustworthy host-to-managed resource mapping contract yet. Local/native/docker retain their existing implementations and inherited environment checks.
- Network must match the registry request at creation; command policy enforcement additionally requires PolicyExecutor.CheckPolicy/RunWithPolicy. A mere matching capability flag is insufficient to enable commands.
- Main hard host ceiling and parent mode/network bounds are validated before allocation. Parent policy/ceiling/controller/envAllowlist now survive intermediate subagents without workspaces. Skills implementation and boundaries remain main's.
- Filesystem mutation protection is retained before policy wrapping for backends implementing MutationProtector. ContextResolver is forwarded through sandbox.FileSystem to preserve remote operation cancellation.

## Tests and checks

Existing main and feature test assertions are retained. The feature test fixture is strengthened to implement the explicit policy contract; it executes a fixed response, so it cannot write files or make network requests. This implementation exists only in _test.go.

New merge regression tests cover legacy isolation claims without policy enforcement, cleanup after that rejection, hard host ceiling before allocation, rejected unmapped host roots/environment, managed filesystem one-call approval/expiry, bound filesystem identity, and propagation of live session modes to policy-aware execution.

RED: /tmp/harness-no-vendor-red.log captured formerly accepted unstructured execution and ignored env allowlist. The initial extra-root test also exposed malformed flow-YAML and was corrected before GREEN.

GREEN:
- env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go test ./preset ./sandbox ./tools/coding (/tmp/harness-no-vendor-focused.log)
- env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go test -race ./... (/tmp/harness-no-vendor-race.log), exit 0
- env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go build ./... (/tmp/harness-no-vendor-build.log), exit 0
- git diff --check clean; no Go conflict markers.

Go dependency: core upgraded from v0.23.0 to v0.32.2; main golang.org/x/sys v0.41.0 preserved. No go mod tidy churn.

## Follow-up: prevent durable replay of unconfirmed effects

Root cause verified against the actual core v0.32.2 Dispatcher/durable.Do chain: absent a tool policy, Dispatcher selects MaxAttempts=3 and RetryFailed=true. ExecutionError preserves ErrUnconfirmed on the first invocation but records a failed durable operation; a later invocation with the same journal/execution key/CallID would execute it again. Additionally, permissions.DecorateTools previously erased the optional DurablePolicyProvider interface.

Minimal fix:
- coding.commandTool now declares durable.OperationPolicy{MaxAttempts: 1}, with RetryFailed and Resumable false. This shared wrapper covers run_command, git_commit (including add/commit/rev-parse), git query tools, write_file, edit_file and multi_edit, including command linters. It prevents a second execution of a failed/pending durable operation; successful completed outputs still replay normally. Non-journal calls and distinct logical call IDs retain their ordinary behavior.
- permissions.DecorateTools preserves DurablePolicyProvider only when the input has it, forwarding the complete policy including reconciliation callbacks. Existing DispatchTimeout behavior is preserved with and without an explicit durable policy. No retry policy is imposed on unrelated tools.
- Unknown outcomes remain failures. The initial invocation retains errors.Is(err, sandbox.ErrUnconfirmed). After reopening the journal, same-CallID recovery returns durable.ErrOperationAttention; core's persisted failure record does not restore the original Go error type, and the code/tests do not pretend it does. No unconfirmed outcome is converted to a confirmed timeout or success, and no hidden retry/reconciliation was introduced.

Regression evidence uses real preset.Build -> coding-tools -> permission schema decoration -> Dispatcher -> file journal. The command backend reports an already-started effect followed by a lost response; the write_file backend actually writes before reporting the unconfirmed response. After reopening FileStore, the second identical call leaves execution/mutation count at 1. Tests cover run_command, git_commit, and write_file. A decorator regression also verifies MaxAttempts/RetryFailed/Resumable/Reconcile forwarding and timeout preservation.

RED: /tmp/harness-no-vendor-durable-red.log demonstrates duplicate command execution (run_command count 2, git_commit count 3) and erased durable policy. GREEN focused checks: /tmp/harness-no-vendor-durable-green.log. Covering race suite (integration, permissions, tools/coding): /tmp/harness-no-vendor-durable-covering-race.log, exit 0.

Final follow-up validation:
- env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go test -race ./... -> exit 0 (/tmp/harness-no-vendor-durable-full-race.log)
- env -u GOROOT GOTOOLCHAIN=go1.25.0 GOWORK=off go build ./... -> exit 0 (/tmp/harness-no-vendor-durable-build.log)
- git diff --check -> clean.

Follow-up files: tools/coding/command_tools.go, permissions/schema.go, permissions/schema_test.go, integration/managed_durable_test.go. No index manipulation, commit or push performed by this worker.
