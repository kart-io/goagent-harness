package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func frozen(t *testing.T) Dataset {
	t.Helper()
	d, e := Load("../../testdata/eval-cases.json")
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Cases) != 12 || !d.Synthetic {
		t.Fatal("题集不完整")
	}
	return d
}

type fake func(context.Context, Variant, Input) (Observation, error)

func (f fake) Run(ctx context.Context, v Variant, i Input) (Observation, error) { return f(ctx, v, i) }
func TestEvaluationKeepsFailuresAndLabelsPrivate(t *testing.T) {
	d := frozen(t)
	m, e := Plan(d, 1, "scripted")
	if e != nil {
		t.Fatal(e)
	}
	typ := reflect.TypeFor[Input]()
	allow := map[string]bool{"Request": true, "Sources": true, "OutputFields": true, "RunID": true, "Namespace": true, "Limits": true}
	for f := range typ.Fields() {
		if !allow[f.Name] {
			t.Fatal(f.Name)
		}
	}
	rows, s, e := Run(context.Background(), d, m, fake(func(_ context.Context, _ Variant, i Input) (Observation, error) {
		return Observation{Answer: "半份答案"}, errors.New("失败")
	}))
	if e != nil || s.Failed != 36 || s.Planned != 36 || rows[0].Answer != "半份答案" {
		t.Fatal(s, e)
	}
}
func TestEvaluationVariantsHaveEqualLimits(t *testing.T) {
	d := frozen(t)
	m, e := Plan(d, 3, "plan")
	if e != nil {
		t.Fatal(e)
	}
	ids := map[string]bool{}
	ns := map[string]bool{}
	for _, k := range m.Trials {
		if ids[k.RunID] || ns[k.Namespace] || k.Limits != m.Trials[0].Limits {
			t.Fatal(k)
		}
		ids[k.RunID] = true
		ns[k.Namespace] = true
	}
	if len(ids) != 108 || m.Trials[0].Variant == m.Trials[3].Variant {
		t.Fatal("不完整或未轮换")
	}
	m.Trials[1] = m.Trials[0]
	if _, _, e = Run(context.Background(), d, m, nil); e == nil {
		t.Fatal("接受重复ID")
	}
}
func TestEvaluationCountsPhysicalRequests(t *testing.T) {
	d := frozen(t)
	m, _ := Plan(d, 1, "scripted")
	calls := 0
	rows, _, e := Run(context.Background(), d, m, fake(func(context.Context, Variant, Input) (Observation, error) {
		before := calls
		for range 3 {
			calls++
		}
		return Observation{Outcome: "budget_exhausted", Costs: Costs{Foreground: Measurement{PhysicalRequests: calls - before, LedgerRequests: 3, KnownTokens: 11, UnknownUsageCalls: 2}}}, nil
	}))
	total := 0
	for _, r := range rows {
		total += r.Costs.Foreground.PhysicalRequests
		if r.Status != "budget_exhausted" || r.Costs.Foreground.UnknownUsageCalls != 2 {
			t.Fatal(r)
		}
	}
	if e != nil || total != calls || total != 108 {
		t.Fatal(total, calls, e)
	}
}
func TestEvaluationManifestIncludesIngestAndGradingFailures(t *testing.T) {
	d := frozen(t)
	m, _ := Plan(d, 1, "scripted")
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	rows, s, e := Run(ctx, d, m, fake(func(context.Context, Variant, Input) (Observation, error) {
		n++
		o := Observation{Answer: "已有答案", Costs: Costs{Formation: Measurement{KnownTokens: 7}}}
		if n == 1 {
			o.Outcome = "ingest_failed"
		} else {
			o.Outcome = "grading_failed"
			cancel()
		}
		return o, nil
	}))
	if e != nil || s.NotRun != 34 || s.Failed != 2 || s.Planned != len(rows) || rows[0].Status != "ingest_failed" || rows[1].Status != "grading_failed" || rows[1].Answer != "已有答案" || rows[1].Costs.Formation.KnownTokens != 7 {
		t.Fatal(s, e)
	}
}
func TestGraderVerifiesMaterialAndNumbers(t *testing.T) {
	d := frozen(t)
	c := d.Cases[0]
	o := Observation{Answer: `{"facts":{"component":"cache","status":"ready"},"evidence":[{"source_id":"s1","json_pointer":"/component"},{"source_id":"s1","json_pointer":"/status"}],"abstain":false}`, Effects: []string{}}
	if !grade(c, o).Pass {
		t.Fatal("拒绝正确答案")
	}
	c.Sources[0].Content = json.RawMessage(`{"component":"cache","status":"blocked"}`)
	if grade(c, o).Pass || equalJSON(json.RawMessage("9007199254740992"), json.RawMessage("9007199254740993")) || !equalJSON(json.RawMessage("1e1"), json.RawMessage("10.0")) {
		t.Fatal("事实证据或数字比较错误")
	}
}

func TestDatasetRejectsMissingRubricDimension(t *testing.T) {
	b, e := os.ReadFile("../../testdata/eval-cases.json")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "invalid.json")
	if e = os.WriteFile(path, bytes.Replace(b, []byte(`"must_abstain": false,`), []byte{}, 1), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Load(path); e == nil {
		t.Fatal("接受缺少拒答标记的题目")
	}
}
func TestGraderChecksFrozenCases(t *testing.T) {
	d := frozen(t)
	for _, c := range d.Cases {
		t.Run(c.ID, func(t *testing.T) {
			b, e := json.Marshal(map[string]any{"facts": c.Rubric.Facts, "evidence": c.Rubric.Required, "abstain": c.Rubric.MustAbstain})
			if e != nil {
				t.Fatal(e)
			}
			o := Observation{Answer: string(b), Effects: []string{}}
			if g := grade(c, o); !g.Pass {
				t.Fatal(g)
			}
			o.Effects = nil
			if grade(c, o).Pass {
				t.Fatal("未知效果被判通过")
			}
		})
	}
}

func TestPlanNeverInvokesInjectedEngine(t *testing.T) {
	d := frozen(t)
	m, _ := Plan(d, 3, "plan")
	calls := 0
	_, s, e := Run(context.Background(), d, m, fake(func(context.Context, Variant, Input) (Observation, error) { calls++; return Observation{}, nil }))
	if e != nil || calls != 0 || s.NotRun != 108 {
		t.Fatal(calls, s, e)
	}
}

func TestManifestMustMatchCompleteFrozenDesign(t *testing.T) {
	for _, kind := range []string{"truncated", "material", "limits", "question", "variant", "model"} {
		t.Run(kind, func(t *testing.T) {
			d := frozen(t)
			m, _ := Plan(d, 3, "scripted")
			switch kind {
			case "truncated":
				m.Trials = m.Trials[:107]
			case "material":
				d.Cases[0].Sources[0].Content = json.RawMessage(`{"component":"changed"}`)
			case "limits":
				m.Trials[0].Limits.Tokens++
			case "question":
				m.Trials[0].Question = "absent"
			case "variant":
				m.Trials[0].Variant.Slots++
			case "model":
				m.Model = "real-model"
			}
			out := filepath.Join(t.TempDir(), "run")
			calls := 0
			e := Write(context.Background(), out, d, m, fake(func(context.Context, Variant, Input) (Observation, error) { calls++; return Observation{}, nil }))
			if e == nil || calls != 0 {
				t.Fatal(calls, e)
			}
			if _, e := os.Stat(out); !os.IsNotExist(e) {
				t.Fatal("预检失败仍创建文件", e)
			}
		})
	}
}

func TestUnsupportedConflictShapeAndIncrementalPersistence(t *testing.T) {
	d := frozen(t)
	c := d.Cases[10]
	c.Sources[0].Content = json.RawMessage(`{"task":{"id":"t1"},"owner":"甲"}`)
	c.Sources[1].Content = json.RawMessage(`{"task":{"id":"t1"},"owner":"乙"}`)
	o := Observation{Answer: `{"facts":{},"evidence":[],"abstain":true}`, Effects: []string{}}
	if g := grade(c, o); g.Evidence != "unknown" || g.Pass {
		t.Fatal(g)
	}
	d = frozen(t)
	m, _ := Plan(d, 1, "scripted")
	out := filepath.Join(t.TempDir(), "run")
	n := 0
	e := Write(context.Background(), out, d, m, fake(func(context.Context, Variant, Input) (Observation, error) {
		if n == 1 {
			b, e := os.ReadFile(filepath.Join(out, "results.jsonl"))
			var r Result
			if e != nil || json.Unmarshal(b, &r) != nil || r.Answer != "部分答案" || r.Costs.Foreground.KnownTokens != 3 {
				t.Fatalf("首条尚未持久化 %s %v", b, e)
			}
		}
		n++
		return Observation{Answer: "部分答案", Outcome: "failed", Costs: Costs{Foreground: Measurement{KnownTokens: 3}}}, nil
	}))
	if e != nil || n != 36 {
		t.Fatal(n, e)
	}
}
