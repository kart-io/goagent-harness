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
	path := writeResults(t, []Result{
		{InstanceID: "a", Mode: "gold", Run: 1, Resolved: true},
		{InstanceID: "a", Mode: "agent", Run: 1, AgentError: "exit status 1: [LLM_REQUEST] [llm] operation=request: 503"},
		{InstanceID: "b", Mode: "agent", Run: 1, AgentError: "exit status 1: 运行超时"},
		{InstanceID: "e", Mode: "agent", Run: 1, AgentError: "exit status 1: [LLM_REQUEST] request failed: Post \"https://x\": context deadline exceeded"},
		{InstanceID: "c", Mode: "agent", Run: 1, Error: "评测: context deadline exceeded"},
		{InstanceID: "d", Mode: "agent", Run: 1, Resolved: true},
	})
	done := doneRuns(path)
	for key, want := range map[string]bool{"a/gold/1": true, "a/agent/1": false, "b/agent/1": true, "e/agent/1": true, "c/agent/1": false, "d/agent/1": true} {
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

func writeResults(t *testing.T, results []Result) string {
	t.Helper()
	var lines []string
	for _, r := range results {
		b, _ := json.Marshal(r)
		lines = append(lines, string(b))
	}
	path := filepath.Join(t.TempDir(), "results.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCategorize(t *testing.T) {
	pass, fail := map[string]string{"T": "PASS"}, map[string]string{"T": "FAIL"}
	cases := []struct {
		r           Result
		applyFailed bool
		want        string
	}{
		{Result{Resolved: true, AgentError: "运行超时"}, false, CatResolved},
		{Result{Error: "镜像: pull"}, false, CatEnv},
		{Result{Mode: "agent", AgentError: "[LLM_REQUEST] 503"}, false, CatModelService},
		{Result{Mode: "agent", AgentError: `[LLM_REQUEST] [llm] operation=request: LLM request failed: Post "https://api.minimaxi.com/v1/chat/completions": context deadline exceeded`}, false, CatAgentStopped},
		{Result{Mode: "agent", AgentError: "模型输出未完整结束：length", PatchBytes: 10}, false, CatAgentStopped},
		{Result{PatchBytes: 10}, true, CatApplyFailed},
		{Result{FailToPass: fail}, false, CatNoPatch},
		{Result{PatchBytes: 10, FailToPass: fail, PassToPass: pass}, false, CatWrongFix},
		{Result{PatchBytes: 10, FailToPass: pass, PassToPass: fail}, false, CatRegression},
	}
	for i, c := range cases {
		if got := categorize(c.r, c.applyFailed); got != c.want {
			t.Errorf("#%d = %s，期望 %s", i, got, c.want)
		}
	}
}

// 续跑重做的运行取最后一条；环境与模型服务失败不计入分母；gold 未通过的题单独列出。
func TestSummarize(t *testing.T) {
	path := writeResults(t, []Result{
		{InstanceID: "a", Mode: "gold", Run: 1, Resolved: true},
		{InstanceID: "c", Mode: "gold", Run: 1},
		{InstanceID: "a", Mode: "agent", Run: 1, AgentError: "[LLM_REQUEST] 503"},
		{InstanceID: "a", Mode: "agent", Run: 1, Resolved: true, Seconds: 100, ModelCalls: "20", Model: "minimax"},
		{InstanceID: "b", Mode: "agent", Run: 1, PatchBytes: 5, FailToPass: map[string]string{"T": "FAIL"}, Seconds: 300, ModelCalls: "40"},
		{InstanceID: "d", Mode: "agent", Run: 1, Error: "评测: 超时"},
		{InstanceID: "x", Mode: "agent", Run: 1, Resolved: true},
	})
	s, err := summarize(path, []Instance{{InstanceID: "a"}, {InstanceID: "b"}, {InstanceID: "c"}, {InstanceID: "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Model != "minimax" || s.Runs != 2 || s.Resolved != 1 || s.Rate != 0.5 || s.AvgSeconds != 200 || s.AvgModelCalls != 30 {
		t.Errorf("汇总 = %+v", s)
	}
	if s.Categories[CatResolved] != 1 || s.Categories[CatWrongFix] != 1 || s.Categories[CatEnv] != 1 || s.Categories[CatModelService] != 0 {
		t.Errorf("分类 = %v", s.Categories)
	}
	if len(s.ExcludedByGold) != 1 || s.ExcludedByGold[0] != "c" {
		t.Errorf("gold 未通过 = %v", s.ExcludedByGold)
	}
}

func TestCompare(t *testing.T) {
	base := Summary{Instances: map[string]InstanceSummary{
		"a": {Runs: 1, Resolved: 1}, "b": {Runs: 1}, "c": {Runs: 2, Resolved: 1}, "gone": {Runs: 1, Resolved: 1},
	}}
	cur := Summary{Instances: map[string]InstanceSummary{
		"a": {Runs: 1}, "b": {Runs: 1, Resolved: 1}, "c": {Runs: 4, Resolved: 2}, "new": {Runs: 1},
	}}
	d := compare(base, cur)
	if strings.Join(d.Regressed, ",") != "a" || strings.Join(d.Improved, ",") != "b" {
		t.Errorf("差异 = %+v", d)
	}
}

// 官方镜像在评测时下载缺失依赖，网络超时的两种输出都应识别为下载失败。
func TestDownloadFailed(t *testing.T) {
	for _, log := range []string{
		`cmd/commandfuncs.go:35:2: github.com/aryann/difflib@v0.0.0-20210328193216-ff5ff6dc229b: Get "https://proxy.golang.org/github.com/aryann/difflib/@v/v0.0.0-20210328193216-ff5ff6dc229b.zip": net/http: TLS handshake timeout`,
		`go: golang.org/x/net@v0.0.0-20221004154528-8021a29435af: Get "https://proxy.golang.org/golang.org/x/net/@v/v0.0.0-20221004154528-8021a29435af.mod": net/http: TLS handshake timeout`,
	} {
		if !downloadFailed.MatchString("+ go test -v ./x\n" + log + "\nFAIL\n") {
			t.Errorf("未识别：%s", log)
		}
	}
	if downloadFailed.MatchString("go: downloading github.com/x/y v1.0.0\n--- FAIL: TestA (0.00s)\n") {
		t.Error("正常下载与测试失败不应识别为下载失败")
	}
}
