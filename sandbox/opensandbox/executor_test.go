package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/stretchr/testify/require"
)

type commandServer struct {
	mu                                  sync.Mutex
	request                             sdk.RunCommandRequest
	starts, interrupts                  int
	running, ignoreInterrupt, missingID bool
	logs                                string
	exit                                int32
}

func (s *commandServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == "POST" && r.URL.Path == "/command":
		_ = json.NewDecoder(r.Body).Decode(&s.request)
		s.starts++
		w.Header().Set("Content-Type", "text/event-stream")
		if !s.missingID {
			_, _ = fmt.Fprint(w, "data: {\"type\":\"init\",\"text\":\"command-1\"}\n\n")
		}
		_, _ = fmt.Fprint(w, "data: {\"type\":\"execution_complete\"}\n\n")
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/command/status/"):
		_ = json.NewEncoder(w).Encode(sdk.CommandStatusResponse{ID: "command-1", Running: s.running, ExitCode: &s.exit})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/logs"):
		w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", fmt.Sprint(len(s.logs)))
		if r.URL.Query().Get("cursor") != "0" && r.URL.Query().Get("cursor") != "" {
			return
		}
		_, _ = fmt.Fprint(w, s.logs)
	case r.Method == "DELETE" && r.URL.Path == "/command":
		s.interrupts++
		if !s.ignoreInterrupt {
			s.running = false
		}
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}
func runnerFixture(t *testing.T, state *commandServer) *commandRunner {
	t.Helper()
	server := httptest.NewServer(state)
	t.Cleanup(server.Close)
	client := sdk.NewExecdClient(server.URL, "", sdk.WithHTTPClient(&http.Client{Transport: &responseLimitTransport{base: http.DefaultTransport, limit: 1 << 20}}))
	return &commandRunner{sandboxID: "sandbox-1", client: client, poll: time.Millisecond, cleanupTimeout: 25 * time.Millisecond, requestTimeout: time.Second}
}
func TestCommandArgvAndNonzeroExitArePreserved(t *testing.T) {
	state := &commandServer{logs: "out+err", exit: 7}
	runner := runnerFixture(t, state)
	argv := []string{"printf", "%s", "space value", "$(touch injection)", "semi;colon"}
	result, err := runner.run(context.Background(), argv, "/workspace", 1024)
	require.NoError(t, err)
	require.Equal(t, 7, result.ExitCode)
	require.Equal(t, "out+err", string(result.Output))
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, argv, state.request.Argv)
	require.Empty(t, state.request.Command)
	require.True(t, state.request.Background)
	require.Equal(t, 1, state.starts)
}
func TestCancelledCommandIsInterruptedUsingIndependentContext(t *testing.T) {
	state := &commandServer{running: true, logs: "prefix"}
	runner := runnerFixture(t, state)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := runner.run(ctx, []string{"sleep", "10"}, "/workspace", 1024)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, 1, state.interrupts)
	require.False(t, state.running)
	require.Equal(t, 1, state.starts)
}
func TestStopUnconfirmedAndMissingIDAreNotSuccess(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			state := &commandServer{running: true, ignoreInterrupt: true, missingID: missing}
			runner := runnerFixture(t, state)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			_, err := runner.run(ctx, []string{"sleep", "10"}, "/workspace", 1024)
			require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
			var unconfirmed *sandbox.UnconfirmedError
			require.ErrorAs(t, err, &unconfirmed)
			require.Equal(t, "sandbox-1", unconfirmed.SandboxID)
		})
	}
}
func TestOutputLimitStopsActualCommand(t *testing.T) {
	state := &commandServer{running: true, logs: strings.Repeat("x", 128)}
	runner := runnerFixture(t, state)
	result, err := runner.run(context.Background(), []string{"yes"}, "/workspace", 16)
	require.ErrorIs(t, err, sandbox.ErrOutputLimit)
	require.LessOrEqual(t, len(result.Output), 16)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, 1, state.interrupts)
	require.False(t, state.running)
}

func TestReviewHTTP408IsUnknownCommandOutcome(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(408)
		_, _ = fmt.Fprint(w, `{"code":"timeout","message":"upstream response timeout"}`)
	}))
	defer server.Close()
	runner := &commandRunner{sandboxID: "s", client: sdk.NewExecdClient(server.URL, ""), poll: time.Millisecond, cleanupTimeout: time.Millisecond, requestTimeout: time.Second}
	_, err := runner.run(context.Background(), []string{"touch", "side-effect"}, "/workspace", 1024)
	require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
}

func TestCodingAdapterFailedInterruptPreservesIdentity(t *testing.T) {
	state := &commandServer{running: true, ignoreInterrupt: true}
	runner := runnerFixture(t, state)
	files, _ := localFilesystem(t, 1024, 100)
	e := &environment{provider: &provider{options: Options{OperationTimeout: time.Second, MaxOutputBytes: 1024}}, runner: runner, files: files}
	w, err := coding.New(files, e, coding.WithCommandTimeout(200*time.Millisecond))
	require.NoError(t, err)
	for _, tool := range w.Tools() {
		if tool.Name() == "run_command" {
			out, err := tool.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]interface{}{"command": "sleep 60"}})
			require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
			require.NotNil(t, out)
			require.False(t, out.Success)
			var uncertain *sandbox.UnconfirmedError
			require.ErrorAs(t, err, &uncertain)
			require.Equal(t, "command-1", uncertain.CommandID)
		}
	}
	require.ErrorIs(t, e.check(), sandbox.ErrUnconfirmed)
	_, err = e.Run(context.Background(), env.Command{Argv: []string{"touch", "another"}})
	require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, 1, state.starts)
	require.True(t, state.running)
}
