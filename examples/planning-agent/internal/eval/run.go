package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Limits specifies shared caps for a single root trial.
type Limits struct {
	Requests      int `json:"requests"`
	Tokens        int `json:"tokens"`
	ContextTokens int `json:"context_tokens"`
}

// Variant specifies the runner and worker concurrency.
type Variant struct {
	Name  string `json:"name"`
	Slots int    `json:"slots"`
}

// Input contains only public material and execution parameters.
type Input struct {
	Request          string
	Sources          []Source
	OutputFields     []string
	RunID, Namespace string
	Limits           Limits
}

// Measurement separates physical requests, ledger admissions and partial usage.
type Measurement struct {
	PhysicalRequests  int     `json:"physical_requests"`
	LedgerRequests    int     `json:"ledger_requests"`
	KnownTokens       int64   `json:"known_tokens"`
	UnknownUsageCalls int     `json:"unknown_usage_calls"`
	KnownCostUSD      float64 `json:"known_cost_usd"`
	UnknownCostCalls  int     `json:"unknown_cost_calls"`
}

// Costs reports independent stages including unsuccessful work.
type Costs struct {
	Formation  Measurement `json:"formation"`
	Foreground Measurement `json:"foreground"`
	Judge      Measurement `json:"judge"`
}

// Observation is the final answer, observed effects, outcome and costs even on errors.
type Observation struct {
	Answer  string   `json:"answer"`
	Effects []string `json:"effects"`
	Costs   Costs    `json:"costs"`
	Outcome string   `json:"outcome"`
}

// Engine executes one isolated trial without access to private grading labels.
type Engine interface {
	Run(context.Context, Variant, Input) (Observation, error)
}

// Trial identifies one planned variant, question and repeat.
type Trial struct {
	Experiment    string  `json:"experiment"`
	Variant       Variant `json:"variant"`
	Case          string  `json:"case"`
	Question      string  `json:"question"`
	Repeat        int     `json:"repeat"`
	RunID         string  `json:"run_id"`
	Namespace     string  `json:"namespace"`
	Limits        Limits  `json:"limits"`
	RubricVersion string  `json:"rubric_version"`
}

// Manifest fixes trial identities and settings before any execution.
type Manifest struct {
	Backend        string  `json:"backend"`
	Synthetic      bool    `json:"synthetic"`
	DatasetVersion string  `json:"dataset_version"`
	Digest         string  `json:"dataset_sha256"`
	Model          string  `json:"model"`
	Temperature    float64 `json:"temperature"`
	PromptVersion  string  `json:"prompt_version"`
	Repeats        int     `json:"repeats"`
	ResultsPath    string  `json:"results_path"`
	SummaryPath    string  `json:"summary_path"`
	Trials         []Trial `json:"trials"`
}

var variants = [...]Variant{{"direct", 1}, {"planning1", 1}, {"planning4", 4}}

func sharedLimits() Limits { return Limits{32, 32768, 8192} }

// Plan preallocates the complete comparison with rotating execution order.
func Plan(d Dataset, repeats int, backend string) (Manifest, error) {
	if repeats < 1 || !slices.Contains([]string{"scripted", "plan", "live"}, backend) {
		return Manifest{}, fmt.Errorf("无效的重复次数或后端")
	}
	m := Manifest{Backend: backend, Synthetic: d.Synthetic, DatasetVersion: d.Version, Digest: d.digest, Model: "offline-none", PromptVersion: "planning-prompt-v1", Repeats: repeats, ResultsPath: "results.jsonl", SummaryPath: "summary.json"}
	exp := uuid.NewString()
	limits := sharedLimits()
	for r := range repeats {
		for n, c := range d.Cases {
			for slot := range variants {
				id := uuid.NewString()
				v := variants[(slot+n+r)%len(variants)]
				m.Trials = append(m.Trials, Trial{exp, v, c.ID, c.ID, r + 1, id, "trial-" + uuid.NewString(), limits, c.Rubric.Version})
			}
		}
	}
	return m, nil
}

// Result contains exactly one terminal record per planned trial.
type Result struct {
	Trial
	Observation
	Status   string        `json:"status"`
	Grade    *Grade        `json:"grade,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Summary counts all planned trials in the denominator.
type Summary struct {
	Planned     int     `json:"planned"`
	Completed   int     `json:"completed"`
	Failed      int     `json:"failed"`
	NotRun      int     `json:"not_run"`
	Passed      int     `json:"passed"`
	SuccessRate float64 `json:"success_rate"`
}

func (s *Summary) add(row Result) {
	s.Planned++
	switch row.Status {
	case "completed":
		s.Completed++
		if row.Grade != nil && row.Grade.Pass {
			s.Passed++
		}
	case "not_run":
		s.NotRun++
	default:
		s.Failed++
	}
	s.SuccessRate = float64(s.Passed) / float64(s.Planned)
}

// Run executes planned trials and retains partial results after failures or cancellation.
func Run(ctx context.Context, d Dataset, m Manifest, engine Engine) ([]Result, Summary, error) {
	if err := validate(d, m, engine); err != nil {
		return nil, Summary{}, err
	}
	rows := make([]Result, 0, len(m.Trials))
	s, err := runTrials(ctx, d, m, engine, func(row Result) error { rows = append(rows, row); return nil })
	return rows, s, err
}

func validate(d Dataset, m Manifest, engine Engine) error {
	current, err := d.fingerprint()
	if err != nil || current != d.snapshot || d.digest == "" || m.Digest != d.digest || m.DatasetVersion != d.Version || m.Synthetic != d.Synthetic {
		return fmt.Errorf("题集摘要或版本不匹配")
	}
	if m.PromptVersion == "" || math.IsNaN(m.Temperature) || math.IsInf(m.Temperature, 0) || m.ResultsPath != "results.jsonl" || m.SummaryPath != "summary.json" {
		return fmt.Errorf("运行元数据无效")
	}
	if host, ok := engine.(interface{ validateManifest(Manifest) error }); ok {
		if err := host.validateManifest(m); err != nil {
			return err
		}
	}
	switch m.Backend {
	case "plan", "scripted":
		if m.Model != "offline-none" || m.Temperature != 0 || m.PromptVersion != "planning-prompt-v1" {
			return fmt.Errorf("离线后端模型或提示配置无效")
		}
	case "live":
		if engine == nil || strings.TrimSpace(m.Model) == "" || m.Model == "offline-none" {
			return fmt.Errorf("live 需要配置宿主 Engine 与模型")
		}
	default:
		return fmt.Errorf("未知后端")
	}
	group := len(d.Cases) * len(variants)
	if group == 0 || m.Repeats < 1 || len(m.Trials)%group != 0 || len(m.Trials)/group != m.Repeats {
		return fmt.Errorf("试验清单不完整")
	}
	ids, names := map[string]bool{}, map[string]bool{}
	experiment := m.Trials[0].Experiment
	for n, t := range m.Trials {
		r := n / group
		cIndex := n % group / len(variants)
		c := d.Cases[cIndex]
		v := variants[(n%len(variants)+cIndex+r)%len(variants)]
		if experiment == "" || t.Experiment != experiment || t.Repeat != r+1 || t.Case != c.ID || t.Question != c.ID || t.Variant != v || t.Limits != sharedLimits() || t.RubricVersion != c.Rubric.Version {
			return fmt.Errorf("试验设计不匹配 %d", n)
		}
		if t.RunID == "" || t.Namespace == "" || ids[t.RunID] || names[t.Namespace] {
			return fmt.Errorf("无效或重复试验 %s", t.RunID)
		}
		ids[t.RunID] = true
		names[t.Namespace] = true
	}
	return nil
}

func runTrials(ctx context.Context, d Dataset, m Manifest, engine Engine, record func(Result) error) (Summary, error) {
	cases := map[string]Case{}
	for _, c := range d.Cases {
		cases[c.ID] = c
	}
	var s Summary
	for _, t := range m.Trials {
		row := Result{Trial: t, Status: "not_run"}
		if m.Backend != "plan" && engine != nil && ctx.Err() == nil {
			row = execute(ctx, cases[t.Case], t, engine)
		}
		if e := record(row); e != nil {
			return s, e
		}
		s.add(row)
	}
	return s, nil
}

func execute(ctx context.Context, c Case, t Trial, engine Engine) Result {
	src := slices.Clone(c.Sources)
	for i := range src {
		src[i].Content = slices.Clone(src[i].Content)
	}
	input := Input{c.Request, src, slices.Clone(c.OutputFields), t.RunID, t.Namespace, t.Limits}
	start := time.Now()
	o, e := engine.Run(ctx, t.Variant, input)
	row := Result{Trial: t, Observation: o, Duration: time.Since(start)}
	switch o.Outcome {
	case "ingest_failed", "grading_failed", "budget_exhausted", "timeout", "failed", "not_run":
		row.Status = o.Outcome
		return row
	}
	if errors.Is(e, context.DeadlineExceeded) {
		row.Status = "timeout"
		return row
	}
	if e != nil || (o.Outcome != "" && o.Outcome != "completed") {
		row.Status = "failed"
		return row
	}
	g := grade(c, o)
	row.Grade = &g
	row.Status = "completed"
	return row
}

// Scripted runs an offline extraction smoke test using only public fields.
type Scripted struct{}

// Run copies directly named source fields into a structured answer.
func (Scripted) Run(_ context.Context, _ Variant, i Input) (Observation, error) {
	facts := map[string]any{}
	refs := []Locator{}
	for _, k := range i.OutputFields {
		for _, s := range i.Sources {
			fields, _ := value(s.Content).(map[string]any)
			if v, ok := fields[k]; ok {
				facts[k] = v
				refs = append(refs, Locator{s.ID, "/" + k})
				break
			}
		}
	}
	b, e := json.Marshal(map[string]any{"facts": facts, "evidence": refs, "abstain": len(facts) == 0})
	return Observation{Answer: string(b), Effects: []string{}, Outcome: "completed"}, e
}

// Write creates an exclusive output directory and saves the manifest before running trials.
func Write(ctx context.Context, dir string, d Dataset, m Manifest, engine Engine) error {
	if err := validate(d, m, engine); err != nil {
		return err
	}
	if e := os.Mkdir(dir, 0700); e != nil {
		return e
	}
	save := func(name string, v any) error {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0600)
	}
	if e := save("manifest.json", m); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(dir, "results.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	enc := json.NewEncoder(f)
	byVariant := map[string]*Summary{}
	s, runErr := runTrials(ctx, d, m, engine, func(r Result) error {
		if e := enc.Encode(r); e != nil {
			return e
		}
		name := r.Variant.Name
		if byVariant[name] == nil {
			byVariant[name] = &Summary{}
		}
		byVariant[name].add(r)
		return nil
	})
	e = f.Close()
	if runErr != nil {
		return runErr
	}
	if e != nil {
		return e
	}
	return save("summary.json", struct {
		Summary
		ByVariant map[string]*Summary `json:"by_variant"`
	}{s, byVariant})
}
