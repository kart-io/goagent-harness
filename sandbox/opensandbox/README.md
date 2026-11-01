# OpenSandbox optional adapter

Independent module: `github.com/kart-io/goagent-harness/sandbox/opensandbox`.

- [Usage, host lifecycle, preset configuration and recovery](../../docs/guides/SANDBOX.md)
- [Design and interface contracts](../../docs/design/SANDBOX.md)
- [Verification report](../../docs/evaluation/2026-11-01-sandbox/REPORT.md)
- [Coding tools example](examples/coding/main.go)
- [Fixed workflow example](examples/workflow/main.go)

Requires Go 1.25, OpenSandbox Docker server/execd **release-1.2.0**, official Go SDK **v1.2.0**, Linux guest image with **python3** and **sleep infinity**, server proxy, explicit trusted `Options.ExecdImage: SupportedExecdImage` deployment pin and `Network:true`. `Network:false` is rejected before allocation. The parent module has no SDK dependency.

This unreleased module references the local parent commit. Use the temporary workspace instructions in the guide, then run `go test -race ./...` here. Root `go test ./...` does not traverse nested modules.

From the repository root, `python3 scripts/verify-sandbox.py --output /tmp/harness-sandbox-results` starts a private pinned Docker control service, runs real command/file/cancellation/lifecycle tests and both examples, then cleans its resources. Local Python3 is also needed for the helper's native filesystem unit tests.

Upstream primary sources: [Go SDK at release-1.2.0](https://github.com/opensandbox-group/OpenSandbox/tree/62a64f0b48cdd6b410f36e53c74dc7559c87719a/sdks/sandbox/go), [server at the same revision](https://github.com/opensandbox-group/OpenSandbox/tree/62a64f0b48cdd6b410f36e53c74dc7559c87719a/server). The SDK's declared module path remains `github.com/alibaba/OpenSandbox/sdks/sandbox/go`.

New validates SDK version; Create/Connect query the server `/version` before accessing instances. Only release 1.2.0 is accepted. Execd Ping does not expose its build version: the host must actually pin the declared Docker service image; the option is a trusted deployment assertion, not remote attestation.

Supplemental research: [capability comparison](../../docs/research/2026-11-01-sandbox-capabilities.md), [current API](../../docs/reference/SANDBOX_API.md), [proposed roadmap](../../docs/design/SANDBOX_ROADMAP.md), [measured research probe](../../docs/evaluation/2026-11-01-sandbox-research/REPORT.md). The upstream SDK renewal/pause calls were verified separately; the managed Harness lifecycle has no such public methods yet.
