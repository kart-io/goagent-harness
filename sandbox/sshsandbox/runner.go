package sshsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
)

const (
	runnerFailedRC   = 125
	runnerTimedOutRC = 124
	runnerPrefix     = "sandbox-runner: "
	runnerDenied     = runnerPrefix + "denied: "
	runnerTimedOut   = runnerPrefix + "timed out\n"
)

// runnerConfig 是宿主传给远端辅助程序的完整策略。
type runnerConfig struct {
	Workspace string          `json:"workspace"`
	Mode      sandbox.Mode    `json:"mode"`
	Network   sandbox.Network `json:"network"`
	Dir       string          `json:"dir,omitempty"`
	Timeout   time.Duration   `json:"timeout,omitempty"`
}

func (c runnerConfig) executor() *sandbox.Executor {
	return &sandbox.Executor{Backend: "native", Policy: sandbox.Policy{WorkspaceRoot: c.Workspace, Mode: c.Mode, Network: c.Network}}
}

// RunnerMain 是 sandbox-runner 的入口：`probe <config>` 输出探测报告，`<config> -- argv...` 在隔离中运行命令并原样转出退出码。
// 命令的输出都写到 stdout，stderr 只留给辅助程序自己的错误。SIGTERM 会终止命令及其进程组。
func RunnerMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	fail := func(prefix string, err error) int {
		_, _ = fmt.Fprintf(stderr, "%s%v\n", prefix, err)
		return runnerFailedRC
	}
	if len(args) == 2 && args[0] == "probe" {
		var c runnerConfig
		if err := json.Unmarshal([]byte(args[1]), &c); err != nil {
			return fail(runnerPrefix, err)
		}
		report, err := c.executor().Probe(ctx)
		if err != nil {
			return fail(runnerPrefix, err)
		}
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return fail(runnerPrefix, err)
		}
		return 0
	}
	if len(args) < 3 || args[1] != "--" {
		return fail(runnerPrefix, errors.New("usage: sandbox-runner probe <config> | <config> -- <argv>"))
	}
	var c runnerConfig
	if err := json.Unmarshal([]byte(args[0]), &c); err != nil {
		return fail(runnerPrefix, err)
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	res, err := c.executor().Run(ctx, env.Command{Argv: args[2:], Dir: c.Dir})
	_, _ = stdout.Write(res.Output)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		_, _ = io.WriteString(stdout, runnerTimedOut)
		return runnerTimedOutRC
	case errors.Is(err, context.Canceled):
		return 128 + int(syscall.SIGTERM)
	case errors.Is(err, sandbox.ErrDenied):
		return fail(runnerDenied, err)
	case err != nil:
		return fail(runnerPrefix, err)
	case res.ExitCode < 0:
		return 128 + int(syscall.SIGKILL)
	}
	return res.ExitCode
}
