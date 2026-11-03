package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
	"github.com/kart-io/goagent/tools"
	"gopkg.in/yaml.v3"
)

const systemPrompt = `你是一个严谨的编码 Agent，在 Git 仓库中修复问题。先阅读相关代码、复现问题，再做最小改动。
用 read_file、grep、glob 阅读代码，用 edit_file 修改，用 run_command 编译和运行相关测试（命令在仓库根目录执行，Go 工具链已就绪）。
不要提交，也不要修改已有的测试；可以新增测试帮助验证。完成后用几句话说明改了什么。`

// goEnvNames 是 Agent 命令需要的 Go 环境变量，由父进程按题设置；沙箱里的命令只继承这些与 PATH 等少数变量。
var goEnvNames = []string{"GOTOOLCHAIN", "HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "GOFLAGS", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE"}

// solve 在 worker 子进程中让 Agent 解一题：工具调用写入 agent.log，每次模型请求的耗时写入 requests.log。
func solve(inst Instance, dir string) error {
	ws := filepath.Join(dir, "repo")
	agentLog, err := os.Create(filepath.Join(dir, "agent.log"))
	if err != nil {
		return err
	}
	defer func() { _ = agentLog.Close() }()
	reqLog, err := os.Create(filepath.Join(dir, "requests.log"))
	if err != nil {
		return err
	}
	defer func() { _ = reqLog.Close() }()
	var mu sync.Mutex
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(agentLog, time.Now().Format("15:04:05 ")+format+"\n", args...)
	}

	doc := map[string]any{"presets": []any{map[string]any{"id": "swe", "components": []any{
		map[string]any{"id": "persona", "use": "persona", "config": map[string]any{"prompt": systemPrompt}},
		map[string]any{"id": "workspace", "use": "workspace", "config": map[string]any{
			"sandbox": "workspace-write", "executor": "native", "network": true, "commandTimeout": "5m",
			"envAllowlist": goEnvNames, "additionalWritableRoots": []string{*goEnv}}},
		map[string]any{"id": "env", "use": "env-context", "config": map[string]any{"tools": []string{"go", "git"}}},
		map[string]any{"id": "coding", "use": "coding-tools"},
		map[string]any{"id": "permissions", "use": "permissions", "config": map[string]any{"base": "coding"}},
		map[string]any{"id": "spill", "use": "spill"},
		map[string]any{"id": "loop", "use": "loop-detect"},
		map[string]any{"id": "verify", "use": "verify-before-stop", "config": map[string]any{"command": "go build ./..."}},
		map[string]any{"id": "compact", "use": "compaction", "config": map[string]any{"maxTokens": 60000}},
		map[string]any{"id": "wrap", "use": "wrap-up"},
	}}}}
	data, _ := yaml.Marshal(doc)
	set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Path: "eval/swebench", Trusted: true, Data: data})
	if err != nil {
		return err
	}
	p, _ := set.Get("swe")
	client, maxTokens, err := newModel()
	if err != nil {
		return err
	}
	summarizer, _, _ := newModel()
	asm, err := preset.Build(context.Background(), p, preset.Host{
		Root: ws, ArtifactDir: dir, CompactionModel: summarizer,
		Approver: tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
			return tools.ApprovalResult{Outcome: tools.ApprovalAllowedSession}, nil
		}),
		Dispatch: tools.DispatchConfig{OnResult: func(_ context.Context, r tools.DispatchResult) error {
			args, _ := json.Marshal(r.Args)
			logf("→ %s %s", r.Name, cut(string(args), 300))
			if msg, ok := tools.RejectedResult(r.Output); ok {
				logf("  ✗ %s", cut(msg, 300))
			}
			return nil
		}},
	})
	if err != nil {
		return err
	}
	defer func() { _ = asm.Close() }()
	timed := &timedClient{modelClient: client, log: reqLog}
	// Configure 的 3 次短重试扛不住服务端过载（503 too busy 常持续数十秒），之后按 5s 起、最长 60s 继续退避。
	// 评测只取最终答复，流先输出几个字节再停住时同样可以重发。
	longRetry := builder.RetryTransient(20, func(retry int) time.Duration { return min(5*time.Second<<max(retry-4, 0), time.Minute) })
	agent, err := preset.Configure(builder.NewSimpleBuilder(timed), asm).WithRequestError(20, longRetry).WithReplayablePartialOutput().WithMaxTokens(maxTokens).
		WithSessionID(inst.InstanceID).WithMaxIterations(*maxIter).WithTimeout(*timeout).Build()
	if err != nil {
		return err
	}
	task := "请解决下面这个 GitHub issue（仓库 " + inst.Repo + "）：\n\n" + inst.ProblemStatement
	out, runErr := agent.AsRunnable().Invoke(context.Background(), task)
	_ = os.WriteFile(filepath.Join(dir, "model_calls"), fmt.Append(nil, timed.count()), 0o644)
	if runErr != nil {
		logf("运行错误: %v", runErr)
		return runErr
	}
	logf("Agent: %v", out.Result)
	return nil
}

// modelClient 是 Agent 用到的模型能力：原生工具流，以及记录用量时读取的模型名。
type modelClient interface {
	llm.ToolStreamClient
	ModelName() string
}

// newModel 按 -model 创建模型客户端，并给出每次请求的输出 token 上限（Builder 的设置覆盖客户端的）。
func newModel() (modelClient, int, error) {
	opts := []llm.ClientOption{llm.WithTemperature(0), llm.WithTimeout(3 * time.Minute),
		// 服务端过载时流会在首包后停住或拖上几分钟，正常请求只要数秒；超时后由重试重发。
		llm.WithStreamIdleTimeout(time.Minute), llm.WithStreamTimeout(2 * time.Minute)}
	switch *model {
	case "deepseek":
		c, err := providers.NewDeepSeekWithOptions(append(opts, llm.WithAPIKey(os.Getenv("DEEPSEEK_API_KEY")), llm.WithModel("deepseek-chat"))...)
		return c, 2000, err
	case "minimax":
		// MiniMax 的 OpenAI 兼容接口把思考过程以 <think> 写在 content 里，原样留在历史中正是它要求的交错思考用法；
		// 思考也计入输出 token，默认的 2000 常在调用工具前就被耗尽。
		c, err := providers.NewOpenAIWithOptions(append(opts, llm.WithAPIKey(os.Getenv("MINIMAX_CN_API_KEY")),
			llm.WithBaseURL("https://api.minimaxi.com/v1"), llm.WithModel("MiniMax-M3"))...)
		return c, 32000, err
	}
	return nil, 0, fmt.Errorf("未知模型 %q，可选 deepseek、minimax", *model)
}

// timedClient 记录每次模型请求的首包延迟、总耗时、结束原因与用量，用于定位卡住的请求。
type timedClient struct {
	modelClient
	mu  sync.Mutex
	n   int
	log *os.File
}

func (c *timedClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *timedClient) record(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = fmt.Fprintf(c.log, time.Now().Format("15:04:05 ")+format+"\n", args...)
}

func (c *timedClient) CompleteToolsStream(ctx context.Context, req *llm.CompletionRequest, ts []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	size := 0
	for _, m := range req.Messages {
		size += len(m.Content)
		for _, tc := range m.ToolCalls {
			size += len(tc.Function.Arguments)
		}
	}
	start := time.Now()
	c.record("#%d 开始 消息 %d 条 %d 字节", n, len(req.Messages), size)
	in, err := c.modelClient.CompleteToolsStream(ctx, req, ts)
	if err != nil {
		c.record("#%d 失败 %s：%v", n, time.Since(start).Round(time.Millisecond), err)
		return nil, err
	}
	out := make(chan *llm.StreamChunk)
	go func() {
		defer close(out)
		var first time.Duration
		var last *llm.StreamChunk
		chars := 0
		for chunk := range in {
			if first == 0 {
				first = time.Since(start)
			}
			chars += len(chunk.Delta)
			last = chunk
			select {
			case out <- chunk:
			case <-ctx.Done():
			}
		}
		line := fmt.Sprintf("#%d 结束 首包 %s 总计 %s 输出 %d 字节", n, first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond), chars)
		if last != nil {
			line += fmt.Sprintf(" finish=%q 工具调用 %d", last.FinishReason, len(last.ToolCalls))
			if last.Usage != nil {
				line += fmt.Sprintf(" 输入 %d（缓存 %d）输出 %d", last.Usage.PromptTokens, last.Usage.CachedTokens, last.Usage.CompletionTokens)
			}
			if last.Error != nil {
				line += fmt.Sprintf(" 错误 %v", last.Error)
			}
		}
		if ctx.Err() != nil {
			line += fmt.Sprintf(" ctx=%v", ctx.Err())
		}
		c.record("%s", line)
	}()
	return out, nil
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
