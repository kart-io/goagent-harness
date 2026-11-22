package main

import (
	"encoding/json"
	"github.com/kart-io/goagent/llm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kart-io/goagent-harness/examples/planning-agent/internal/memoryeval"
	"github.com/stretchr/testify/require"
)

func TestDefaultPlanAndScriptedMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.json")
	raw := `[{"question_id":"q","question":"coffee?","answer":"tea","question_date":"2023/05/23 (Tue) 02:21","haystack_session_ids":["s","s2"],"haystack_dates":["2023/05/20 (Sat) 02:21","2023/05/20 (Sat) 02:25"],"haystack_sessions":[[{"role":"user","content":"coffee"}],[{"role":"user","content":"tea"}]]}]`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0600))
	plan := filepath.Join(dir, "plan")
	require.NoError(t, run(t.Context(), []string{"-dataset", path, "-out", plan}))
	files, err := os.ReadDir(plan)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "manifest.json", files[0].Name())
	out := filepath.Join(dir, "smoke")
	require.NoError(t, run(t.Context(), []string{"-dataset", path, "-out", out, "-backend", "scripted", "-repeats", "1"}))
	data, err := os.ReadFile(filepath.Join(out, "summary.json"))
	require.NoError(t, err)
	var s memoryeval.Summary
	require.NoError(t, json.Unmarshal(data, &s))
	require.Equal(t, 3, s.Completed)
	require.Zero(t, s.Failed)
}

func TestLiveSubmissionDoesNotFollowRedirect(t *testing.T) {
	var initial, target atomic.Int64
	next := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { target.Add(1) }))
	defer next.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initial.Add(1)
		http.Redirect(w, r, next.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	t.Setenv("OPENAI_BASE_URL", origin.URL)
	t.Setenv("OPENAI_API_KEY", "offline")
	client, err := service("offline")
	require.NoError(t, err)
	_, err = llm.Chat(t.Context(), client, []llm.Message{llm.UserMessage("test")})
	require.Error(t, err)
	require.EqualValues(t, 1, initial.Load())
	require.Zero(t, target.Load())
}
