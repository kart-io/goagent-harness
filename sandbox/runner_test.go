package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kart-io/goagent/harness/env"
)

// A failed runner must not masquerade as a command that ran and was denied.
func TestConfinedRunnerStartupIsDistinctFromCommandFailure(t *testing.T) {
	for _, startupFailure := range []bool{true, false} {
		name := "command"
		if startupFailure {
			name = "startup"
		}
		t.Run(name, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\nif [ \"$1\" != run ]; then exit 0; fi\n"
			if startupFailure {
				script += "echo 'docker: fixture startup failed' >&2; exit 125\n"
			} else {
				script += "while [ \"$1\" != harness-test-image ]; do shift; done\nshift\nexec \"$@\"\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			e := Executor{Backend: "docker", Image: "harness-test-image", Policy: Policy{Mode: WorkspaceWrite, WorkspaceRoot: t.TempDir(), Network: NetworkDeny}}
			result, err := e.raw(context.Background(), e.Policy, env.Command{Argv: []string{"sh", "-c", "printf 'docker: fixture startup failed'; exit 125"}})
			if startupFailure {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("runner failure was not unavailable: %+v %v", result, err)
				}
			} else if err != nil || result.ExitCode != 125 || string(result.Output) != "docker: fixture startup failed" {
				t.Fatalf("ordinary command was misclassified or output changed: %+v %v", result, err)
			}
			if strings.Contains(string(result.Output), "harness-command-start-") {
				t.Fatal("internal start marker leaked")
			}
		})
	}
}
