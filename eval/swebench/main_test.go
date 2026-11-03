package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 因模型服务出错中止的 Agent 运行重跑时重新做；评测环境出错的也重新做；其余结果跳过。
func TestDoneRunsSkipsModelFailures(t *testing.T) {
	results := []Result{
		{InstanceID: "a", Mode: "gold", Run: 1, Resolved: true},
		{InstanceID: "a", Mode: "agent", Run: 1, AgentError: "exit status 1: [LLM_REQUEST] [llm] operation=request: 503"},
		{InstanceID: "b", Mode: "agent", Run: 1, AgentError: "exit status 1: 运行超时"},
		{InstanceID: "c", Mode: "agent", Run: 1, Error: "评测: context deadline exceeded"},
		{InstanceID: "d", Mode: "agent", Run: 1, Resolved: true},
	}
	var lines []string
	for _, r := range results {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := doneRuns(path)
	for key, want := range map[string]bool{"a/gold/1": true, "a/agent/1": false, "b/agent/1": true, "c/agent/1": false, "d/agent/1": true} {
		if _, ok := done[key]; ok != want {
			t.Errorf("%s 已完成 = %v，期望 %v", key, ok, want)
		}
	}
}

func TestParseGoTest(t *testing.T) {
	log := "=== RUN   TestA\n--- PASS: TestA (0.00s)\n    --- FAIL: TestB/sub (0.01s)\n--- SKIP: TestC (0.00s)\n"
	got := pick(parseGoTest(log), []string{"TestA", "TestB/sub", "TestC", "TestD"})
	want := map[string]string{"TestA": "PASS", "TestB/sub": "FAIL", "TestC": "SKIP", "TestD": "MISSING"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s，期望 %s", k, got[k], v)
		}
	}
	if allPass(got) {
		t.Error("有失败时不应全部通过")
	}
}
