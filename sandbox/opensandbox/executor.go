package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
)

type commandRunner struct {
	sandboxID                            string
	client                               *sdk.ExecdClient
	poll, cleanupTimeout, requestTimeout time.Duration
}
type commandRejected struct{}

func (commandRejected) Error() string { return "OpenSandbox rejected command startup" }

func (r *commandRunner) run(ctx context.Context, argv []string, dir string, max int64) (env.Result, error) {
	result := env.Result{ExitCode: -1}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if len(argv) == 0 || argv[0] == "" || max <= 0 {
		return result, errors.New("command argv and positive output limit required")
	}
	for _, arg := range argv {
		if strings.ContainsRune(arg, 0) {
			return result, errors.New("command argument contains NUL")
		}
	}
	startCtx, cancel := context.WithTimeout(ctx, r.requestTimeout)
	id := ""
	err := r.client.RunCommand(startCtx, sdk.RunCommandRequest{Argv: argv, Cwd: dir, Background: true}, func(event sdk.StreamEvent) error {
		if event.Data == "" {
			return nil
		}
		var message struct{ Type, Text string }
		if err := json.Unmarshal([]byte(event.Data), &message); err != nil {
			return err
		}
		switch message.Type {
		case "init":
			if id != "" {
				return errors.New("multiple command IDs in startup stream")
			}
			id = message.Text
		case "error":
			if id == "" {
				return commandRejected{}
			}
		}
		return nil
	})
	cancel()
	abort := func(cause error) (env.Result, error) {
		if id == "" {
			return result, &sandbox.UnconfirmedError{SandboxID: r.sandboxID, Cause: cause}
		}
		return result, r.stop(ctx, id, cause)
	}
	if err != nil {
		var rejected commandRejected
		var apiError *sdk.APIError
		if id == "" && (errors.As(err, &rejected) || (errors.As(err, &apiError) && apiError.StatusCode >= 400 && apiError.StatusCode < 500)) {
			return result, err
		}
		return abort(err)
	}
	if id == "" {
		return abort(errors.New("startup stream has no command ID"))
	}
	cursor := int64(0)
	readLogs := func() (bool, error) {
		rpc, cancel := context.WithTimeout(ctx, r.requestTimeout)
		defer cancel()
		logs, err := r.client.GetCommandLogs(rpc, id, &cursor)
		if err != nil {
			return false, err
		}
		if logs.Cursor < cursor || (logs.Output != "" && logs.Cursor == cursor) {
			return false, errors.New("command log cursor did not advance")
		}
		remaining := max - int64(len(result.Output))
		if int64(len(logs.Output)) > remaining {
			result.Output = append(result.Output, logs.Output[:int(remaining)]...)
			return false, sandbox.ErrOutputLimit
		}
		changed := logs.Cursor != cursor
		result.Output = append(result.Output, logs.Output...)
		cursor = logs.Cursor
		return changed, nil
	}
	for {
		if _, err = readLogs(); err != nil {
			return abort(err)
		}
		rpc, done := context.WithTimeout(ctx, r.requestTimeout)
		status, statusErr := r.client.GetCommandStatus(rpc, id)
		done()
		if statusErr != nil {
			return abort(statusErr)
		}
		if !status.Running {
			if status.ExitCode == nil {
				return result, &sandbox.UnconfirmedError{SandboxID: r.sandboxID, CommandID: id, Cause: errors.New("stopped command has no exit code")}
			}
			result.ExitCode = int(*status.ExitCode)
			// Wait for two stable log cursors after exit, so trailing pipe output is
			// not discarded merely because process status became terminal first.
			for stable := 0; stable < 2; {
				changed, err := readLogs()
				if err != nil {
					return abort(err)
				}
				if changed {
					stable = 0
				} else {
					stable++
				}
				if stable < 2 {
					if err := wait(ctx, r.poll); err != nil {
						return abort(err)
					}
				}
			}
			return result, nil
		}
		if err = wait(ctx, r.poll); err != nil {
			return abort(err)
		}
	}
}

func (r *commandRunner) stop(parent context.Context, id string, cause error) error {
	clean, cancel := context.WithTimeout(context.WithoutCancel(parent), r.cleanupTimeout)
	defer cancel()
	interruptErr := r.client.InterruptCommand(clean, id)
	var last error
	for {
		status, err := r.client.GetCommandStatus(clean, id)
		if err == nil && !status.Running {
			return errors.Join(cause, interruptErr)
		}
		last = err
		if err = wait(clean, r.poll); err != nil {
			return &sandbox.UnconfirmedError{SandboxID: r.sandboxID, CommandID: id, Cause: errors.Join(cause, interruptErr, last, err)}
		}
	}
}
func wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return fmt.Errorf("poll duration must be positive")
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
