package memoryeval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	ae "github.com/kart-io/goagent/errors"
	"os"
	"path/filepath"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/memory/memeval"
)

// Config freezes equal per-stage admission limits for every variant.
type Config struct {
	Repeats         int    `json:"repeats"`
	Runes           int    `json:"context_runes"`
	CoreRunes       int    `json:"core_runes"`
	ContextTokens   int    `json:"context_tokens"`
	OutputTokens    int    `json:"output_tokens"`
	Requests        int    `json:"request_limit"`
	Tokens          int    `json:"reserved_token_limit"`
	FormationModel  string `json:"formation_model"`
	ForegroundModel string `json:"foreground_model"`
	JudgeModel      string `json:"judge_model"`
}

// DefaultConfig returns the offline experiment defaults.
func DefaultConfig() Config {
	return Config{3, 2048, 512, 32768, 1024, 12, 131072, "scripted", "scripted", "scripted"}
}

// Clients supplies host-configured single-attempt providers. Runner retries are metered.
type Clients struct{ Formation, Foreground, Judge llm.Client }

// Key identifies a preregistered question trial.
type Key struct {
	Experiment string `json:"experiment"`
	Variant    string `json:"variant"`
	Case       string `json:"case"`
	Question   string `json:"question"`
	Namespace  string `json:"namespace"`
	Repeat     int    `json:"repeat"`
}

// Manifest fixes trial order, input identity, settings and output locations.
type Manifest struct {
	Config        Config  `json:"config"`
	DatasetSHA256 string  `json:"dataset_sha256"`
	PromptVersion string  `json:"prompt_version"`
	JudgeVersion  string  `json:"judge_version"`
	Temperature   float64 `json:"temperature"`
	ResultsFile   string  `json:"results_file"`
	SummaryFile   string  `json:"summary_file"`
	Trials        []Key   `json:"trials"`
}

// Row preserves the answer even if grading fails.
type Row struct {
	Key
	Status       string        `json:"status"`
	Answer       string        `json:"answer"`
	Error        string        `json:"error,omitempty"`
	Correct      bool          `json:"correct"`
	EvidenceHit  bool          `json:"evidence_hit"`
	ContextRunes int           `json:"context_runes"`
	Foreground   Cost          `json:"foreground"`
	Judge        Cost          `json:"judge"`
	Elapsed      time.Duration `json:"elapsed_ns"`
}

// Formation records the shared extraction expense once per case and repeat.
type Formation struct {
	Case   string `json:"case"`
	Repeat int    `json:"repeat"`
	Cost   Cost   `json:"cost"`
	Error  string `json:"error,omitempty"`
}

// Summary reports success against every planned question.
type Summary struct {
	Planned     int         `json:"planned"`
	Completed   int         `json:"completed"`
	Failed      int         `json:"failed"`
	NotRun      int         `json:"not_run"`
	Correct     int         `json:"correct"`
	SuccessRate float64     `json:"success_rate"`
	Formation   []Formation `json:"formation"`
}

func makeManifest(cases []memeval.Case, c Config) (Manifest, error) {
	if len(cases) == 0 || c.Repeats < 1 || c.Runes <= 0 || c.CoreRunes < 0 || c.CoreRunes > c.Runes || c.Requests < 1 || c.OutputTokens < 1 || c.ContextTokens <= c.OutputTokens || c.Tokens <= 0 || c.FormationModel == "" || c.ForegroundModel == "" || c.JudgeModel == "" {
		return Manifest{}, fmt.Errorf("配置或用例无效")
	}
	data, err := json.Marshal(cases)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{Config: c, DatasetSHA256: fmt.Sprintf("%x", sha256.Sum256(data)), PromptVersion: "memeval-answer-v1", JudgeVersion: "memeval-longmemeval-v1", ResultsFile: "results.jsonl", SummaryFile: "summary.json"}
	seen := map[string]bool{}
	for _, cs := range cases {
		if cs.ID == "" || seen[cs.ID] || len(cs.Questions) == 0 {
			return m, fmt.Errorf("用例为空或重复")
		}
		seen[cs.ID] = true
		qs := map[string]bool{}
		for _, q := range cs.Questions {
			if q.ID == "" || qs[q.ID] {
				return m, fmt.Errorf("题号为空或重复")
			}
			qs[q.ID] = true
		}
	}
	labels := []string{"off", "legacy", "layout"}
	for r := 1; r <= c.Repeats; r++ {
		for i, cs := range cases {
			for j := range labels {
				v := labels[(j+r-1)%3]
				for n, q := range cs.Questions {
					m.Trials = append(m.Trials, Key{"memory-layout", v, cs.ID, q.ID, fmt.Sprintf("r%d-c%d-%s-q%d", r, i, v, n), r})
				}
			}
		}
	}
	return m, nil
}
func writeJSON(path string, v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(buf, '\n'), 0600)
}

// Run writes a manifest to a new directory. Nil clients select plan-only mode.
func Run(ctx context.Context, out string, cases []memeval.Case, c Config, clients *Clients) (Summary, error) {
	m, err := makeManifest(cases, c)
	if err != nil {
		return Summary{}, err
	}
	if clients != nil && (clients.Formation == nil || clients.Foreground == nil || clients.Judge == nil) {
		return Summary{}, fmt.Errorf("缺少模型客户端")
	}
	s := Summary{Planned: len(m.Trials)}
	if err = os.Mkdir(out, 0700); err != nil {
		return s, err
	}
	if err = writeJSON(filepath.Join(out, "manifest.json"), m); err != nil || clients == nil {
		return s, err
	}
	f, err := os.OpenFile(filepath.Join(out, m.ResultsFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return s, err
	}
	enc := json.NewEncoder(f)
	var idx int
	for r := 1; r <= c.Repeats; r++ {
		for i, cs := range cases {
			dir := filepath.Join(out, fmt.Sprintf("r%d-c%d", r, i))
			var systems []layoutSystem
			var formErr error
			if ctx.Err() == nil {
				mt, e := newMeter(filepath.Join(dir, "formation-cost"), clients.Formation, c.FormationModel, c)
				formed := Formation{Case: cs.ID, Repeat: r}
				formErr = e
				if e == nil {
					systems, formErr = form(ctx, dir, cs, mt, c.Runes, memory.ContextLayout{CoreRunes: c.CoreRunes})
					formed.Cost, e = mt.cost(ctx)
					formErr = errors.Join(formErr, e)
				}
				if formErr != nil {
					formed.Error = formErr.Error()
				}
				s.Formation = append(s.Formation, formed)
			}
			for range 3 {
				for _, q := range cs.Questions {
					k := m.Trials[idx]
					idx++
					row := Row{Key: k, Status: "not_run"}
					switch {
					case ctx.Err() != nil:
						row.Error = ctx.Err().Error()
					case k.Variant != "off" && formErr != nil:
						row.Status = "ingest_failed"
						row.Error = formErr.Error()
					default:
						var sys memeval.System = memeval.Recent{Runes: c.Runes}
						for _, candidate := range systems {
							if candidate.Name() == k.Variant {
								sys = candidate
							}
						}
						row = ask(ctx, filepath.Join(out, k.Namespace), k, sys, cs, q, c, *clients)
					}
					switch row.Status {
					case "completed":
						s.Completed++
						if row.Correct {
							s.Correct++
						}
					case "not_run":
						s.NotRun++
					default:
						s.Failed++
					}
					if err = enc.Encode(row); err != nil {
						return s, errors.Join(err, f.Close())
					}
				}
			}
		}
	}
	s.SuccessRate = float64(s.Correct) / float64(s.Planned)
	return s, errors.Join(f.Close(), writeJSON(filepath.Join(out, m.SummaryFile), s), ctx.Err())
}

func ask(ctx context.Context, dir string, k Key, sys memeval.System, cs memeval.Case, q memeval.Question, c Config, clients Clients) (row Row) {
	start := time.Now()
	defer func() { row.Elapsed = time.Since(start) }()
	row = Row{Key: k, Status: "failed"}
	fore, err := newMeter(filepath.Join(dir, "foreground"), clients.Foreground, c.ForegroundModel, c)
	if err != nil {
		row.Error = err.Error()
		return
	}
	judge, err := newMeter(filepath.Join(dir, "judge"), clients.Judge, c.JudgeModel, c)
	if err != nil {
		row.Error = err.Error()
		return
	}
	cs.Questions = []memeval.Question{q}
	observed := &observedSystem{System: sys}
	runner := memeval.Runner{Systems: []memeval.System{observed}, Answerer: fore, Judge: judge}
	results, runErr := runner.Run(ctx, []memeval.Case{cs})
	row.Answer = fore.answer
	row.ContextRunes = utf8.RuneCountInString(observed.retrieved.Text)
	row.EvidenceHit = slices.ContainsFunc(observed.retrieved.Sessions, func(id string) bool { return slices.Contains(q.Evidence, id) })
	row.Foreground, err = fore.cost(ctx)
	runErr = errors.Join(runErr, err)
	row.Judge, err = judge.cost(ctx)
	runErr = errors.Join(runErr, err)
	if runErr != nil {
		switch {
		case fore.answered:
			row.Status = "grading_failed"
		case errors.Is(runErr, context.DeadlineExceeded):
			row.Status = "timeout"
		case errors.Is(runErr, ae.ErrModelBudgetExceeded):
			row.Status = "budget_exhausted"
		}
		row.Error = runErr.Error()
		return
	}
	if len(results) != 1 {
		row.Error = "评估器未返回唯一结果"
		return
	}
	result := results[0]
	row.Status = "completed"
	row.Correct = result.Correct
	row.EvidenceHit = result.EvidenceHit
	row.ContextRunes = result.ContextRunes
	return
}
