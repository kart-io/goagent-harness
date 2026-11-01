// This example uses no model/API key. It exercises the same policy pipeline an
// Agent uses: deny a write, optionally approve one write, then deny the next write.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
)

func main() {
	backend := flag.String("backend", "native", "native or docker")
	approve := flag.Bool("approve", false, "approve the demonstration's single file write")
	flag.Parse()
	if err := run(*backend, *approve); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(backend string, approve bool) error {
	if backend != "native" && backend != "docker" {
		return fmt.Errorf("backend must be native or docker")
	}
	root, err := os.MkdirTemp("", "harness-permission-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	image := ""
	if backend == "docker" {
		image = ", image: alpine:3"
	}
	set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Trusted: true, Data: []byte(fmt.Sprintf(`presets:
  - id: coder
    components:
      - {id: workspace, use: workspace, config: {sandbox: read-only, maxSandbox: workspace-write, executor: %s%s}}
      - {id: tools, use: coding-tools, config: {tools: [write_file]}}
      - {id: permissions, use: permissions, config: {rules: [{tool: write_file, decision: allow}]}}
`, backend, image))})
	if err != nil {
		return err
	}
	p, _ := set.Get("coder")
	asm, err := preset.Build(context.Background(), p, preset.Host{Root: root, AgentID: "demo-coder", Approver: tools.ApproverFunc(func(_ context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
		fmt.Printf("approval: agent=%s session=%s call=%s\n", req.Call.CallerID, req.Call.SessionID, req.Call.CallID)
		if approve {
			return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
		}
		return tools.ApprovalResult{Outcome: tools.ApprovalRejected}, nil
	})})
	if err != nil {
		return err
	}
	defer asm.Close()
	ctx := tools.WithSessionID(context.Background(), "demo-session")
	for _, tc := range []struct {
		id, path string
		escalate bool
	}{{"first", "denied.txt", false}, {"approved", "allowed.txt", true}, {"last", "still-denied.txt", false}} {
		args := map[string]any{"path": tc.path, "content": "demo\n"}
		if tc.escalate {
			args["sandbox_permissions"] = "workspace-write"
			args["justification"] = "Create the demonstration file for this call only"
		}
		out, err := asm.Tools[0].Invoke(ctx, &interfaces.ToolInput{CallID: tc.id, Args: args})
		if err != nil {
			return err
		}
		fmt.Printf("%s: success=%t\n", tc.id, out.Success)
		if want := tc.escalate && approve; out.Success != want {
			return fmt.Errorf("unexpected permission result for %s", tc.id)
		}
	}
	return nil
}
