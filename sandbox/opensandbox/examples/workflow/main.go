// A fixed workflow uses the sandbox directly, without a child Agent or model.
package main

import (
	"context"
	"fmt"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/opensandbox/examples/internal/demo"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/workflow"
	"log"
	"strings"
)

func main() {
	if err := demo.Run(run); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context, s *sandbox.Session) error {
	g := workflow.NewGraph[string]()
	if err := g.AddNode("prepare", func(ctx context.Context, input string) (string, error) {
		_, err := s.FileSystem().WriteFile(ctx, "input.txt", []byte(input), env.WriteIntent{CreateIfAbsent: true})
		return input, err
	}); err != nil {
		return err
	}
	if err := g.AddNode("execute", func(ctx context.Context, _ string) (string, error) {
		result, err := s.Executor().Run(ctx, env.Command{Argv: []string{"python3", "-c", "from pathlib import Path;Path('output.txt').write_text(Path('input.txt').read_text().upper())"}})
		if err != nil {
			return "", err
		}
		if result.ExitCode != 0 {
			return "", fmt.Errorf("command exit %d: %s", result.ExitCode, result.Output)
		}
		data, _, err := s.FileSystem().ReadFile(ctx, "output.txt")
		return string(data), err
	}); err != nil {
		return err
	}
	for _, edge := range [][2]string{{workflow.START, "prepare"}, {"prepare", "execute"}, {"execute", workflow.END}} {
		if err := g.AddEdge(edge[0], edge[1]); err != nil {
			return err
		}
	}
	runnable, err := g.Compile(ctx)
	if err != nil {
		return err
	}
	out, err := runnable.Invoke(ctx, "hello workflow")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "HELLO WORKFLOW" {
		return fmt.Errorf("unexpected result %q", out)
	}
	fmt.Println(out)
	return nil
}
