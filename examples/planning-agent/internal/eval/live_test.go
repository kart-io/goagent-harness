package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveDirectAndAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		limits Limits
		calls  int
	}{
		{"accepted", sharedLimits(), 1}, {"context", Limits{32, 32768, 8}, 0}, {"root tokens", Limits{32, 8, 8192}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := mocks.NewScriptedClient(mocks.TextReply("answer", `{"facts":{},"evidence":[],"abstain":true}`))
			engine := Live{Client: client, Model: "test-model", Root: t.TempDir()}
			obs, err := engine.Run(t.Context(), variants[0], Input{RunID: "run", Namespace: "ns", Request: "Question", Limits: test.limits})
			if test.calls == 0 {
				require.Error(t, err)
				require.Equal(t, "budget_exhausted", obs.Outcome)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, test.calls, client.Calls())
			require.Equal(t, test.calls, obs.Costs.Foreground.PhysicalRequests)
			require.Equal(t, test.calls, obs.Costs.Foreground.LedgerRequests)
		})
	}
}

func TestLivePlanningRolesUseSharedLedger(t *testing.T) {
	for _, v := range variants[1:] {
		t.Run(v.Name, func(t *testing.T) {
			const draft = `{"tasks":[{"id":"new:report","member_id":"reader","instruction":"read source","criteria":[{"id":"checked","description":"check answer","evidence_required":true}],"criterion_ids":["answer"],"goal_input_ids":["s1"],"outputs":[{"name":"report","media_type":"application/json","max_bytes":4096,"deliverable_id":"answer"}]}],"rationale":"extract"}`
			const artifacts = `{"artifacts":[{"name":"report","media_type":"application/json","deliverable_id":"answer","content":{"facts":{"n":1},"abstain":false,"evidence":[{"source_id":"s1","json_pointer":"/n"}]},"sources":[{"source_id":"s1","locator_kind":"json_pointer","locator":"/n"}]}]}`
			review := mocks.ScriptedRule{Name: "review", Respond: func(_ context.Context, req *llm.CompletionRequest) (mocks.ScriptedReply, error) {
				var in p.ReviewInput
				msg := req.Messages[len(req.Messages)-1].Content
				_, data, _ := strings.Cut(msg, "\n")
				if err := json.Unmarshal([]byte(data), &in); err != nil {
					return mocks.ScriptedReply{}, err
				}
				id := "checked"
				if in.Final {
					id = "answer"
				}
				b, err := json.Marshal(p.Review{SubjectDigest: in.SubjectDigest, Verdict: "accept", Reason: "supported", Criteria: []p.CriterionResult{{CriterionID: id, Status: "pass", EvidenceIDs: []string{in.Results[0].Artifacts[0].ID}}}})
				return mocks.ScriptedReply{Text: string(b)}, err
			}}
			for _, cap := range []int{32, 1} {
				t.Run(fmt.Sprint(cap), func(t *testing.T) {
					client := mocks.NewScriptedClient(mocks.TextReply("plan", draft), mocks.TextReply("execute", artifacts), review, review)
					engine := Live{Root: t.TempDir(), Model: "test-model", Client: client, Temperature: 0.25}
					input := Input{Request: "read n", Sources: []Source{{"s1", "Source", json.RawMessage(`{"n":1}`)}}, OutputFields: []string{"n"}, RunID: "run", Namespace: "ns", Limits: sharedLimits()}
					input.Limits.Requests = cap
					obs, err := engine.Run(t.Context(), v, input)
					if cap == 1 {
						require.Error(t, err)
						require.Equal(t, 1, client.Calls())
						require.Equal(t, "budget_exhausted", obs.Outcome)
					} else {
						require.NoError(t, err)
						require.Equal(t, 4, client.Calls())
						require.JSONEq(t, `{"facts":{"n":1},"abstain":false,"evidence":[{"source_id":"s1","json_pointer":"/n"}]}`, obs.Answer)
					}
					require.Equal(t, client.Calls(), obs.Costs.Foreground.PhysicalRequests)
					require.Equal(t, client.Calls(), obs.Costs.Foreground.LedgerRequests)
					for _, r := range client.Requests() {
						require.Equal(t, "test-model", r.Model)
						require.Equal(t, 0.25, r.Temperature)
						for _, msg := range r.Messages {
							for _, private := range []string{"expected_facts", "required_source_locators", "must_abstain", "expected_final_effects"} {
								require.NotContains(t, msg.Content, private)
							}
						}
					}
				})
			}
		})
	}
}

// 返回部分回复后中断，供应商未提供用量。
type brokenStream struct{ *mocks.ScriptedClient }

func (b brokenStream) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	ch := make(chan *llm.StreamChunk, 2)
	ch <- &llm.StreamChunk{Delta: "partial"}
	ch <- &llm.StreamChunk{Error: errors.New("broken stream")}
	close(ch)
	return ch, nil
}

func TestLiveRetainsFailedAndRetriedCalls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		client   llm.Client
		variant  Variant
		requests int
		unknown  int
		known    int64
		success  bool
	}{
		{"stream failure", brokenStream{mocks.NewScriptedClient()}, variants[0], 1, 1, 0, false},
		{"retry", mocks.NewScriptedClient(mocks.ScriptedRule{Name: "unavailable", Respond: func(context.Context, *llm.CompletionRequest) (mocks.ScriptedReply, error) {
			return mocks.ScriptedReply{}, &llm.ProviderError{StatusCode: 503, Err: errors.New("unavailable")}
		}}, mocks.TextReply("success", `{"facts":{},"evidence":[],"abstain":true}`)), variants[0], 2, 1, 2, true},
		{"invalid plan", mocks.NewScriptedClient(mocks.TextReply("not JSON", "invalid")), variants[1], 1, 0, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := Live{Client: tc.client, Root: t.TempDir(), Model: "model"}
			limits := sharedLimits()
			if tc.name == "invalid plan" {
				limits.Requests = 1
			}
			o, err := e.Run(t.Context(), tc.variant, Input{RunID: "run", Namespace: "ns", Request: "question", Limits: limits})
			if tc.success {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, Measurement{PhysicalRequests: tc.requests, LedgerRequests: tc.requests, KnownTokens: tc.known, UnknownUsageCalls: tc.unknown, UnknownCostCalls: tc.requests}, o.Costs.Foreground)
		})
	}
}

func TestLiveManifestMatchesActualConfiguration(t *testing.T) {
	d := frozen(t)
	m, err := Plan(d, 1, "live")
	require.NoError(t, err)
	client := mocks.NewScriptedClient()
	host := Live{Client: client, Root: t.TempDir(), Model: "test-model", Temperature: 0.25}
	require.Error(t, validate(d, m, host))
	m.Model, m.Temperature, m.PromptVersion = host.Model, host.Temperature, LivePromptVersion
	require.NoError(t, validate(d, m, host))
	for _, change := range []func(*Manifest){
		func(m *Manifest) { m.Model = "other" },
		func(m *Manifest) { m.Temperature = 0.5 },
		func(m *Manifest) { m.PromptVersion = "other" },
	} {
		modified := m
		change(&modified)
		require.Error(t, validate(d, modified, host))
	}
	offline, err := Plan(d, 1, "scripted")
	require.NoError(t, err)
	require.Error(t, validate(d, offline, host))
	require.Zero(t, client.Calls())
}

func TestLiveRejectsRedirectBeforeSecondPOST(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "local-fixture")
	var initial, redirected atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
		}
		if r.URL.Path == "/chat/completions" {
			initial.Add(1)
			http.Redirect(w, r, "/redirect/chat/completions", http.StatusTemporaryRedirect)
			return
		}
		redirected.Add(1)
		http.Error(w, "unexpected redirect", http.StatusBadRequest)
	}))
	defer server.Close()
	client, err := NewLiveOpenAIClient("fixture", 0, server.URL)
	require.NoError(t, err)
	e := Live{Client: client, Root: t.TempDir(), Model: "fixture"}
	d := frozen(t)
	trial := Trial{Variant: variants[0], RunID: "run", Namespace: "ns", Limits: Limits{1, 32768, 8192}}
	row := execute(t.Context(), d.Cases[0], trial, e)
	require.Contains(t, []string{"budget_exhausted", "failed"}, row.Status)
	require.EqualValues(t, 1, initial.Load())
	require.EqualValues(t, 0, redirected.Load())
	require.Equal(t, Measurement{PhysicalRequests: 1, LedgerRequests: 1, UnknownUsageCalls: 1, UnknownCostCalls: 1}, row.Costs.Foreground)
}

func TestLiveCancellationRetainsUnknownUsage(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "local-fixture")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := NewLiveOpenAIClient("fixture", 0, server.URL)
	require.NoError(t, err)
	host := Live{Client: client, Root: t.TempDir(), Model: "fixture"}
	row := execute(ctx, frozen(t).Cases[0], Trial{Variant: variants[0], RunID: "run", Namespace: "ns", Limits: sharedLimits()}, host)
	require.Equal(t, "failed", row.Status)
	require.EqualValues(t, 1, requests.Load())
	require.Equal(t, Measurement{PhysicalRequests: 1, LedgerRequests: 1, UnknownUsageCalls: 1, UnknownCostCalls: 1}, row.Costs.Foreground)
}
