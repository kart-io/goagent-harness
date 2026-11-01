package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPResponseLimitAndRedirect(t *testing.T) {
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++; w.WriteHeader(200) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer origin.Close()
	transport := &responseLimitTransport{base: http.DefaultTransport, limit: 16}
	client := http.Client{Transport: transport}
	response, err := client.Get(origin.URL)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	require.ErrorIs(t, err, errResponseLimit)
	require.LessOrEqual(t, len(data), 17)
	p := providerFixture(t, &lifecycleServer{}).(*provider)
	response, err = p.http.Get(origin.URL + "/redirect")
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, 307, response.StatusCode)
	require.Zero(t, targetCalls)
}
func TestUnknownCreateRemainsReconciliableWithoutID(t *testing.T) {
	var owner string
	state := &lifecycleServer{}
	// Exercise the unknown-result path with a response that cannot be decoded;
	// the server has nevertheless recorded the exact owner token.
	p := providerFixture(t, state).(*provider)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"version":"1.2.0"}`)
			return
		}
		if r.Method == "POST" {
			state.mu.Lock()
			defer state.mu.Unlock()
			require.NoError(t, json.NewDecoder(r.Body).Decode(&state.create))
			owner = state.create.Metadata["harness.owner"]
			_, _ = io.WriteString(w, "invalid-json")
			return
		}
		if r.URL.Path == "/v1/sandboxes" {
			_, _ = io.WriteString(w, `{"items":[],"pagination":{}}`)
			return
		}
		state.ServeHTTP(w, r)
	}))
	defer broken.Close()
	p.lifecycle = sdk.NewLifecycleClientWithCache(broken.URL+"/v1", "", nil)
	e, err := p.Create(context.Background(), sandbox.Request{Scope: "s", Image: "i", Network: true})
	require.ErrorIs(t, err, sandbox.ErrUnconfirmed)
	require.NotNil(t, e)
	require.NotEmpty(t, owner)
	err = e.Destroy(context.Background())
	require.True(t, errors.Is(err, sandbox.ErrUnconfirmed), "empty listing cannot prove a timed-out create never allocated")
}
