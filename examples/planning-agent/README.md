# Offline planning examples

[简体中文](README_CN.md)

The module pins the remotely available core pseudo-version `v0.60.1-0.20261123132038-4ccdc96483d1` (commit `4ccdc96483d1`), including the required memory APIs. No workspace or local replace is required. This pins an immutable development commit; no release tag was created.

Run from the Harness repository root with the Go toolchain selected by the repository. Each directory holds one run.

```sh
go run ./examples/planning-agent -scenario dag -state-dir "$(mktemp -d)"
go run ./examples/planning-agent -scenario sources -state-dir "$(mktemp -d)"
go run ./examples/planning-agent -scenario replan -state-dir "$(mktemp -d)"
```

| Scenario | Expected result |
| --- | --- |
| `dag` | Extract the north station count, accept it, then produce a report with total 5. |
| `sources` | Extract the north and south station counts in two slots in the first wave. The next wave starts the report after review accepts both extractions. The report totals 12. |
| `replan` | Final review asks for the corrected north record. Revision 2 reuses the accepted south result, supersedes the old north result and report, and produces a report with total 15. |
| `wait` | Pause at a durable input gate and record confirmation after an explicit answer. |

Sources are synthetic JSON embedded in the executable. Extraction references locate `/count` in source inputs, and report references locate `/count` in accepted extraction artifacts. The host fills the source and quote digests before accepting an artifact. The local reviewer separately compares the extracted values and verifies the total.

## Evaluation

The separate CLI uses twelve frozen synthetic cases. Default `plan` preregisters 108 trials: direct Agent, planning with one slot, and the same planning configuration with four slots, each repeated three times. Every group has the same root request/token/context limits. Public requests and sources reach the model; private rubric labels reach only the code grader.

```sh
go run ./examples/planning-agent/cmd/eval -out ./eval-plan-new
go run ./examples/planning-agent/cmd/eval -backend scripted -out ./eval-scripted-new
```

Each output directory must be new. `manifest.json` fixes trial identities and settings before execution. `results.jsonl` retains one terminal record per trial, including failed and unstarted work; `summary.json` uses the planned count as its denominator. On Ctrl-C (SIGINT), the CLI cancels the current call, waits for accounting and all planned terminal rows to be written, then exits with a cancellation error. This does not provide recovery from SIGKILL or a process crash. `scripted` checks the runner locally and does not measure model quality.

An explicit `-backend live -model MODEL -out ./eval-live-new` uses the existing OpenAI-compatible provider. Configure `OPENAI_API_KEY` securely; `OPENAI_MODEL` and `OPENAI_BASE_URL` are optional defaults, with `-model`, `-base-url` and `-temperature` overrides. This mode makes paid requests. It creates isolated durable state per trial, counts client submissions against each root ledger, includes planner/reviewer calls and retries, and reports unknown usage/cost as unknown. The retained JSON field `physical_requests` counts calls submitted to the client, including local failures before transport; it is not an independent count of network requests. It has no external-effect tools. Results from the deterministic examples or scripted backend cannot demonstrate quality, latency or cost improvements.

The separate [memory layout comparison](internal/memoryeval/README.md) reuses `memory/memeval`. It forms memory once per history/repeat, applies a preregistered history-only pinned-record rule, and copies that snapshot for nil/quota layouts. It reports whether the layout actually changed submitted context, with formation, answering and judge costs separated. Use `cmd/memory-eval`; its default backend also only plans trials.

## Resume a waiting run

```sh
state_dir="$(mktemp -d)"
go run ./examples/planning-agent -scenario wait -state-dir "$state_dir"
```

The first command exits after printing `status: waiting`. Copy `interrupts[0].key` from its JSON output into `resume_key`, then run:

```sh
go run ./examples/planning-agent -scenario wait -state-dir "$state_dir" \
  -resume-key "$resume_key" -answer true
```

The gate publishes the schema `{"type":"boolean","const":true}`. Pass the JSON boolean `true` as shown above. A string such as `"true"` is rejected. Running the first command again with the same directory displays the persisted wait and its leaf interrupt key. For a deployed host, authenticate the caller and authorize the specific action before passing a response to the graph. The interrupt key identifies a continuation and carries no credentials.

## State, permissions, and cost

- The file store keeps the graph checkpoint, operation journal, and budget ledger. A completed run prints planning State, including accepted artifacts and historical attempts. Superseded artifacts remain in history and cannot feed subsequent tasks. A business outcome is reported in `outcome.status`, while execution errors are reported on stderr with a nonzero exit status.
- State includes a frozen copy of the bounded source JSON as well as derived results. Source files or records that a real host imported have their own storage and lifecycle. Deleting a dedicated demo state directory discards its local continuation and ledger. It does not remove the original embedded materials, delete independently stored source records, or undo external effects.
- The demo uses a trusted read-only local member and provides no network, shell, workspace mutation, or model backend. It makes zero model requests and requires no API key. A real host must enforce its own workspace, tool, network, and approval permissions. Planning dependencies and criteria grant no additional access.
- The examples cap execution at 8 tasks, 8 waves, 2 replans, 2 attempts per task, and 1 goal-resolution round. Bounds are 1 KiB per artifact, 8 KiB for artifacts in total, and 128 KiB for State. The persistent root budget caps model requests, tool calls, and agent calls at 8 each, with 4,096 reserved tokens. Deterministic local callbacks do not consume model, tool, or agent calls.
- To continue, keep the same directory, scenario, and compatible executable. The graph validates its workflow, state, planner, reviewer, member, and policy versions, and the budget ledger checks the root policy. For old checkpoints, use the matching previous executable and configuration. Use a fresh directory to run an incompatible version or a different scenario.
