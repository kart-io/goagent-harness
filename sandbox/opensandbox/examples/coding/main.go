// A deterministic tool invocation using the same workspace an Agent receives.
package main

import (
	"context"
	"fmt"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/opensandbox/examples/internal/demo"
	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/tools"
	"log"
)

func main() {
	if err := demo.Run(run); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context, s *sandbox.Session) error {
	workspace, err := coding.New(s.FileSystem(), s.Executor())
	if err != nil {
		return err
	}
	registry := tools.NewRegistry()
	for _, tool := range workspace.Tools() {
		if err = registry.Register(tool); err != nil {
			return err
		}
	}
	dispatcher, err := tools.NewDispatcher(registry, tools.DispatchConfig{})
	if err != nil {
		return err
	}
	for _, request := range []tools.ToolRequest{
		{Name: "write_file", Input: &interfaces.ToolInput{Args: map[string]interface{}{"path": "demo/result.txt", "content": "hello from remote coding tools\n"}}},
		{Name: "read_file", Input: &interfaces.ToolInput{Args: map[string]interface{}{"path": "demo/result.txt"}}},
	} {
		out, err := dispatcher.Invoke(ctx, request)
		if err != nil {
			return err
		}
		if !out.Success {
			return fmt.Errorf("tool %s: %v", request.Name, out.Error)
		}
		fmt.Println(request.Name, out.Result)
	}
	// dispatcher.Tools() can be passed to builder.WithTools for model-driven use.
	return nil
}
