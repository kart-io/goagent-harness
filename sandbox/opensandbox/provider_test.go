package opensandbox

import (
	"context"
	"encoding/json"
	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type lifecycleServer struct {
	mu          sync.Mutex
	create      sdk.CreateSandboxRequest
	deleted     bool
	creates     int
	endpoint    string
	helperFails bool
	auth        []string
}

func (s *lifecycleServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = append(s.auth, r.Header.Get("OPEN-SANDBOX-API-KEY"))
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
		s.creates++
		_ = json.NewDecoder(r.Body).Decode(&s.create)
		_ = json.NewEncoder(w).Encode(sdk.SandboxInfo{ID: "one", Metadata: s.create.Metadata, Image: s.create.Image, Status: sdk.SandboxStatus{State: sdk.StateRunning}})
	case r.Method == "DELETE" && r.URL.Path == "/v1/sandboxes/one":
		s.deleted = true
		w.WriteHeader(204)
	case r.URL.Path == "/v1/sandboxes/one":
		if s.deleted {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(sdk.SandboxInfo{ID: "one", Metadata: s.create.Metadata, Image: s.create.Image, Status: sdk.SandboxStatus{State: sdk.StateRunning}})
	case strings.Contains(r.URL.Path, "/endpoints/"):
		_ = json.NewEncoder(w).Encode(sdk.Endpoint{Endpoint: s.endpoint})
	case r.URL.Path == "/ping":
		w.WriteHeader(200)
	case r.Method == "POST" && r.URL.Path == "/files/upload":
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(204)
	case r.Method == "DELETE" && r.URL.Path == "/files":
		w.WriteHeader(204)
	case r.Method == "POST" && r.URL.Path == "/command":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"init\",\"text\":\"cmd\"}\n\ndata: {\"type\":\"execution_complete\"}\n\n")
	case strings.HasPrefix(r.URL.Path, "/command/status/"):
		code := int32(0)
		if s.helperFails {
			code = 127
		}
		_ = json.NewEncoder(w).Encode(sdk.CommandStatusResponse{ID: "cmd", ExitCode: &code})
	case strings.HasSuffix(r.URL.Path, "/logs"):
		text := "{\"protocol\":1}"
		w.Header().Set("EXECD-COMMANDS-TAIL-CURSOR", "14")
		if r.URL.Query().Get("cursor") == "0" {
			_, _ = io.WriteString(w, text)
		}
	default:
		http.NotFound(w, r)
	}
}
func providerFixture(t *testing.T, state *lifecycleServer) sandbox.Provider {
	t.Helper()
	server := httptest.NewServer(state)
	t.Cleanup(server.Close)
	if state.endpoint == "" {
		state.endpoint = server.URL
	}
	p, err := New(Options{Connection: sdk.ConnectionConfig{Domain: strings.TrimPrefix(server.URL, "http://"), Protocol: "http", APIKey: "secret", UseServerProxy: true}, PollInterval: time.Millisecond})
	require.NoError(t, err)
	return p
}
func TestProviderCreateConnectAndOwnedCleanup(t *testing.T) {
	state := &lifecycleServer{}
	p := providerFixture(t, state)
	req := sandbox.Request{Provider: "opensandbox", Scope: "scope", Image: "python-image", Network: true}
	created, err := p.Create(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "one", created.ID())
	require.NoError(t, created.Close(context.Background()))
	require.False(t, state.deleted)
	connected, err := p.Connect(context.Background(), "one", req)
	require.NoError(t, err)
	require.NoError(t, connected.Close(context.Background()))
	require.False(t, state.deleted)
	req.Scope = "other"
	_, err = p.Connect(context.Background(), "one", req)
	require.Error(t, err)
	require.NoError(t, created.Destroy(context.Background()))
	require.True(t, state.deleted)
	require.Equal(t, 1, state.creates)
	for _, key := range state.auth {
		require.Equal(t, "secret", key)
	}
}
func TestProviderRejectsUnsupportedBeforeAllocation(t *testing.T) {
	state := &lifecycleServer{}
	p := providerFixture(t, state)
	_, err := p.Create(context.Background(), sandbox.Request{Scope: "s", Image: "i"})
	require.ErrorIs(t, err, sandbox.ErrUnsupported)
	require.Zero(t, state.creates)
	retry := sdk.DefaultRetryConfig()
	_, err = New(Options{Connection: sdk.ConnectionConfig{UseServerProxy: true, Retry: &retry}})
	require.Error(t, err)
}
func TestProviderInitializationFailurePreservesPartialForCleanup(t *testing.T) {
	for _, state := range []*lifecycleServer{{helperFails: true}, {endpoint: "http://untrusted.invalid"}} {
		p := providerFixture(t, state)
		e, err := p.Create(context.Background(), sandbox.Request{Scope: "s", Image: "i", Network: true})
		require.Error(t, err)
		require.NotNil(t, e)
		require.Equal(t, "one", e.ID())
		require.NoError(t, e.Destroy(context.Background()))
		require.True(t, state.deleted)
	}
}

func TestMetadataFitsOpenSandboxLabels(t *testing.T) {
	p := providerFixture(t, &lifecycleServer{}).(*provider)
	metadata := p.metadata(sandbox.Request{Scope: `["preset","component"]`, Image: "registry/image@sha256:123", Network: true})
	for key, value := range metadata {
		require.LessOrEqual(t, len(value), 63, key)
		require.Regexp(t, `^[a-zA-Z0-9]([a-zA-Z0-9_.-]*[a-zA-Z0-9])?$`, value, key)
	}
}
