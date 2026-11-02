package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

// ComponentSubagent 是子 Agent 组件名。
const ComponentSubagent = "subagent"

// SubagentConfig 是 subagent 组件的配置：提供一个工具，把任务交给另一个 preset 装配的子 Agent，返回其最终答复。
// 子 Agent 与父 Agent 一同装配：它的每次调用同样要经过父 Agent 的调用前钩子与守卫，沙箱不宽于父 Agent，
// 父 Agent 的 approval: never 同样适用，因此权限是父 Agent、目标 preset 与宿主上限的交集。
type SubagentConfig struct {
	// Preset 是子 Agent 的 preset id。
	Preset string `yaml:"preset"`
	// ToolName 默认 agent_<preset>，preset id 中的其他字符替换为下划线。
	ToolName    string `yaml:"toolName"`
	Description string `yaml:"description"`
	// ToolFilter 在初始化之前限定子 Agent 的工具，被完全过滤掉的组件不初始化。
	ToolFilter ToolFilter `yaml:"toolFilter"`
	// MaxDepth 是允许的嵌套深度，默认 1：子 Agent 自己不能再委派。
	MaxDepth *int `yaml:"maxDepth"`
	// Timeout 是一次委派的上限，默认 10m。
	Timeout time.Duration `yaml:"timeout"`
	// Mode 默认 foreground：工具等待子 Agent 并返回答复。background 时工具立即返回 child_id，另有 <toolName>_status 查询工具，
	// 子 Agent 由 Host.Background 持有，完成或等待审批时通知所有者，需要持久运行提供的 ExecutionKey。
	Mode string `yaml:"mode"`
}

func init() {
	Register(Spec[SubagentConfig]{
		Name: ComponentSubagent, Version: "1",
		Validate: func(c *SubagentConfig) error {
			if c.Preset == "" {
				return errors.New("需要 preset")
			}
			if c.ToolName == "" {
				c.ToolName = "agent_" + strings.Map(func(r rune) rune {
					if r == '-' || r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
						return r
					}
					return '_'
				}, c.Preset)
			}
			if !toolNamePattern.MatchString(c.ToolName) {
				return fmt.Errorf("toolName %q 不合法，须匹配 %s", c.ToolName, toolNamePattern)
			}
			if c.Description == "" {
				c.Description = "把任务交给子 Agent（preset " + c.Preset + "）独立完成，返回其最终答复。task 须包含完成任务所需的全部上下文。"
			}
			if c.MaxDepth == nil {
				one := 1
				c.MaxDepth = &one
			}
			if *c.MaxDepth < 0 {
				return errors.New("maxDepth 不能为负")
			}
			if c.Timeout == 0 {
				c.Timeout = 10 * time.Minute
			}
			if c.Timeout < 0 {
				return errors.New("timeout 不能为负")
			}
			if c.Mode == "" {
				c.Mode = string(background.ModeForeground)
			}
			if c.Mode != string(background.ModeForeground) && c.Mode != string(background.ModeBackground) {
				return fmt.Errorf("mode %q 不合法，须是 foreground 或 background", c.Mode)
			}
			return nil
		},
		References: func(c *SubagentConfig) []string { return []string{c.Preset} },
		Build:      buildSubagent,
	})
}

func buildSubagent(ctx context.Context, a *Assembler, c *SubagentConfig) error {
	if a.host.RunAgent == nil {
		return errors.New("子 Agent 需要 Host.RunAgent")
	}
	isBackground := c.Mode == string(background.ModeBackground)
	if isBackground && a.host.Background == nil {
		return errors.New("后台子 Agent 需要 Host.Background")
	}
	depth := a.Depth() + 1
	if depth > *c.MaxDepth {
		return fmt.Errorf("子 Agent 嵌套深度 %d 超过 maxDepth %d", depth, *c.MaxDepth)
	}
	target, ok := a.preset.set.Get(c.Preset)
	if !ok {
		return fmt.Errorf("preset %s 不存在", c.Preset)
	}
	tool := &subagentTool{name: c.ToolName, description: c.Description, timeout: c.Timeout, run: a.host.RunAgent}
	// 后台链路中的前台子 Agent：等待要能穿到外层，所以保留日志并声明可续跑。
	_, tool.keepJournal = a.host.Approver.(deferredApprover)
	switch {
	case isBackground:
		tool.keepJournal = false
		a.AddTools(tool, &subagentStatusTool{name: c.ToolName + "_status", tool: tool})
	case tool.keepJournal:
		a.AddTools(resumableSubagentTool{tool})
	default:
		a.AddTools(tool)
	}
	slot := a.cur
	parentSandbox := Sandbox("")
	var parentNetwork *bool
	if a.opts.parent != nil {
		parentSandbox = a.opts.parent.sandbox
		parentNetwork = a.opts.parent.network
	}
	a.AfterPublish(func(parent *tools.Pipeline) error {
		if a.workspace != nil {
			if parentSandbox == "" || a.workspaceInfo.Sandbox.rank() < parentSandbox.rank() {
				parentSandbox = a.workspaceInfo.Sandbox
			}
			network := a.workspaceInfo.Network
			if parentNetwork != nil {
				network = network && *parentNetwork
			}
			parentNetwork = &network
		}
		filter := c.ToolFilter
		childHost := a.host
		if childHost.AgentID == "" {
			childHost.AgentID = a.preset.ID
		}
		childHost.AgentID += "/" + a.preset.Rows[slot].ID
		if isBackground {
			// 后台没有人在同步等待审批，占着槽位阻塞也没有意义。
			childHost.Approver = deferredApprover{}
		}
		policy, ceiling, controller, envAllowlist := a.workspaceInfo.Policy, a.workspaceInfo.Ceiling, a.controller, a.workspaceInfo.EnvAllowlist
		if a.workspace == nil && a.opts.parent != nil {
			policy, ceiling, controller, envAllowlist = a.opts.parent.policy, a.opts.parent.ceiling, a.opts.parent.controller, a.opts.parent.envAllowlist
		}
		if isBackground {
			tool.manager, tool.runner, tool.agentID = a.host.Background, childHost.AgentID+"/"+c.ToolName, childHost.AgentID
		}
		if isBackground && a.opts.childSandboxes != nil {
			// Build 时已用模板登记 Runner：这一次只是某个 child 的运行，登记会覆盖它，也不能让内层依赖外层的这次运行。
			return nil
		}
		scope := append(slices.Clone(a.opts.scope), a.preset.Rows[slot].ID, target.ID)
		options := buildOptions{
			depth: depth,
			scope: scope,
			parent: &parentScope{network: parentNetwork, pre: parent.Pre, guards: parent.Guards, sandbox: parentSandbox, denyApproval: a.denyApproval,
				policy: policy, ceiling: ceiling, controller: controller, envAllowlist: envAllowlist},
			filter:         &filter,
			template:       a.opts.template || isBackground,
			childSandboxes: a.opts.childSandboxes,
		}
		child, err := build(ctx, target, childHost, options)
		if err != nil {
			return fmt.Errorf("子 Agent %s: %w", c.Preset, err)
		}
		a.closers = append(a.closers, child.Close)
		tool.child = child
		if child.perRun && !isBackground {
			a.perRun = true
		}
		if a.contribs[slot].info == nil {
			a.contribs[slot].info = map[string]string{}
		}
		a.contribs[slot].info["manifest"] = child.Manifest.Digest()
		if !isBackground {
			return nil
		}
		if a.host.Sandboxes != nil {
			a.host.Background.RegisterResource(sandboxResource, releaseSandbox(a.host.Sandboxes))
		}
		a.host.Background.Register(tool.runner, func(ctx context.Context, task background.Task) (string, error) {
			if task.Session != "" {
				ctx = tools.WithSessionID(ctx, task.Session)
			}
			asm := child
			if child.perRun {
				perRun := options
				perRun.template = false
				perRun.scope = append(slices.Clone(scope), task.ID)
				perRun.childSandboxes = &childSandboxes{manager: a.host.Background, childID: task.ID}
				built, err := build(ctx, target, childHost, perRun)
				if err != nil {
					return "", err
				}
				asm = built
				// 实例已交给子工作或宿主，这里只断开连接；销毁由子工作结束时的释放负责。
				defer func() { _ = asm.Close() }()
			}
			if err := asm.PinManifest(ctx); err != nil {
				if errors.Is(err, ErrManifestChanged) {
					return "", &effects.FailureError{Kind: "manifest_changed", Err: err}
				}
				return "", err
			}
			return a.host.RunAgent(ctx, asm, task.Input)
		})
		return nil
	})
	return nil
}

type subagentTool struct {
	name, description string
	timeout           time.Duration
	run               func(context.Context, *Assembly, string) (string, error)
	child             *Assembly
	// keepJournal 让子 Agent 继续写入父调用的日志，见 resumableSubagentTool。
	keepJournal bool
	// manager 非 nil 时工具提交后台子工作。
	manager         *background.Manager
	runner, agentID string
}

// resumableSubagentTool 是后台链路中的前台子 Agent：效果都记在调用的派生执行键下，审批等待后重新进入只会回放。
type resumableSubagentTool struct{ *subagentTool }

func (resumableSubagentTool) DurableOperationPolicy() durable.OperationPolicy {
	return durable.OperationPolicy{MaxAttempts: 3, Resumable: true}
}

func (t *subagentTool) Name() string                   { return t.name }
func (t *subagentTool) Description() string            { return t.description }
func (t *subagentTool) DispatchTimeout() time.Duration { return t.timeout }
func (t *subagentTool) ArgsSchema() string {
	return `{"type":"object","properties":{"task":{"type":"string","description":"交给子 Agent 的任务，须包含完成任务所需的全部上下文"}},"required":["task"],"additionalProperties":false}`
}

// Invoke 运行子 Agent。父 Agent 的日志已记录整次委派，子 Agent 不再写入日志：恢复时中断的委派按父调用的策略整体重跑。
// 后台链路例外，见 resumableSubagentTool。
func (t *subagentTool) Invoke(ctx context.Context, input *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
	task, _ := input.Args["task"].(string)
	if strings.TrimSpace(task) == "" {
		return tools.Rejected("task must not be empty", nil), nil
	}
	if t.manager != nil {
		return t.spawn(ctx, input, task)
	}
	runCtx := ctx
	if !t.keepJournal {
		runCtx = durable.WithJournal(ctx, nil)
	}
	answer, err := t.run(runCtx, t.child, task)
	if err != nil {
		return nil, fmt.Errorf("子 Agent %s: %w", t.name, err)
	}
	return &interfaces.ToolOutput{Success: true, Result: map[string]any{"answer": answer}}, nil
}

func (t *subagentTool) ownerOf(ctx context.Context) string {
	if owner := tools.SessionIDFrom(ctx); owner != "" {
		return owner
	}
	return t.agentID
}

// spawn 提交后台子工作。ID 取自持久调用的 ExecutionKey：工具调用重放时找回同一 child，不会启动第二份工作。
func (t *subagentTool) spawn(ctx context.Context, input *interfaces.ToolInput, task string) (*interfaces.ToolOutput, error) {
	if input.ExecutionKey == "" {
		return nil, fmt.Errorf("子 Agent %s: 后台模式需要持久运行提供的 ExecutionKey", t.name)
	}
	record, err := t.manager.Spawn(ctx, background.Task{ID: input.ExecutionKey, Owner: t.ownerOf(ctx), Runner: t.runner, Input: task,
		Mode: background.ModeBackground, Session: tools.SessionIDFrom(ctx)})
	if err != nil {
		return nil, fmt.Errorf("子 Agent %s: %w", t.name, err)
	}
	return &interfaces.ToolOutput{Success: true, Result: map[string]any{"child_id": record.ID, "status": string(record.Status)}}, nil
}

// subagentStatusTool 查询后台子工作。只读，只看得到当前所有者的记录。
type subagentStatusTool struct {
	name string
	tool *subagentTool
}

func (t *subagentStatusTool) Name() string { return t.name }
func (t *subagentStatusTool) Description() string {
	return "查询 " + t.tool.name + " 启动的后台子工作的状态和结果。"
}
func (t *subagentStatusTool) ArgsSchema() string {
	return `{"type":"object","properties":{"child_id":{"type":"string","description":"启动子工作时返回的 child_id"}},"required":["child_id"],"additionalProperties":false}`
}

// DurableOperationPolicy 允许中断后重新查询：查询没有副作用。
func (t *subagentStatusTool) DurableOperationPolicy() durable.OperationPolicy {
	return durable.OperationPolicy{MaxAttempts: 3, RetryFailed: true}
}

func (t *subagentStatusTool) Invoke(ctx context.Context, input *interfaces.ToolInput) (*interfaces.ToolOutput, error) {
	id, _ := input.Args["child_id"].(string)
	record, err := t.tool.manager.Get(ctx, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("子 Agent %s: %w", t.tool.name, err)
	}
	// 不存在和属于别人返回同样的结果，不暴露其他所有者的子工作。
	if err != nil || record.Owner != t.tool.ownerOf(ctx) {
		return tools.Rejected("unknown child_id", nil), nil
	}
	result := map[string]any{"child_id": record.ID, "status": string(record.Status)}
	if record.Result != "" {
		result["result"] = record.Result
	}
	if record.ResultTruncated {
		result["result_truncated"] = true
	}
	if record.FailureKind != "" {
		result["failure_kind"] = record.FailureKind
	}
	return &interfaces.ToolOutput{Success: true, Result: result}, nil
}

// 宿主答复审批等待时使用的续跑输入。其他值一律按拒绝处理。
const (
	ApprovalAllow = "allow"
	ApprovalDeny  = "deny"
)

const maxApprovalArgs = 4 << 10

// Approval 是后台子 Agent 等待审批的调用，供宿主展示。内容来自模型，是数据，不授予任何权限。
type Approval struct {
	Tool          string `json:"tool"`
	Reason        string `json:"reason,omitempty"`
	Args          string `json:"args"`
	ArgsTruncated bool   `json:"args_truncated,omitempty"`
}

// ApprovalOf 取出等待中的审批调用。不是审批等待（如子流程自身的等待）时报错。
func ApprovalOf(wait *durable.WaitError) (Approval, error) {
	var approval Approval
	if err := json.Unmarshal(wait.Continuation, &approval); err != nil || approval.Tool == "" {
		return Approval{}, errors.New("不是审批等待")
	}
	return approval, nil
}

// deferredApprover 把审批变成可续跑的等待：准入在操作日志之外，批准后重新进入时只放行这一次调用，不留下会话授权。
type deferredApprover struct {
	// cancel 在拒绝时终止这次调用已经启动的命令（重启后附着前要重新审批），started 报告它是否启动过。
	cancel func(ctx context.Context, key string) (started bool, err error)
}

func (d deferredApprover) Approve(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
	key := req.Call.ExecutionKey
	// 没有日志就无法续跑（如前台孙辈），没有人能作答。
	if !durable.HasJournal(ctx) || key == "" {
		return tools.ApprovalResult{Outcome: tools.ApprovalUnavailable}, nil
	}
	if input, ok := durable.ResumeInput(durable.WithExecutionKey(ctx, key)); ok {
		if input == ApprovalAllow {
			return tools.ApprovalResult{Outcome: tools.ApprovalAllowedOnce}, nil
		}
		if d.cancel != nil {
			started, err := d.cancel(ctx, key)
			if err != nil {
				return tools.ApprovalResult{}, fmt.Errorf("终止被拒绝的命令: %w", err)
			}
			if started {
				return tools.ApprovalResult{Outcome: tools.ApprovalRejected, Feedback: "这条命令在进程重启前已经启动，可能已部分生效，现已终止。请先检查状态再继续。"}, nil
			}
		}
		return tools.ApprovalResult{Outcome: tools.ApprovalRejected}, nil
	}
	args, err := json.Marshal(req.Call.Args)
	if err != nil {
		return tools.ApprovalResult{}, err
	}
	approval := Approval{Tool: req.Call.Tool, Reason: req.Reason, Args: string(args)}
	if len(approval.Args) > maxApprovalArgs {
		approval.Args, approval.ArgsTruncated = strings.ToValidUTF8(approval.Args[:maxApprovalArgs], ""), true
	}
	continuation, err := json.Marshal(approval)
	if err != nil {
		return tools.ApprovalResult{}, err
	}
	return tools.ApprovalResult{}, &durable.WaitError{Path: []durable.WaitStep{{ID: "approval/" + req.Call.Tool, Key: key}}, Continuation: continuation}
}

const sandboxResource = "sandbox"

// childSandboxes 让托管沙箱的责任跟随子工作：进程停止时实例保留，重启后连回同一个，子工作结束时才销毁。
// 分配前先落一条意图，崩溃后才知道该去查回实例。宿主用 Registry.Resolve 借出或保留的实例仍由宿主负责，不记在子工作上。
type childSandboxes struct {
	manager *background.Manager
	childID string
}

// sandboxRecord 是 Resource.Data。Instance 为空表示分配结果未知。
type sandboxRecord struct {
	Request  sandbox.Request `json:"request"`
	Instance string          `json:"instance,omitempty"`
}

func (c *childSandboxes) attach(ctx context.Context, key string, state sandboxRecord) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return c.manager.Attach(ctx, c.childID, background.Resource{Kind: sandboxResource, ID: key, Data: data})
}

func (c *childSandboxes) open(ctx context.Context, registry *sandbox.Registry, req sandbox.Request) (*sandbox.Session, error) {
	record, err := c.manager.Get(ctx, c.childID)
	if err != nil {
		return nil, err
	}
	identity, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	key := sha256Hex(string(identity))
	var previous *sandboxRecord
	for _, resource := range record.Resources {
		if resource.Kind == sandboxResource && resource.ID == key {
			previous = &sandboxRecord{}
			if err := json.Unmarshal(resource.Data, previous); err != nil {
				return nil, fmt.Errorf("无效的沙箱记录: %w", err)
			}
		}
	}
	adopted := false
	session, err := registry.OpenWith(ctx, req, func(scoped sandbox.Request) (sandbox.Binding, error) {
		if previous != nil {
			if previous.Instance != "" {
				return sandbox.Binding{ID: previous.Instance}, nil
			}
			// 上次在分配时崩溃。实例可能已存在：查不了就不能再分配一个掩盖它。
			id, found, err := registry.FindByScope(ctx, req)
			if errors.Is(err, sandbox.ErrUnsupported) {
				return sandbox.Binding{}, &effects.FailureError{Kind: "sandbox_unknown", Err: err}
			}
			if err != nil {
				return sandbox.Binding{}, err
			}
			adopted = true
			if found {
				return sandbox.Binding{ID: id}, nil
			}
			return sandbox.Binding{Retain: true}, nil
		}
		if registry.Resolve != nil {
			binding, err := registry.Resolve(scoped)
			if err != nil || binding.ID != "" || binding.Retain {
				return binding, err
			}
		}
		if err := c.attach(ctx, key, sandboxRecord{Request: req}); err != nil {
			return sandbox.Binding{}, err
		}
		adopted = true
		return sandbox.Binding{Retain: true}, nil
	})
	if !adopted {
		if errors.Is(err, sandbox.ErrInstanceNotFound) {
			return session, &effects.FailureError{Kind: "sandbox_gone", Err: err}
		}
		return session, err
	}
	if err == nil {
		err = c.attach(ctx, key, sandboxRecord{Request: req, Instance: session.Info().ID})
	}
	if err == nil {
		return session, nil
	}
	if session == nil || session.Info().ID == "" {
		// 除非 Provider 确认没有实例，否则响应可能只是丢了：保留意图，释放时再查回，查不了就交给宿主。
		if !errors.Is(err, sandbox.ErrNotCreated) {
			return session, err
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return session, errors.Join(err, c.manager.Detach(cleanup, c.childID, sandboxResource, key))
	}
	// 实例没有被记录就不会有人释放，这里当场删除；Session 由调用方关闭。
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if destroyErr := registry.Destroy(cleanup, req, session.Info().ID); destroyErr != nil {
		return session, errors.Join(err, destroyErr)
	}
	return session, errors.Join(err, c.manager.Detach(cleanup, c.childID, sandboxResource, key))
}

func releaseSandbox(registry *sandbox.Registry) func(context.Context, background.Resource) error {
	return func(ctx context.Context, resource background.Resource) error {
		var state sandboxRecord
		if err := json.Unmarshal(resource.Data, &state); err != nil {
			return effects.Drop("corrupt_sandbox_resource")
		}
		instance := state.Instance
		if instance == "" {
			id, found, err := registry.FindByScope(ctx, state.Request)
			switch {
			case errors.Is(err, sandbox.ErrUnsupported):
				return background.ErrResourceUnknown
			case errors.Is(err, sandbox.ErrClosed):
				return effects.ErrLater
			case err != nil:
				return err
			case !found:
				return nil
			}
			instance = id
		}
		err := registry.Destroy(ctx, state.Request, instance)
		if errors.Is(err, sandbox.ErrClosed) {
			return effects.ErrLater
		}
		return err
	}
}

// BuilderRunner 返回用 builder 运行子 Agent 的 Host.RunAgent：client 为模型，maxIterations 为子 Agent 的最大轮数。
func BuilderRunner(client llm.Client, maxIterations int) func(context.Context, *Assembly, string) (string, error) {
	return func(ctx context.Context, asm *Assembly, task string) (string, error) {
		b := Configure(builder.NewSimpleBuilder(client), asm).WithMaxIterations(maxIterations)
		if durable.HasJournal(ctx) {
			// 只有后台子工作带日志；执行键已区分每个 child，范围名固定即可。
			// 没有人在等它重新发起，重启后要自己继续，代价是被打断的请求可能重复计费，由预算兜底。
			b = b.WithDurableScope("subagent").WithInterruptedModelResend()
		}
		agent, err := b.Build()
		if err != nil {
			return "", err
		}
		out, err := agent.AsRunnable().Invoke(ctx, task)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(out.Result), nil
	}
}
