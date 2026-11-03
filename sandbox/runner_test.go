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

// fakeDockerCLI 模拟 docker create 和 start：create 记下镜像之后的命令，start 执行它。
// createFailure 非空时 create 打印它并以 125 退出。capture 非空时把每次调用的参数追加进去。
func fakeDockerCLI(t *testing.T, image, createFailure, capture string) {
	t.Helper()
	bin := t.TempDir()
	saved := filepath.Join(bin, "command")
	script := "#!/bin/sh\n"
	if capture != "" {
		script += "printf '%s\\n' \"$@\" >> " + shellQuote(capture) + "\n"
	}
	script += "case \"$1\" in\ncreate)\n"
	if createFailure != "" {
		script += "  echo " + shellQuote(createFailure) + " >&2; exit 125 ;;\n"
	} else {
		script += "  while [ \"$1\" != " + shellQuote(image) + " ]; do shift; done; shift\n  : > " + shellQuote(saved) + "\n  for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + shellQuote(saved) + "; done ;;\n"
	}
	script += "start)\n  set --; while IFS= read -r a; do set -- \"$@\" \"$a\"; done < " + shellQuote(saved) + "\n  exec \"$@\" ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
}

// A failed runner must not masquerade as a command that ran and was denied.
func TestConfinedRunnerStartupIsDistinctFromCommandFailure(t *testing.T) {
	for _, startupFailure := range []bool{true, false} {
		name := "command"
		if startupFailure {
			name = "startup"
		}
		t.Run(name, func(t *testing.T) {
			failure := ""
			if startupFailure {
				failure = "docker: fixture startup failed"
			}
			fakeDockerCLI(t, "harness-test-image", failure, "")
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
