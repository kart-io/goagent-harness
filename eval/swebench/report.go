package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// 结果分类。按排查顺序判定：环境与模型服务的问题与 Agent 能力无关，先排除；
// 其余按 Agent 停下的原因与补丁的评测结果区分。
const (
	CatResolved     = "resolved"      // 已解决
	CatEnv          = "env"           // 镜像、评测脚本等环境问题
	CatModelService = "model_service" // 模型请求失败（503、断流等）而中止
	CatAgentStopped = "agent_stopped" // Agent 未正常结束：超时、步数用尽、输出被截断等
	CatApplyFailed  = "apply_failed"  // 补丁无法应用
	CatNoPatch      = "no_patch"      // 没有改动
	CatWrongFix     = "wrong_fix"     // 应修复的测试仍未通过
	CatRegression   = "regression"    // 应修复的测试通过，但原本通过的测试失败
)

var categoryOrder = []string{CatResolved, CatWrongFix, CatRegression, CatNoPatch, CatApplyFailed, CatAgentStopped, CatModelService, CatEnv}

func categorize(r Result, applyFailed bool) string {
	switch {
	case r.Resolved:
		return CatResolved
	case r.Error != "":
		return CatEnv
	case modelFailure(r):
		return CatModelService
	case r.AgentError != "":
		return CatAgentStopped
	case applyFailed:
		return CatApplyFailed
	case r.PatchBytes == 0:
		return CatNoPatch
	case !allPass(r.FailToPass):
		return CatWrongFix
	}
	return CatRegression
}

// Summary 汇总一个结果目录中的 Agent 运行，写入 summary.json，供之后的运行用 -baseline 比较。
type Summary struct {
	Model          string                     `json:"model"`
	Verify         string                     `json:"verify,omitempty"`
	Runs           int                        `json:"runs"`
	Resolved       int                        `json:"resolved"`
	Rate           float64                    `json:"rate"`
	Categories     map[string]int             `json:"categories"`
	AvgSeconds     float64                    `json:"avg_seconds"`
	AvgModelCalls  float64                    `json:"avg_model_calls"`
	Instances      map[string]InstanceSummary `json:"instances"`
	ExcludedByGold []string                   `json:"excluded_by_gold,omitempty"`
}

// InstanceSummary 是一题的结果；Category 取最后一次运行。
type InstanceSummary struct {
	Runs     int    `json:"runs"`
	Resolved int    `json:"resolved"`
	Category string `json:"category"`
}

// summarize 汇总 results.jsonl 中属于 all 的题。同一 题/模式/次 有多条时取最后一条（续跑重做的运行）。
// 环境问题与模型服务失败不计入解决率的分母：它们反映的是环境与服务状态。
func summarize(path string, all []Instance) (Summary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Summary{}, err
	}
	want := map[string]bool{}
	for _, inst := range all {
		want[inst.InstanceID] = true
	}
	latest := map[string]Result{}
	var keys []string
	gold := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		var r Result
		if json.Unmarshal([]byte(line), &r) != nil || !want[r.InstanceID] {
			continue
		}
		if r.Mode == "gold" && r.Error == "" {
			gold[r.InstanceID] = r.Resolved
		}
		if r.Mode != "agent" {
			continue
		}
		if r.Category == "" {
			r.Category = categorize(r, false)
		}
		key := fmt.Sprintf("%s/%d", r.InstanceID, r.Run)
		if _, ok := latest[key]; !ok {
			keys = append(keys, key)
		}
		latest[key] = r
	}
	s := Summary{Categories: map[string]int{}, Instances: map[string]InstanceSummary{}}
	for id, ok := range gold {
		if !ok {
			s.ExcludedByGold = append(s.ExcludedByGold, id)
		}
	}
	slices.Sort(s.ExcludedByGold)
	var seconds, calls float64
	var counted int
	for _, key := range keys {
		r := latest[key]
		s.Categories[r.Category]++
		if s.Model == "" {
			s.Model, s.Verify = r.Model, r.Verify
		}
		if r.Category == CatEnv || r.Category == CatModelService {
			continue
		}
		inst := s.Instances[r.InstanceID]
		inst.Runs++
		inst.Category = r.Category
		s.Runs++
		if r.Resolved {
			inst.Resolved++
			s.Resolved++
		}
		s.Instances[r.InstanceID] = inst
		seconds += r.Seconds
		if n, err := strconv.Atoi(r.ModelCalls); err == nil {
			calls += float64(n)
			counted++
		}
	}
	if s.Runs > 0 {
		s.Rate = float64(s.Resolved) / float64(s.Runs)
		s.AvgSeconds = seconds / float64(s.Runs)
	}
	if counted > 0 {
		s.AvgModelCalls = calls / float64(counted)
	}
	return s, nil
}

// Diff 是本次结果相对基线的变化。只比较两边都有有效运行的题。
type Diff struct {
	Regressed []string // 基线解决率高于本次
	Improved  []string // 本次解决率高于基线
}

func compare(base, cur Summary) Diff {
	var d Diff
	for id, c := range cur.Instances {
		b, ok := base.Instances[id]
		if !ok || b.Runs == 0 || c.Runs == 0 {
			continue
		}
		// 比较解决率：b.Resolved/b.Runs 与 c.Resolved/c.Runs，交叉相乘避免浮点误差。
		switch bv, cv := b.Resolved*c.Runs, c.Resolved*b.Runs; {
		case bv > cv:
			d.Regressed = append(d.Regressed, id)
		case cv > bv:
			d.Improved = append(d.Improved, id)
		}
	}
	slices.Sort(d.Regressed)
	slices.Sort(d.Improved)
	return d
}

// errRegressed 表示相对基线有退步。
var errRegressed = errors.New("相对基线有退步")

// report 写出 summary.json 并打印分类；给了 -baseline 时打印与基线的差异，有退步时返回 errRegressed。
func report(all []Instance) error {
	s, err := summarize(filepath.Join(*outDir, "results.jsonl"), all)
	if err != nil {
		return err
	}
	// 早期结果没有记录模型，此时以显式给出的 -model 为准。
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "model" && s.Model == "" {
			s.Model = *model
		}
	})
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(filepath.Join(*outDir, "summary.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("\n汇总（%s %s）：%d/%d 已解决（%.0f%%），平均 %.0fs、%.0f 次模型请求\n", s.Model, s.Verify, s.Resolved, s.Runs, s.Rate*100, s.AvgSeconds, s.AvgModelCalls)
	for _, c := range categoryOrder {
		if n := s.Categories[c]; n > 0 {
			fmt.Printf("  %-14s %d\n", c, n)
		}
	}
	if len(s.ExcludedByGold) > 0 {
		fmt.Printf("  gold 未通过、未计入：%s\n", strings.Join(s.ExcludedByGold, ", "))
	}
	if *baseline == "" {
		return nil
	}
	data, err := os.ReadFile(*baseline)
	if err != nil {
		return err
	}
	var base Summary
	if err := json.Unmarshal(data, &base); err != nil {
		return fmt.Errorf("基线 %s: %w", *baseline, err)
	}
	d := compare(base, s)
	fmt.Printf("\n与基线（%s，%d/%d）比较：退步 %d 题，进步 %d 题\n", base.Model, base.Resolved, base.Runs, len(d.Regressed), len(d.Improved))
	for _, id := range d.Regressed {
		fmt.Printf("  退步 %s：%s\n", id, s.Instances[id].Category)
	}
	for _, id := range d.Improved {
		fmt.Printf("  进步 %s\n", id)
	}
	if len(d.Regressed) > 0 {
		return errRegressed
	}
	return nil
}
