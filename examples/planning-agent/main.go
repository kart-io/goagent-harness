// planning-agent demonstrates durable planning with deterministic local components.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	agenterrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/execution"
	"github.com/kart-io/goagent/workflow"
)

const exampleRunID = "planning-example"

func runScenario(ctx context.Context, dir string, initial p.State, cfg p.Config, deps p.Dependencies, key string, answer json.RawMessage) (*workflow.GraphSnapshot[p.State], error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store, err := durable.NewFileStore(dir)
	if err != nil {
		return nil, err
	}
	controller, err := budget.NewDurable(budget.Config{
		RootKey: exampleRunID, Version: "offline-budget-v1",
		TokenPolicyVersion: "offline-token-v1",
		MaxModelRequests:   8, MaxReservedTokens: 4096, MaxToolCalls: 8, MaxAgentCalls: 8,
	}, store)
	if err != nil {
		return nil, err
	}
	scope, err := execution.NewScope(execution.Config{
		RunID: exampleRunID, SessionID: exampleRunID, Journal: store, Budget: controller,
	})
	if err != nil {
		return nil, err
	}
	ctx, stop, err := scope.Bind(ctx)
	if err != nil {
		return nil, err
	}
	defer stop()
	graph, err := p.NewGraph(cfg, deps)
	if err != nil {
		return nil, err
	}
	config, err := p.DurableConfig(cfg, deps, store)
	if err != nil {
		return nil, err
	}
	graphRun, err := graph.CompileDurable(ctx, config)
	if err != nil {
		return nil, err
	}
	if key != "" {
		return graphRun.Resume(ctx, exampleRunID, workflow.ResumeRequest{Key: key, Input: answer})
	}
	initial, err = p.PrepareInitialState(initial, cfg)
	if err != nil {
		return nil, err
	}
	return graphRun.Run(ctx, exampleRunID, initial, "")
}

func main() {
	if err := runCLI(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI() error {
	name := flag.String("scenario", "dag", "offline scenario: dag, replan, wait, or sources")
	dir := flag.String("state-dir", "", "checkpoint directory (required, one run per directory)")
	key := flag.String("resume-key", "", "leaf interrupt key printed by the waiting run")
	answer := flag.String("answer", "", "JSON input for -resume-key")
	flag.Parse()
	if *dir == "" || flag.NArg() != 0 || (*key == "") != (*answer == "") {
		return errors.New("supply -state-dir and, when answering a wait, both -resume-key and -answer")
	}
	initial, cfg, deps, err := scenario(*name)
	if err != nil {
		return err
	}
	snap, err := runScenario(context.Background(), *dir, initial, cfg, deps, *key, json.RawMessage(*answer))
	if err != nil && (!errors.Is(err, agenterrors.ErrApprovalRequired) || snap == nil || !snap.Waiting) {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if snap.Waiting {
		return encoder.Encode(struct {
			Status     string                    `json:"status"`
			Interrupts []workflow.InterruptPoint `json:"interrupts"`
		}{"waiting", snap.Interrupts})
	}
	return encoder.Encode(snap.State)
}
