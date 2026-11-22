package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	agenterrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/execution"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/providers"
	"github.com/kart-io/goagent/memory"
)

// LivePromptVersion identifies the public prompts used by all three live variants.
const LivePromptVersion = "planning-live-v1"
const answerContract = "Return only a JSON object with facts (object), evidence (array of {source_id,json_pointer}), and abstain (boolean). Use the supplied materials only."

// Live runs independent trials against the configured provider.
type Live struct {
	// Client must send at most one physical model request per CompleteStream call.
	// Disable transport redirects, internal retries and request splitting. Builder
	// owns any retry so that each new request has a separate ledger admission.
	Client      llm.Client
	Root, Model string
	Temperature float64
}

type countedClient struct {
	llm.Client
	model    string
	physical atomic.Int64
}

func (c *countedClient) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	actual := *req
	actual.Model = c.model
	c.physical.Add(1)
	return c.Client.CompleteStream(ctx, &actual)
}

type liveTrial struct {
	client      *countedClient
	input       Input
	temperature float64
}

func (t liveTrial) agent(role, prompt string) (*builder.SimpleAgent, error) {
	return builder.NewSimpleBuilder(t.client).WithAgentID(role).WithDurableScope(role).WithSessionID(t.input.Namespace).
		WithSystemPrompt(prompt).WithMaxIterations(1).WithMaxTokens(1024).WithTemperature(t.temperature).
		WithTokenEstimator(t.estimate).WithRequestError(1, builder.RetryTransient(1, nil)).Build()
}

func (t liveTrial) estimate(_ context.Context, req *llm.CompletionRequest, tools []interfaces.Tool) (int, error) {
	if len(tools) != 0 || req.MaxTokens <= 0 {
		return 0, fmt.Errorf("仅支持设置了输出上限的纯文本请求")
	}
	actual := *req
	actual.Model = t.client.model
	data, err := json.Marshal(actual)
	if err != nil {
		return 0, err
	}
	// 每个 UTF-8 字节按一个 token 估算，再加消息封装开销与预留输出。
	n := len(data) + 16*(1+len(actual.Messages)) + req.MaxTokens
	if n > t.input.Limits.ContextTokens {
		return 0, fmt.Errorf("估算上下文用量为 %d：%w", n, agenterrors.ErrModelBudgetExceeded)
	}
	return n, nil
}

// 取消后仍等到 Builder 关闭事件流，确保模型用量与日志已写完再读取账本。
func liveRunnable(a *builder.SimpleAgent) core.Runnable[any, *builder.AgentOutput] {
	return core.NewRunnableFunc(func(ctx context.Context, input any) (*builder.AgentOutput, error) {
		events, err := a.ExecuteStream(ctx, input)
		if err != nil {
			return nil, err
		}
		var out *builder.AgentOutput
		for event := range events {
			if event.Error != nil {
				err = event.Error
			}
			if event.Type == "done" {
				out = event.Output
			}
		}
		if err != nil {
			return nil, err
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if out == nil {
			return nil, io.ErrUnexpectedEOF
		}
		return out, nil
	})
}

func (t liveTrial) ask(ctx context.Context, role, prompt string, input any) (answer string, err error) {
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	a, err := t.agent(role, prompt)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, a.Shutdown(context.WithoutCancel(ctx))) }()
	out, err := liveRunnable(a).Invoke(ctx, string(data))
	if err != nil {
		return "", err
	}
	answer, ok := out.Result.(string)
	if !ok {
		return "", fmt.Errorf("角色 %s 返回了非文本输出", role)
	}
	return answer, nil
}

func (e Live) Run(ctx context.Context, v Variant, input Input) (obs Observation, err error) {
	if err = e.validateConfig(); err != nil {
		return obs, err
	}
	if input.RunID == "" || !filepath.IsLocal(input.Namespace) ||
		filepath.Base(input.Namespace) != input.Namespace || input.Namespace == "." {
		return obs, fmt.Errorf("试验运行标识或命名空间无效")
	}
	if !slices.Contains(variants[:], v) || input.Limits.Requests <= 0 ||
		input.Limits.Tokens <= 0 || input.Limits.ContextTokens <= 0 {
		return obs, fmt.Errorf("试验组别或资源上限无效")
	}
	if err = os.MkdirAll(e.Root, 0700); err != nil {
		return
	}
	dir := filepath.Join(e.Root, input.Namespace)
	if err = os.Mkdir(dir, 0700); err != nil {
		return
	}
	store, err := durable.NewFileStore(dir)
	if err != nil {
		return obs, err
	}
	controller, err := budget.NewDurable(budget.Config{
		RootKey: input.RunID, Version: "eval-v1", TokenPolicyVersion: "utf8-bytes-plus-output-v1",
		MaxModelRequests: input.Limits.Requests, MaxReservedTokens: input.Limits.Tokens,
	}, store)
	if err != nil {
		return obs, err
	}
	scope, err := execution.NewScope(execution.Config{RunID: input.RunID, SessionID: input.Namespace, Journal: store, Budget: controller})
	if err != nil {
		return obs, err
	}
	ctx, stop, err := scope.Bind(memory.WithNamespace(ctx, "planning-eval", input.Namespace))
	if err != nil {
		return obs, err
	}
	defer stop()
	t := liveTrial{&countedClient{Client: e.Client, model: e.Model}, input, e.Temperature}
	obs.Effects = []string{}
	if v.Name == "direct" {
		obs.Answer, err = t.ask(ctx, "direct", answerContract, struct {
			Request      string
			Sources      []Source
			OutputFields []string
		}{input.Request, input.Sources, input.OutputFields})
	} else {
		obs.Answer, err = t.plan(ctx, store, v.Slots)
	}
	ledger, readErr := controller.Attribution(context.WithoutCancel(ctx))
	physical := int(t.client.physical.Load())
	unknown := ledger.Unattributed.UnknownTotalEntries
	for _, a := range ledger.Agents {
		unknown += a.Self.UnknownTotalEntries
	}
	obs.Costs.Foreground = Measurement{
		PhysicalRequests: physical, LedgerRequests: ledger.Root.ReservedRequests,
		KnownTokens:       int64(ledger.Root.KnownTokens.TotalTokens),
		UnknownUsageCalls: unknown, UnknownCostCalls: physical,
	}
	err = errors.Join(err, readErr)
	if readErr == nil && physical != ledger.Root.ReservedRequests {
		err = errors.Join(err, fmt.Errorf("物理调用次数 %d 与账本计数 %d 不一致", physical, ledger.Root.ReservedRequests))
	}
	switch {
	case errors.Is(err, agenterrors.ErrModelBudgetExceeded):
		obs.Outcome = "budget_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		obs.Outcome = "timeout"
	case err != nil:
		obs.Outcome = "failed"
	default:
		obs.Outcome = "completed"
	}
	return
}

func (e Live) validateConfig() error {
	if e.Client == nil || e.Root == "" || strings.TrimSpace(e.Model) == "" || e.Model == "offline-none" {
		return fmt.Errorf("live 模式缺少模型、客户端或状态目录")
	}
	if math.IsNaN(e.Temperature) || e.Temperature < 0 || e.Temperature > 2 {
		return fmt.Errorf("live 模式的温度必须介于 0 和 2 之间")
	}
	return nil
}

func (e Live) validateManifest(m Manifest) error {
	if err := e.validateConfig(); err != nil {
		return err
	}

	if m.Backend != "live" || m.Model != e.Model || m.Temperature != e.Temperature || m.PromptVersion != LivePromptVersion {
		return fmt.Errorf("试验清单与 live 模型配置不匹配")
	}
	return nil
}

// NewLiveOpenAIClient builds the OpenAI-compatible client used by the live CLI.
func NewLiveOpenAIClient(model string, temperature float64, baseURL string) (llm.Client, error) {
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	opts := []llm.ClientOption{llm.WithModel(model), llm.WithTemperature(temperature), llm.WithHTTPClient(httpClient)}
	if baseURL != "" {
		opts = append(opts, llm.WithBaseURL(baseURL))
	}
	return providers.NewOpenAIWithOptions(opts...)
}
