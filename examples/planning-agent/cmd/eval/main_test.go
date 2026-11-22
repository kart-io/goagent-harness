package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPlanWithoutModel(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run")
	args := []string{"-dataset", "../../testdata/eval-cases.json", "-out", out}
	if e := run(args); e != nil {
		t.Fatal(e)
	}
	for _, f := range []string{"manifest.json", "results.jsonl", "summary.json"} {
		if _, e := os.Stat(filepath.Join(out, f)); e != nil {
			t.Fatal(e)
		}
	}
	if run(args) == nil || run(append(args, "-backend", "live")) == nil {
		t.Fatal("缺少配置或重复目录应失败")
	}
}
