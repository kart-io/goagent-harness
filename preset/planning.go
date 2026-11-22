package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
)

// ComponentPlanningMembers registers a trusted planning member directory.
const ComponentPlanningMembers = "planning-members"

// PlanningMembersConfig selects the member presets available to a planner.
type PlanningMembersConfig struct {
	Members []PlanningMemberConfig `yaml:"members"`
}

// PlanningMemberConfig binds a host-owned member ID to a filtered preset.
// The member's agent identity is parent-agent-ID/component-row-ID/member-ID.
type PlanningMemberConfig struct {
	ID         string     `yaml:"id"`
	Preset     string     `yaml:"preset"`
	ToolFilter ToolFilter `yaml:"toolFilter"`
}

// PlanningAgentFactory opens one owned session for a frozen assignment.
// Both the agent and its cleanup belong to the caller. The supplied context
// carries the original run, budget, and the journal for this attempt.
type PlanningAgentFactory func(context.Context, *Assembly, planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error)

// PlanningBuilderFactory builds a fresh agent with a stable journal scope and member identity.
func PlanningBuilderFactory(client llm.Client, maxIterations int) PlanningAgentFactory {
	return func(ctx context.Context, asm *Assembly, _ planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		b := Configure(builder.NewSimpleBuilder(client), asm).WithMaxIterations(maxIterations).WithDurableScope("planning-member").WithAgentID(asm.Manifest.AgentID)
		if session := tools.SessionIDFrom(ctx); session != "" {
			b = b.WithSessionID(session)
		}
		agent, err := b.Build()
		if err != nil {
			return tools.OwnedRunnable[any, *builder.AgentOutput]{}, err
		}
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: agent.AsRunnable(), Close: agent.Shutdown}, nil
	}
}

type planningChild struct {
	member planning.Member
	target *Resolved
	host   Host
	opts   buildOptions
}
type planningExecutor struct {
	directory []planning.Member
	children  map[string]planningChild
}

// PlanningExecutor returns the configured executor for frozen assignments.
// For a planning approval, the host authenticates the respondent and resumes
// the leaf interrupt key with its Info object plus "decision": "allow" or "deny".
// Approval binds one concrete tool action and never grants session permission.
// The executor enforces frozen per-output bounds. The graph or a direct caller
// must apply its configured global artifact limits before committing the result.
// Write members get a single attempt for tools without an explicit journal policy.
// Native sandbox escalations use the planning-native-approval-v1 journal intent:
// a recorded approval wait may resume, an interrupted write needs reconciliation.
func (a *Assembly) PlanningExecutor() (planning.Executor, error) {
	if a.planning == nil {
		return nil, errors.New("preset 缺少 planning-members 组件")
	}
	return a.planning, nil
}

// PlanningMembers returns an independent copy of the trusted member directory.
func (a *Assembly) PlanningMembers() []planning.Member {
	if a.planning == nil {
		return nil
	}
	members := slices.Clone(a.planning.directory)
	for i := range members {
		members[i].Claims = slices.Clone(members[i].Claims)
		members[i].Capabilities = slices.Clone(members[i].Capabilities)
	}
	return members
}

func init() {
	Register(Spec[PlanningMembersConfig]{Name: ComponentPlanningMembers, Version: "1", Singleton: true, Validate: func(c *PlanningMembersConfig) error {
		seen := map[string]bool{}
		for _, m := range c.Members {
			if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Preset) == "" || seen[m.ID] {
				return fmt.Errorf("成员 %q 缺少配置或重复", m.ID)
			}
			seen[m.ID] = true
		}
		if len(seen) == 0 {
			return errors.New("规划成员目录为空")
		}
		return nil
	}, References: func(c *PlanningMembersConfig) []string {
		var refs []string
		for _, m := range c.Members {
			refs = append(refs, m.Preset)
		}
		return refs
	}, Build: buildPlanningMembers})
}

func buildPlanningMembers(ctx context.Context, a *Assembler, c *PlanningMembersConfig) error {
	if a.host.PlanningAgentFactory == nil {
		return errors.New("规划成员需要 Host.PlanningAgentFactory")
	}
	cfg, err := planning.NormalizeConfig(planning.Config{Version: "1", Members: a.host.PlanningMembers})
	if err != nil {
		return err
	}
	if len(cfg.Members) != len(c.Members) {
		return errors.New("规划成员配置与宿主目录数量不符")
	}
	directory := make(map[string]planning.Member, len(cfg.Members))
	for _, m := range cfg.Members {
		directory[m.ID] = m
	}
	slot := a.cur
	e := &planningExecutor{directory: cfg.Members, children: map[string]planningChild{}}
	a.AfterPublish(func(parent *tools.Pipeline) error {
		for _, item := range c.Members {
			m, ok := directory[item.ID]
			if !ok {
				return fmt.Errorf("宿主目录缺少成员 %s", item.ID)
			}
			target, ok := a.preset.set.Get(item.Preset)
			if !ok {
				return fmt.Errorf("成员 preset %s 不存在", item.Preset)
			}
			for _, row := range target.Rows {
				switch row.Use {
				case ComponentPlanMode, ComponentPlanningMembers, ComponentSubagent:
					return fmt.Errorf("规划成员不支持组件 %s", row.Use)
				case ComponentWorkspace:
					w := row.config.(*WorkspaceConfig)
					if w.Executor != "native" || a.host.Root == "" {
						return errors.New("规划成员需要宿主持久的 native 工作区")
					}
					if len(m.Claims) == 0 {
						return errors.New("有工作区的规划成员必须声明资源")
					}
				}
			}
			host, opts := a.childSettings(parent, target, a.preset.Rows[slot].ID+"/"+item.ID, item.ToolFilter, false)
			child := planningChild{member: m, target: target, host: host, opts: opts}
			template, err := child.build(ctx, planning.Assignment{})
			if err != nil {
				return err
			}
			if err = template.Close(); err != nil {
				return err
			}
			e.children[m.ID] = child
		}
		a.planning = e
		info, err := json.Marshal(cfg.Members)
		if err != nil {
			return err
		}
		a.contribs[slot].info = map[string]string{"directory": sha256Hex(string(info))}
		return nil
	})
	return nil
}

func (c planningChild) build(ctx context.Context, a planning.Assignment) (*Assembly, error) {
	host := c.host
	// 目录中的写成员可能持有无专用核对协议的工具。未声明策略时不重试未知效果。
	if c.member.EffectClass == "write" {
		host.Dispatch.MaxAttempts = 1
	}
	host.Approver = planningApprover{a}
	asm, err := build(ctx, c.target, host, c.opts)
	if err != nil {
		return nil, err
	}
	if asm.perRun {
		return nil, errors.Join(errors.New("规划成员不能使用临时工作区"), asm.Close())
	}
	if w := asm.Manifest.Workspace; w != nil {
		if len(c.member.Claims) == 0 {
			return nil, errors.Join(errors.New("有工作区的规划成员必须声明资源"), asm.Close())
		}
		if w.Executor != "native" || w.Root == "" || w.Root != host.Root {
			return nil, errors.Join(errors.New("规划成员的实际工作区与宿主核验的 native 目录不符"), asm.Close())
		}
	}
	if actual := asm.Manifest.Digest(); actual != c.member.ManifestDigest {
		return nil, errors.Join(fmt.Errorf("%w：成员 %s，实际摘要 %s", ErrManifestChanged, c.member.ID, actual), asm.Close())
	}
	for i, hook := range asm.Pipeline.Pre {
		asm.Pipeline.Pre[i] = rejectPlanningSubstitution(hook)
	}
	return asm, nil
}

// open 将会话与本次装配交给同一个所有者，包含部分构建失败的清理。
func (c planningChild) open(ctx context.Context, a planning.Assignment) (owned tools.OwnedRunnable[any, *builder.AgentOutput], err error) {
	asm, err := c.build(ctx, a)
	if err != nil {
		return owned, err
	}
	// 工厂发生 panic 时，外层工具负责报告异常，当前装配仍须断开。
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, asm.Close())
		}
	}()
	if err = asm.PinManifest(ctx); err != nil {
		return owned, err
	}
	owned, err = c.host.PlanningAgentFactory(ctx, asm, a)
	closeAgent := owned.Close
	if closeAgent == nil {
		if err == nil {
			err = errors.New("规划成员工厂未提供会话关闭函数")
		}
		return owned, err
	}
	owned.Close = func(ctx context.Context) (closeErr error) {
		defer func() { closeErr = errors.Join(closeErr, asm.Close()) }()
		return closeAgent(ctx)
	}
	transferred = true
	return owned, err
}

const taskInstruction = "完成冻结任务，遵守目标约束、范围和输出约定，仅使用所选输入作为材料。返回单一 TaskResult JSON 对象，包含 outcome（produced 或 rejected）、artifacts（name、media_type、content、sources）；被拒时提供 reason。身份由宿主填写。"

func (e *planningExecutor) Execute(ctx context.Context, a planning.Assignment) (planning.TaskResult, error) {
	var result planning.TaskResult
	c, ok := e.children[a.Task.MemberID]
	if !ok {
		return result, fmt.Errorf("未配置成员 %q", a.Task.MemberID)
	}
	if a.MemberVersion != c.member.Version || a.ManifestDigest != c.member.ManifestDigest {
		return result, fmt.Errorf("%w：冻结成员版本或摘要不符", ErrManifestChanged)
	}
	if a.AttemptKey == "" || a.GoalDigest == "" || a.PlanRevision <= 0 || a.Task.ID == "" || !slices.Equal(a.Claims, c.member.Claims) {
		return result, errors.New("冻结任务身份或资源声明不符")
	}
	data, err := json.Marshal(a)
	if err != nil {
		return result, err
	}
	factory := func(ctx context.Context) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		return c.open(ctx, a)
	}
	tool, err := tools.NewRunnableToolFactory(factory, tools.RunnableToolConfig[any, *builder.AgentOutput]{Name: "execute", Version: c.member.Version, Description: taskInstruction, ModelInvoking: true, Resumable: true, MaxAttempts: 3,
		ArgsSchema: `{"type":"object","properties":{"assignment":{"type":"string"}},"required":["assignment"],"additionalProperties":false}`,
		Decode: func(args map[string]any) (any, error) {
			return taskInstruction + "\n" + args["assignment"].(string), nil
		},
		Encode: func(out *builder.AgentOutput) (any, error) {
			if out != nil {
				if s, ok := out.Result.(string); ok {
					return s, nil
				}
			}
			return nil, errors.New("规划成员未返回 JSON 文本")
		},
	})
	if err != nil {
		return result, err
	}
	reg := tools.NewRegistry()
	if err = reg.Register(tool); err != nil {
		return result, err
	}
	d, err := tools.NewDispatcher(reg, tools.DispatchConfig{})
	if err != nil {
		return result, err
	}
	response, err := d.Invoke(durable.WithExecutionKey(ctx, a.AttemptKey), tools.ToolRequest{Name: "execute", Input: &interfaces.ToolInput{CallID: "execute", Args: map[string]any{"assignment": string(data)}}})
	if err != nil {
		return result, err
	}
	raw, ok := response.Result.(string)
	if !ok {
		return result, errors.New("规划成员日志缺少结果文本")
	}
	if err = planning.DecodeCandidate([]byte(raw), &result); err != nil {
		return planning.TaskResult{}, err
	}
	result.TaskID = a.Task.ID
	result.AttemptKey = a.AttemptKey
	// 每项输出遵守冻结额度；图在提交前使用本次运行的实际单项和合计额度再次校验。
	return planning.ValidateTaskResult(a, result, planning.Config{MaxArtifactBytes: math.MaxInt, MaxArtifactTotalBytes: math.MaxInt})
}

// 只装饰 native 工作区里已由 permissions 扩充升级参数的工具。原装饰器
// 暴露工具接口、持久策略与超时；普通调用沿用原策略。升级请求只允许从已落盘
// 的 waiting 继续，写入之后若中断，MaxAttempts=1 会保留未知效果。
type planningEscalationTool struct{ interfaces.Tool }

func (t planningEscalationTool) DurableCallPolicy(args map[string]any) (durable.OperationPolicy, bool) {
	var policy durable.OperationPolicy
	var declared bool
	if p, ok := t.Tool.(tools.DurableCallPolicyProvider); ok {
		policy, declared = p.DurableCallPolicy(args)
	}
	if !declared {
		if p, ok := t.Tool.(tools.DurablePolicyProvider); ok {
			policy, declared = p.DurableOperationPolicy(), true
		}
	}
	if _, ok := args["sandbox_permissions"]; ok {
		policy.MaxAttempts = 1
		policy.Resumable = true
		policy.RetryInterrupted = false
		policy.RetryFailed = false
		return policy, true
	}
	return policy, declared
}
func (t planningEscalationTool) DispatchTimeout() time.Duration {
	if p, ok := t.Tool.(tools.DispatchTimeoutProvider); ok {
		return p.DispatchTimeout()
	}
	return 0
}
func (t planningEscalationTool) DurableIntentVersion() string {
	const version = "planning-native-approval-v1"
	if p, ok := t.Tool.(tools.DurableIntentVersioner); ok {
		return p.DurableIntentVersion() + "/" + version
	}
	return version
}

// 替代调用沿用原工具的持久意图、重试及核对策略。必须在进入替代工具之前拒绝。
func rejectPlanningSubstitution(hook tools.PreHook) tools.PreHook {
	return func(ctx context.Context, call *tools.Call) (tools.PreDecision, error) {
		decision, err := hook(ctx, call)
		if err == nil && decision.Kind == tools.PreSubstitute {
			return tools.PreDecision{Kind: tools.PreDeny, Feedback: "规划成员不支持工具替代，请直接调用目标工具"}, nil
		}
		return decision, err
	}
}

type planningApprover struct{ assignment planning.Assignment }

type planningApproval struct {
	GoalDigest   string `json:"goal_digest"`
	PlanRevision int    `json:"plan_revision"`
	AttemptKey   string `json:"attempt_key"`
	ActionDigest string `json:"action_digest"`
	Tool         string `json:"tool"`
	ArgsDigest   string `json:"args_digest"`
}

func (p planningApprover) Approve(ctx context.Context, req tools.ApprovalRequest) (tools.ApprovalResult, error) {
	var result tools.ApprovalResult
	if req.Call == nil || req.Call.ExecutionKey == "" || p.assignment.AttemptKey == "" {
		return result, errors.New("规划审批缺少冻结身份或调用键")
	}
	action, err := json.Marshal(struct {
		Call   *tools.Call
		Reason string
	}{req.Call, req.Reason})
	if err != nil {
		return result, err
	}
	args, err := json.Marshal(req.Call.Args)
	if err != nil {
		return result, err
	}
	// 宿主可用参数摘要核对其受信调用记录，等待快照不暴露参数正文。
	info := planningApproval{GoalDigest: p.assignment.GoalDigest, PlanRevision: p.assignment.PlanRevision, AttemptKey: p.assignment.AttemptKey, ActionDigest: sha256Hex(string(action)), Tool: req.Call.Tool, ArgsDigest: sha256Hex(string(args))}
	data, err := json.Marshal(info)
	if err != nil {
		return result, err
	}
	type answer struct {
		planningApproval
		Decision string `json:"decision"`
	}
	input, err := durable.AwaitInput(durable.WithExecutionKey(ctx, req.Call.ExecutionKey), "planning-approval/"+sha256Hex(string(data)), data, func(raw json.RawMessage) error {
		var r answer
		if err := planning.DecodeCandidate(raw, &r); err != nil || r.planningApproval != info || (r.Decision != ApprovalAllow && r.Decision != ApprovalDeny) {
			return errors.New("审批答复须匹配本次意图和决定")
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	var r answer
	if err = json.Unmarshal(input, &r); err != nil {
		return result, err
	}
	result.Outcome = tools.ApprovalRejected
	if r.Decision == ApprovalAllow {
		result.Outcome = tools.ApprovalAllowedOnce
	}
	return result, nil
}
