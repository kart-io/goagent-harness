// swebench 在 SWE-bench Multilingual 的 Go 子集上评测编码 Agent。
//
// Agent 在本机的 native 沙箱（bubblewrap 或 Landlock）中修改仓库，Go 版本经 GOTOOLCHAIN 对齐官方镜像；
// 补丁交给官方镜像和官方评测脚本判定，FAIL_TO_PASS 与 PASS_TO_PASS 全部通过记为解决。镜像在后台提前拉取。
//
// 运行：DEEPSEEK_API_KEY=... go run ./eval/swebench -data go.json -out results [-mode gold,agent] [-ids a,b] [-parallel 4]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Instance 是数据集中的一题，字段同 SWE-bench/SWE-bench_Multilingual。
type Instance struct {
	InstanceID       string `json:"instance_id"`
	Repo             string `json:"repo"`
	BaseCommit       string `json:"base_commit"`
	Image            string `json:"image"`
	Patch            string `json:"patch"`
	TestPatch        string `json:"test_patch"`
	EvalScript       string `json:"eval_script"`
	ProblemStatement string `json:"problem_statement"`
	FailToPass       tests  `json:"FAIL_TO_PASS"`
	PassToPass       tests  `json:"PASS_TO_PASS"`
}

// tests 兼容数据集中以 JSON 字符串或数组给出的测试名列表。
type tests []string

func (t *tests) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return json.Unmarshal([]byte(s), (*[]string)(t))
	}
	return json.Unmarshal(b, (*[]string)(t))
}

// Result 是一题一次运行的结果。
type Result struct {
	InstanceID string            `json:"instance_id"`
	Mode       string            `json:"mode"`
	Run        int               `json:"run"`
	Resolved   bool              `json:"resolved"`
	FailToPass map[string]string `json:"fail_to_pass"`
	PassToPass map[string]string `json:"pass_to_pass"`
	PatchBytes int               `json:"patch_bytes"`
	AgentError string            `json:"agent_error,omitempty"`
	Error      string            `json:"error,omitempty"`
	Seconds    float64           `json:"seconds"`
	ModelCalls string            `json:"model_calls,omitempty"`
}

var (
	dataPath = flag.String("data", "go.json", "数据集 JSON 数组")
	outDir   = flag.String("out", "results", "结果目录")
	modes    = flag.String("mode", "gold,agent", "逗号分隔，每题按顺序执行。gold：官方补丁（校验环境）；empty：空补丁（应判未解决）；agent：Agent 生成补丁，gold 未通过的题不跑")
	ids      = flag.String("ids", "", "只跑这些题，逗号分隔")
	parallel = flag.Int("parallel", 4, "并行题数")
	pulls    = flag.Int("pulls", 3, "后台并行拉取镜像数")
	runs     = flag.Int("runs", 1, "agent 每题重复次数")
	model    = flag.String("model", "deepseek", "Agent 使用的模型：deepseek（deepseek-chat）或 minimax（MiniMax-M3）")
	maxIter  = flag.Int("max-iterations", 100, "Agent 每题的模型请求上限")
	timeout  = flag.Duration("timeout", 30*time.Minute, "Agent 每题的运行时限")
	goEnv    = flag.String("go-env", "", "Agent 命令的 GOPATH、GOCACHE 与 HOME 所在目录，默认 <out>/goenv")
	worker   = flag.String("worker", "", "内部使用：在子进程中让 Agent 解这一题")
	workDir  = flag.String("dir", "", "内部使用：worker 的运行目录")
)

func main() {
	flag.Parse()
	all, err := load(*dataPath)
	if err != nil {
		fatal(err)
	}
	if *worker != "" {
		i := slices.IndexFunc(all, func(i Instance) bool { return i.InstanceID == *worker })
		if i < 0 {
			fatal(fmt.Errorf("没有题目 %s", *worker))
		}
		if err := solve(all[i], *workDir); err != nil {
			fatal(err)
		}
		return
	}
	if *ids != "" {
		want := strings.Split(*ids, ",")
		all = slices.DeleteFunc(all, func(i Instance) bool { return !slices.Contains(want, i.InstanceID) })
	}
	steps := strings.Split(*modes, ",")
	if slices.Contains(steps, "agent") && os.Getenv("DEEPSEEK_API_KEY") == "" {
		fatal(errors.New("需要 DEEPSEEK_API_KEY"))
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatal(err)
	}
	if *goEnv == "" {
		*goEnv = filepath.Join(*outDir, "goenv")
	}
	if *goEnv, err = filepath.Abs(*goEnv); err != nil {
		fatal(err)
	}
	if *dataPath, err = filepath.Abs(*dataPath); err != nil {
		fatal(err)
	}
	summary, err := os.OpenFile(filepath.Join(*outDir, "results.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = summary.Close() }()
	done := doneRuns(filepath.Join(*outDir, "results.jsonl"))
	ready := prefetch(all)

	var mu sync.Mutex
	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	resolved, total := 0, 0
	for _, inst := range all {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			pullErr := <-ready[inst.InstanceID]
			for _, m := range steps {
				n := 1
				if m == "agent" {
					n = *runs
					mu.Lock()
					gold, ok := done[inst.InstanceID+"/gold/1"]
					mu.Unlock()
					if ok && !gold {
						fmt.Printf("%s 跳过 agent：gold 未通过\n", inst.InstanceID)
						continue
					}
				}
				for run := 1; run <= n; run++ {
					key := fmt.Sprintf("%s/%s/%d", inst.InstanceID, m, run)
					mu.Lock()
					_, seen := done[key]
					mu.Unlock()
					if seen {
						continue
					}
					r := evaluate(inst, m, run, pullErr)
					line, _ := json.Marshal(r)
					mu.Lock()
					done[key] = r.Resolved
					if m == "agent" {
						total++
						if r.Resolved {
							resolved++
						}
					}
					_, _ = summary.Write(append(line, '\n'))
					fmt.Printf("[agent %d/%d 已解决] %s %s 第 %d 次：resolved=%v %s%s（%.0fs）\n", resolved, total, inst.InstanceID, m, run, r.Resolved, r.Error, r.AgentError, r.Seconds)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	fmt.Printf("\n本次 agent 运行：%d/%d 已解决\n", resolved, total)
}

func load(path string) ([]Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var all []Instance
	return all, json.Unmarshal(data, &all)
}

// prefetch 按题目顺序在后台拉取官方镜像，已在本地的跳过；每题的通道在镜像就绪或拉取失败时收到结果。
func prefetch(all []Instance) map[string]chan error {
	ready := make(map[string]chan error, len(all))
	for _, inst := range all {
		ready[inst.InstanceID] = make(chan error, 1)
	}
	queue := make(chan Instance)
	for range *pulls {
		go func() {
			for inst := range queue {
				var err error
				if exec.Command("docker", "image", "inspect", inst.Image).Run() != nil {
					start := time.Now()
					if out, e := exec.Command("docker", "pull", "-q", inst.Image).CombinedOutput(); e != nil {
						err = fmt.Errorf("pull: %v %s", e, out)
					}
					fmt.Printf("拉取 %s 用时 %s %v\n", inst.InstanceID, time.Since(start).Round(time.Second), err)
				}
				ready[inst.InstanceID] <- err
			}
		}()
	}
	go func() {
		for _, inst := range all {
			queue <- inst
		}
		close(queue)
	}()
	return ready
}

// doneRuns 读出已有结果，键为 题/模式/次，值为是否解决；中断后重跑时跳过已完成的部分。
// 因模型服务出错（503、断流）而中止的 Agent 运行不算完成：它反映的是服务状态，不是 Agent 的能力。
func doneRuns(path string) map[string]bool {
	done := map[string]bool{}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		var r Result
		if json.Unmarshal([]byte(line), &r) == nil && r.InstanceID != "" && r.Error == "" && !modelFailure(r) {
			done[fmt.Sprintf("%s/%s/%d", r.InstanceID, r.Mode, r.Run)] = r.Resolved
		}
	}
	return done
}

// modelFailure 报告 Agent 运行是否因模型请求失败而中止。
func modelFailure(r Result) bool {
	return r.Mode == "agent" && !r.Resolved && strings.Contains(r.AgentError, "[LLM_REQUEST]")
}

func evaluate(inst Instance, mode string, run int, pullErr error) (r Result) {
	start := time.Now()
	r = Result{InstanceID: inst.InstanceID, Mode: mode, Run: run}
	dir := filepath.Join(*outDir, inst.InstanceID, fmt.Sprintf("%s-%d", mode, run))
	defer func() {
		r.Seconds = time.Since(start).Seconds()
		b, _ := json.MarshalIndent(r, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Error = err.Error()
		return r
	}
	if pullErr != nil {
		r.Error = "镜像: " + pullErr.Error()
		return r
	}
	var patch string
	switch mode {
	case "gold":
		patch = inst.Patch
	case "empty":
	case "agent":
		p, calls, err := runAgent(inst, dir)
		patch, r.ModelCalls = p, calls
		if err != nil {
			r.AgentError = err.Error()
		}
	default:
		r.Error = "未知模式 " + mode
		return r
	}
	r.PatchBytes = len(patch)
	if err := os.WriteFile(filepath.Join(dir, "patch.diff"), []byte(patch), 0o644); err != nil {
		r.Error = err.Error()
		return r
	}
	log, err := runEval(inst, dir)
	_ = os.WriteFile(filepath.Join(dir, "eval.log"), []byte(log), 0o644)
	if err != nil {
		r.Error = "评测: " + err.Error()
		return r
	}
	status := parseGoTest(log)
	r.FailToPass, r.PassToPass = pick(status, inst.FailToPass), pick(status, inst.PassToPass)
	r.Resolved = allPass(r.FailToPass) && allPass(r.PassToPass)
	return r
}

// runAgent 准备仓库，在子进程中运行 Agent，返回它相对基准提交的补丁。每题的 Go 版本不同，
// 而沙箱里的命令只继承进程的环境变量，所以每题一个子进程。
func runAgent(inst Instance, dir string) (string, string, error) {
	ws, err := filepath.Abs(filepath.Join(dir, "repo"))
	if err != nil {
		return "", "", err
	}
	if err := prepareRepo(inst, ws); err != nil {
		return "", "", err
	}
	version, err := imageGoVersion(inst.Image)
	if err != nil {
		return "", "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	home, gopath, gocache := filepath.Join(*goEnv, "home"), filepath.Join(*goEnv, "gopath"), filepath.Join(*goEnv, "gocache")
	for _, d := range []string{home, gopath, gocache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout+10*time.Minute)
	defer cancel()
	abs, _ := filepath.Abs(dir)
	cmd := exec.CommandContext(ctx, self, "-worker", inst.InstanceID, "-data", *dataPath, "-dir", abs, "-go-env", *goEnv, "-model", *model,
		"-max-iterations", fmt.Sprint(*maxIter), "-timeout", timeout.String())
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN="+version, "HOME="+home, "GOPATH="+gopath,
		"GOMODCACHE="+filepath.Join(gopath, "pkg", "mod"), "GOCACHE="+gocache, "GOFLAGS=-buildvcs=false")
	out, runErr := cmd.CombinedOutput()
	_ = os.WriteFile(filepath.Join(dir, "worker.log"), out, 0o644)
	patch, err := diff(ws, inst.BaseCommit)
	if err != nil {
		return "", "", err
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "model_calls"))
	if runErr != nil {
		return patch, strings.TrimSpace(string(calls)), fmt.Errorf("%v: %s", runErr, lastLine(out))
	}
	return patch, strings.TrimSpace(string(calls)), nil
}

// prepareRepo 从官方镜像取出 /testbed，属主改为宿主用户。镜像里原有的未跟踪文件（如编译出的 gin.test）
// 写入 .git/info/exclude，不进入补丁：评测时它们同样存在，补丁再新建会应用失败。
func prepareRepo(inst Instance, ws string) error {
	if err := os.RemoveAll(ws); err != nil {
		return err
	}
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return err
	}
	uid := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if out, err := exec.Command("docker", "run", "--rm", "-v", ws+":/out", inst.Image, "sh", "-c", "cp -a /testbed/. /out/ && chown -R "+uid+" /out").CombinedOutput(); err != nil {
		return fmt.Errorf("复制仓库: %v %s", err, out)
	}
	untracked, err := exec.Command("git", "-C", ws, "ls-files", "--others", "--exclude-standard").Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %v", err)
	}
	var exclude strings.Builder
	for _, f := range strings.Split(strings.TrimSpace(string(untracked)), "\n") {
		if f != "" {
			exclude.WriteString("/" + f + "\n")
		}
	}
	return os.WriteFile(filepath.Join(ws, ".git", "info", "exclude"), []byte(exclude.String()), 0o644)
}

var goVersions sync.Map

// imageGoVersion 返回官方镜像里的 Go 版本（如 go1.23.8），作为 Agent 命令的 GOTOOLCHAIN。
func imageGoVersion(image string) (string, error) {
	if v, ok := goVersions.Load(image); ok {
		return v.(string), nil
	}
	out, err := exec.Command("docker", "run", "--rm", image, "bash", "-c", "PATH=$PATH:/usr/local/go/bin go env GOVERSION").Output()
	v := strings.TrimSpace(string(out))
	if i := strings.LastIndex(v, "\n"); i >= 0 {
		v = v[i+1:]
	}
	if err != nil || !strings.HasPrefix(v, "go") {
		return "", fmt.Errorf("读取镜像 Go 版本: %v %q", err, v)
	}
	goVersions.Store(image, v)
	return v, nil
}

// diff 返回工作区相对基准提交的全部改动，包括新文件。
func diff(ws, base string) (string, error) {
	if out, err := exec.Command("git", "-C", ws, "add", "-A").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git add: %v %s", err, out)
	}
	out, err := exec.Command("git", "-C", ws, "-c", "core.fileMode=false", "diff", "--cached", "--binary", base).Output()
	return string(out), err
}

// runEval 在官方镜像中应用补丁并运行官方评测脚本，与 SWE-bench 的评测一致。
func runEval(inst Instance, dir string) (string, error) {
	abs, _ := filepath.Abs(dir)
	if err := os.WriteFile(filepath.Join(dir, "eval.sh"), []byte(inst.EvalScript), 0o644); err != nil {
		return "", err
	}
	script := `cd /testbed && if [ -s /eval/patch.diff ]; then git apply -v /eval/patch.diff || { echo ">>>>> Patch Apply Failed"; exit 0; }; fi; bash /eval/eval.sh`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-v", abs+":/eval:ro", inst.Image, "bash", "-c", script).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return string(out), err
	}
	return string(out), nil
}

// goTestLine 同 SWE-bench 的 parse_log_gotest。
var goTestLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(`)

func parseGoTest(log string) map[string]string {
	status := map[string]string{}
	for _, l := range strings.Split(log, "\n") {
		if m := goTestLine.FindStringSubmatch(l); m != nil {
			status[m[2]] = m[1]
		}
	}
	return status
}

func pick(status map[string]string, names []string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		if s, ok := status[n]; ok {
			out[n] = s
		} else {
			out[n] = "MISSING"
		}
	}
	return out
}

func allPass(m map[string]string) bool {
	for _, s := range m {
		if s != "PASS" {
			return false
		}
	}
	return true
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
