package main

import (
	"encoding/json"
	"os"
	"path/filepath"
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
