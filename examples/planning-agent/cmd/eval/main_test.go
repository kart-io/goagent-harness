package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/examples/planning-agent/internal/eval"
)

func TestDefaultPlanWithoutModel(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	args := []string{"-dataset", "../../testdata/eval-cases.json", "-out", out}
	if e := run(context.Background(), args); e != nil {
		t.Fatal(e)
	}
	for _, f := range []string{"manifest.json", "results.jsonl", "summary.json"} {
		if _, e := os.Stat(filepath.Join(out, f)); e != nil {
			t.Fatal(e)
		}
	}
	if run(context.Background(), args) == nil || run(context.Background(), append(args, "-backend", "live")) == nil {
		t.Fatal("缺少配置或重复目录应失败")
	}
}

func TestInterruptWritesAllPlannedTrialTerminals(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "eval")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("构建 CLI：%v\n%s", err, output)
	}
	received, release := make(chan struct{}, 1), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("意外的方法 %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case received <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	out := filepath.Join(t.TempDir(), "run")
	cmd := exec.Command(binary, "-dataset", "../../testdata/eval-cases.json", "-out", out, "-backend", "live", "-model", "fixture", "-base-url", server.URL, "-repeats", "1")
	cmd.Env = append(os.Environ(), "OPENAI_API_KEY=local-fixture")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	select {
	case <-received:
	case err := <-done:
		t.Fatalf("首次请求前退出：%v\n%s", err, stderr.String())
	case <-time.After(20 * time.Second):
		t.Fatal("等待首次本地 POST 超时")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(stderr.String(), "context canceled") {
			t.Errorf("取消应作为错误返回：%v\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SIGINT 后未完成收尾")
	}
	load := func(name string, target any) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, target); err != nil {
			t.Fatal(err)
		}
	}
	var manifest eval.Manifest
	load("manifest.json", &manifest)
	if len(manifest.Trials) != 36 {
		t.Fatalf("计划项数 %d", len(manifest.Trials))
	}
	data, err := os.ReadFile(filepath.Join(out, "results.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	seen := map[string]bool{}
	var rows []eval.Result
	for {
		var row eval.Result
		if err := decoder.Decode(&row); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if seen[row.RunID] {
			t.Fatalf("重复终态 %s", row.RunID)
		}
		seen[row.RunID] = true
		rows = append(rows, row)
	}
	if len(rows) != len(manifest.Trials) {
		t.Fatalf("终态 %d，计划 %d", len(rows), len(manifest.Trials))
	}
	for i, row := range rows {
		if !reflect.DeepEqual(row.Trial, manifest.Trials[i]) {
			t.Fatalf("第 %d 项键与计划不符", i)
		}
		want := "not_run"
		if i == 0 {
			want = "failed"
			cost := row.Costs.Foreground
			if cost.PhysicalRequests != 1 || cost.LedgerRequests != 1 || cost.UnknownUsageCalls != 1 || cost.UnknownCostCalls != 1 {
				t.Errorf("取消丢失费用：%+v", cost)
			}
		}
		if row.Status != want {
			t.Errorf("第 %d 项终态 %s，期望 %s", i, row.Status, want)
		}
	}
	var summary eval.Summary
	load("summary.json", &summary)
	if summary.Planned != 36 || summary.Failed != 1 || summary.NotRun != 35 || summary.Completed != 0 {
		t.Fatalf("汇总丢失分母：%+v", summary)
	}
}
