package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/observe"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

func planningTestContext(t *testing.T) context.Context {
	t.Helper()
	s, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	b, err := budget.NewDurable(budget.Config{RootKey: "run", Version: "1", MaxModelRequests: 9}, s)
	require.NoError(t, err)
	return observe.WithRunID(budget.WithController(durable.WithJournal(durable.WithExecutionKey(t.Context(), "node"), s), b), "run")
}

func init() {
	Register(Spec[struct{}]{Name: "test-planning-ambiguous", Version: "1", Build: func(_ context.Context, a *Assembler, _ *struct{}) error {
		a.AddTools(tools.NewFunctionTool("external", "执行外部写入", `{"type":"object"}`, func(ctx context.Context, _ map[string]any) (any, error) {
			counter := ctx.Value(ambiguousCounter{}).(*int)
			*counter++
			return nil, agentErrors.ErrEffectUnknown
		}))
		return nil
	}})
	Register(Spec[struct{}]{Name: "test-planning-substitute-write", Version: "1", Build: func(_ context.Context, a *Assembler, _ *struct{}) error {
		a.AddPreHook(func(_ context.Context, c *tools.Call) (tools.PreDecision, error) {
			if c.Tool == "external" {
				return tools.PreDecision{Kind: tools.PreSubstitute, Substitute: &tools.Substitution{Tool: "write_file", Args: map[string]any{"path": "report", "content": "unexpected", "sandbox_permissions": "workspace-write", "justification": "写入报告"}}}, nil
			}
			return tools.PreDecision{}, nil
		})
		return nil
	}})
	alias := workspaceSpec
	alias.Name = "test-planning-workspace"
	Register(alias)
	Register(Spec[struct{}]{Name: "test-planning-lookup", Version: "1", Build: func(_ context.Context, a *Assembler, _ *struct{}) error {
		for _, name := range []string{"search", "browser"} {
			a.AddTools(tools.NewFunctionTool(name, "查询本地固定样本", `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`, func(context.Context, map[string]any) (any, error) { return name, nil }))
		}
		a.AddPreHook(func(_ context.Context, c *tools.Call) (tools.PreDecision, error) {
			if c.Tool == "search" {
				return tools.PreDecision{Kind: tools.PreSubstitute, Substitute: &tools.Substitution{Tool: "browser", Args: map[string]any{"query": "实际调用"}}}, nil
			}
			return tools.PreDecision{}, nil
		})
		return nil
	}})
}

func planningFixture(t *testing.T, f PlanningAgentFactory) (*Assembly, planning.Assignment) {
	t.Helper()
	p := mustGetID(t, "lead", trusted(`presets:
- id: lead
  components: [{id: team, use: planning-members, config: {members: [{id: analyst, preset: child}]}}]
- id: child
  components: [{id: lookup, use: test-planning-lookup}]`))
	target, _ := p.set.Get("child")
	template, err := Build(t.Context(), target, Host{AgentID: "lead/team/analyst"})
	require.NoError(t, err)
	m := planning.Member{ID: "analyst", Version: "v1", ManifestDigest: template.Manifest.Digest(), EffectClass: "read_only"}
	require.NoError(t, template.Close())
	a, err := Build(t.Context(), p, Host{PlanningMembers: []planning.Member{m}, PlanningAgentFactory: f})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })
	return a, planning.Assignment{Task: planning.TaskSpec{ID: "task", MemberID: m.ID, Instruction: "检查"}, GoalContext: planning.GoalContext{Objective: "独立核算", Constraints: []planning.Constraint{{ID: "C1"}}}, GoalDigest: "goal", PlanRevision: 1, AttemptKey: "attempt", MemberVersion: m.Version, ManifestDigest: m.ManifestDigest}
}

type planningRequestModel struct {
	finalModel
	inputs []*llm.CompletionRequest
}

func (m *planningRequestModel) CompleteStream(_ context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	m.inputs = append(m.inputs, r)
	return m.stream(), nil
}
func (m *planningRequestModel) CompleteToolsStream(ctx context.Context, r *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	return m.CompleteStream(ctx, r)
}

func TestExecutorReceivesFrozenGoalContext(t *testing.T) {
	m := &planningRequestModel{finalModel: finalModel{answer: `{"outcome":"produced"}`}}
	asm, a := planningFixture(t, PlanningBuilderFactory(m, 2))
	a.Parameters = []planning.GoalParameter{{Key: "币种", Value: "人民币", Origin: "host"}}
	a.GoalInputs = []planning.SourceInput{{ID: "选中材料", Content: json.RawMessage(`"核算样本"`)}}
	a.Output = planning.OutputProfile{Format: "四列表格"}
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	r, err := e.Execute(planningTestContext(t), a)
	require.NoError(t, err)
	require.Equal(t, a.AttemptKey, r.AttemptKey)
	require.Len(t, m.inputs, 1)
	raw, err := json.Marshal(m.inputs[0].Messages)
	require.NoError(t, err)
	require.Contains(t, string(raw), "独立核算")
	require.Contains(t, string(raw), "C1")
	for _, want := range []string{"人民币", "核算样本", "四列表格"} {
		require.Contains(t, string(raw), want)
	}
}
func TestPlanningExecutorPreservesJournal(t *testing.T) {
	opened, closed := 0, 0
	m := &planningRequestModel{finalModel: finalModel{answer: `{"outcome":"produced"}`}}
	asm, a := planningFixture(t, func(ctx context.Context, asm *Assembly, a planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		require.True(t, durable.HasJournal(ctx))
		require.Equal(t, "run", observe.RunIDFromContext(ctx))
		require.NotEmpty(t, durable.ExecutionKey(ctx))
		opened++
		owned, err := PlanningBuilderFactory(m, 2)(ctx, asm, a)
		closeAgent := owned.Close
		owned.Close = func(ctx context.Context) error { closed++; return closeAgent(ctx) }
		return owned, err
	})
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	ctx := planningTestContext(t)
	for range 2 {
		_, err = e.Execute(ctx, a)
		require.NoError(t, err)
	}
	require.Equal(t, 1, opened)
	require.Equal(t, 1, closed)
	require.Len(t, m.inputs, 1)
	a.ManifestDigest = "changed"
	_, err = e.Execute(ctx, a)
	require.ErrorIs(t, err, ErrManifestChanged)
}

func TestPlanningApprovalBindsGoalPlanAndAttempt(t *testing.T) {
	ctx := durable.AllowInterrupt(planningTestContext(t))
	a := planning.Assignment{GoalDigest: "goal", PlanRevision: 4, AttemptKey: "attempt"}
	req := tools.ApprovalRequest{Call: &tools.Call{ExecutionKey: "call", Tool: "write", Args: map[string]any{"path": "a"}}}
	approver := planningApprover{assignment: a}
	_, err := approver.Approve(ctx, req)
	waits := durable.Waits(err)
	require.Len(t, waits, 1)
	var c durable.InterruptContinuation
	require.NoError(t, json.Unmarshal(waits[0].Continuation, &c))
	var info map[string]any
	require.NoError(t, json.Unmarshal(c.Info, &info))
	require.Equal(t, "goal", info["goal_digest"])
	require.EqualValues(t, 4, info["plan_revision"])
	require.Equal(t, "attempt", info["attempt_key"])
	require.NotEmpty(t, info["action_digest"])
	key := waits[0].Path[len(waits[0].Path)-1].Key
	info["decision"] = "allow"
	answer, err := json.Marshal(info)
	require.NoError(t, err)
	resume := durable.WithResume(ctx, key, string(answer))
	got, err := approver.Approve(resume, req)
	require.NoError(t, err)
	require.Equal(t, tools.ApprovalAllowedOnce, got.Outcome)
	req.Call.Args["path"] = "b"
	_, err = approver.Approve(resume, req)
	require.Error(t, err)
}

func TestPlanningPermissionRevokedBeforeWrite(t *testing.T) {
	src := trusted(`presets:
- id: lead
  components:
  - {id: w, use: workspace, config: {executor: native}}
  - {id: c, use: coding-tools, config: {tools: [write_file]}}
  - {id: team, use: planning-members, config: {members: [{id: writer, preset: child}]}}
- id: child
  components:
  - {id: w, use: workspace, config: {executor: native}}
  - {id: c, use: coding-tools, config: {tools: [write_file]}}`)
	p := mustGetID(t, "lead", src)
	target, _ := p.set.Get("child")
	root := t.TempDir()
	h := Host{Root: root, AgentID: "lead/team/writer"}
	template, err := Build(t.Context(), target, h)
	require.NoError(t, err)
	member := planning.Member{ID: "writer", Version: "1", ManifestDigest: template.Manifest.Digest(), EffectClass: "write", Claims: []planning.ResourceClaim{{Key: "workspace", Mode: "write"}}}
	require.NoError(t, template.Close())
	var closed int

	h.AgentID = "lead"
	h.PlanningMembers = []planning.Member{member}
	h.PlanningAgentFactory = func(_ context.Context, child *Assembly, a planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
			out, err := child.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "write_file", Input: &interfaces.ToolInput{CallID: "write-once", Args: map[string]any{"path": "proof.txt", "content": "hello"}}})
			if err != nil {
				return nil, err
			}
			if out.Success {
				return nil, fmt.Errorf("写操作越过已撤销的权限")
			}
			return &builder.AgentOutput{Result: `{"outcome":"rejected","reason":"权限已撤销"}`}, nil
		})
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { closed++; return nil }}, nil
	}
	asm, err := Build(t.Context(), p, h)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	a := planning.Assignment{Task: planning.TaskSpec{ID: "t", MemberID: "writer"}, GoalDigest: "g", AttemptKey: "attempt", PlanRevision: 1, MemberVersion: "1", ManifestDigest: member.ManifestDigest, Claims: member.Claims}
	ctx := tools.WithSessionID(planningTestContext(t), "session")
	_, err = e.Execute(ctx, a)
	waits := durable.Waits(err)
	require.Len(t, waits, 1, "%v", err)
	require.Equal(t, 1, closed)
	require.DirExists(t, root)
	var cont durable.InterruptContinuation
	require.NoError(t, json.Unmarshal(waits[0].Continuation, &cont))
	var ans map[string]any
	require.NoError(t, json.Unmarshal(cont.Info, &ans))
	require.Equal(t, sha256Hex(`{"content":"hello","path":"proof.txt"}`), ans["args_digest"])
	ans["decision"] = "allow"
	raw, err := json.Marshal(ans)
	require.NoError(t, err)
	require.NoError(t, asm.Permissions.SetSession(ctx, "session", sandbox.ReadOnly))
	path := waits[0].Path
	_, err = e.Execute(durable.WithResume(ctx, path[len(path)-1].Key, string(raw)), a)
	require.ErrorIs(t, err, sandbox.ErrDenied)
	require.NoFileExists(t, filepath.Join(root, "proof.txt"))
	require.DirExists(t, root)
	require.Equal(t, 2, closed)
}

func TestUnknownToolDoesNotGuessSubstitute(t *testing.T) {
	asm, a := planningFixture(t, func(ctx context.Context, child *Assembly, a planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
			_, err := child.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "unregistered_browser", Input: &interfaces.ToolInput{CallID: "browser", Args: map[string]any{}}})
			return nil, err
		})
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { return nil }}, nil
	})
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	_, err = e.Execute(planningTestContext(t), a)
	require.ErrorContains(t, err, "unregistered_browser")
}

func TestPlanningAgentFactoryRequiresOwnedCleanup(t *testing.T) {
	invoked := false
	asm, a := planningFixture(t, func(context.Context, *Assembly, planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: core.NewRunnableFunc(func(context.Context, any) (*builder.AgentOutput, error) {
			invoked = true
			return &builder.AgentOutput{Result: `{"outcome":"produced"}`}, nil
		})}, nil
	})
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	_, err = e.Execute(planningTestContext(t), a)
	require.Error(t, err)
	require.False(t, invoked)
}

func TestPlanningRejectsUnsafeMembers(t *testing.T) {
	for _, tc := range []struct {
		name, rows, parent, effect string
		claims                     []planning.ResourceClaim
		want                       string
	}{
		{"规划模式", "[{id: plan, use: plan-mode}]", "", "read_only", nil, "plan-mode"},
		{"递归成员", "[{id: members, use: planning-members, config: {members: [{id: nested, preset: leaf}]}}]", "", "read_only", nil, "planning-members"},
		{"后台子代理", "[{id: bg, use: subagent, config: {preset: leaf, mode: background}}]", "", "read_only", nil, "subagent"},
		{"前台子代理", "[{id: sub, use: subagent, config: {preset: leaf}}]", "", "read_only", nil, "subagent"},
		{"托管沙箱", "[{id: w, use: workspace, config: {executor: sandbox, provider: ssh, image: test}}]", "", "write", []planning.ResourceClaim{{Key: "db", Mode: "write"}}, "native"},
		{"临时容器", "[{id: w, use: workspace, config: {executor: docker, image: local}}]", "", "write", []planning.ResourceClaim{{Key: "db", Mode: "write"}}, "native"},
		{"未声明实际工作区", "[{id: alias, use: test-planning-workspace, config: {executor: native}}]", "", "read_only", nil, "声明资源"},
		{"缺少声明", "[{id: w, use: workspace, config: {executor: native}}]", "", "read_only", nil, "声明资源"},
		{"空效果", "[]", "", "", nil, "EffectClass"},
		{"写缺少声明", "[]", "", "write", nil, "资源声明"},
		{"突破上限", "[{id: w, use: workspace, config: {executor: native}}]", "{id: w, use: workspace, config: {executor: native, sandbox: read-only}},", "write", []planning.ResourceClaim{{Key: "dir", Mode: "write"}}, "宽于父"},
		{"清单不符", "[]", "", "read_only", nil, "摘要"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := trusted("presets: [{id: lead, components: [" + tc.parent + "{id: members, use: planning-members, config: {members: [{id: m, preset: child}]}}]}, {id: child, components: " + tc.rows + "}, {id: leaf, components: []}]")
			m := planning.Member{ID: "m", Version: "1", ManifestDigest: "wrong", EffectClass: tc.effect, Claims: tc.claims}
			_, err := Build(t.Context(), mustGetID(t, "lead", src), Host{Root: t.TempDir(), PlanningMembers: []planning.Member{m}, PlanningAgentFactory: PlanningBuilderFactory(finalModel{}, 1)})
			require.ErrorContains(t, err, tc.want)
		})
	}
	_, err := Load(project(`presets: [{id: lead, components: [{id: team, use: planning-members, config: {members: [{id: m, preset: child, effectClass: read_only}]}}]}, {id: child, components: []}]`))
	require.ErrorContains(t, err, "effectClass")
}

func TestPlanningDirectoryIsolatedAndRequired(t *testing.T) {
	asm, err := Build(t.Context(), mustGet(t, trusted("presets: [{id: p, components: []}]")), Host{})
	require.NoError(t, err)
	_, err = asm.PlanningExecutor()
	require.ErrorContains(t, err, "planning-members")
	require.Empty(t, asm.PlanningMembers())
	p := mustGetID(t, "lead", trusted("presets: [{id: lead, components: [{id: team, use: planning-members, config: {members: [{id: m, preset: child}]}}]}, {id: child, components: []}]"))
	target, _ := p.set.Get("child")
	template, err := Build(t.Context(), target, Host{AgentID: "lead/team/m"})
	require.NoError(t, err)
	m := planning.Member{ID: "m", Version: "v2", ManifestDigest: template.Manifest.Digest(), EffectClass: "write", Claims: []planning.ResourceClaim{{Key: "db", Mode: "write"}}, Capabilities: []string{"reports"}}
	h := Host{PlanningMembers: []planning.Member{m}, PlanningAgentFactory: PlanningBuilderFactory(finalModel{}, 1)}
	asm, err = Build(t.Context(), p, h)
	require.NoError(t, err)
	h.PlanningMembers[0].Claims[0].Mode = "read"
	h.PlanningMembers[0].Capabilities[0] = "changed"
	got := asm.PlanningMembers()
	require.Equal(t, "write", got[0].Claims[0].Mode)
	require.Equal(t, "reports", got[0].Capabilities[0])
	got[0].Claims[0].Mode = "read"
	got[0].Capabilities[0] = "another"
	require.Equal(t, "write", asm.PlanningMembers()[0].Claims[0].Mode)
	require.Equal(t, "reports", asm.PlanningMembers()[0].Capabilities[0])
}

func TestPlanningManifestCheckedOnWaitReentry(t *testing.T) {
	p := mustGetID(t, "lead", trusted(`presets:
- id: lead
  components: [{id: team, use: planning-members, config: {members: [{id: reader, preset: child}]}}]
- id: child
  components:
  - {id: w, use: workspace, config: {executor: native, sandbox: read-only}}
  - {id: rules, use: instructions}`))
	root := t.TempDir()
	path := filepath.Join(root, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("最初的约束"), 0600))
	child, _ := p.set.Get("child")
	template, err := Build(t.Context(), child, Host{Root: root, AgentID: "lead/team/reader"})
	require.NoError(t, err)
	m := planning.Member{ID: "reader", Version: "1", ManifestDigest: template.Manifest.Digest(), EffectClass: "read_only", Claims: []planning.ResourceClaim{{Key: "dir", Mode: "read"}}}
	require.NoError(t, template.Close())
	opened, closed := 0, 0
	h := Host{Root: root, PlanningMembers: []planning.Member{m}, PlanningAgentFactory: func(context.Context, *Assembly, planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		opened++
		run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
			_, err := durable.AwaitInput(ctx, "hold", json.RawMessage(`{}`), nil)
			return &builder.AgentOutput{Result: `{"outcome":"produced"}`}, err
		})
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { closed++; return nil }}, nil
	}}
	asm, err := Build(t.Context(), p, h)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	a := planning.Assignment{Task: planning.TaskSpec{ID: "t", MemberID: m.ID}, AttemptKey: "attempt", PlanRevision: 1, GoalDigest: "g", ManifestDigest: m.ManifestDigest, MemberVersion: m.Version, Claims: m.Claims}
	ctx := planningTestContext(t)
	_, err = e.Execute(ctx, a)
	waits := durable.Waits(err)
	require.Len(t, waits, 1, "%v", err)
	require.NoError(t, os.WriteFile(path, []byte("变更的约束"), 0600))
	steps := waits[0].Path
	_, err = e.Execute(durable.WithResume(ctx, steps[len(steps)-1].Key, "true"), a)
	require.ErrorIs(t, err, ErrManifestChanged)
	require.Equal(t, 1, opened)
	require.Equal(t, 1, closed)
}

func TestPlanningIgnoresStaleApprovalAnswer(t *testing.T) {
	ctx := durable.AllowInterrupt(planningTestContext(t))
	req := tools.ApprovalRequest{Call: &tools.Call{ExecutionKey: "call", Tool: "write", Args: map[string]any{"path": "file"}}}
	original := planning.Assignment{GoalDigest: "g", PlanRevision: 1, AttemptKey: "a"}
	approver := planningApprover{original}
	_, err := approver.Approve(ctx, req)
	waits := durable.Waits(err)
	require.Len(t, waits, 1)
	var c durable.InterruptContinuation
	require.NoError(t, json.Unmarshal(waits[0].Continuation, &c))
	var info map[string]any
	require.NoError(t, json.Unmarshal(c.Info, &info))
	info["decision"] = "allow"
	raw, err := json.Marshal(info)
	require.NoError(t, err)
	for _, f := range []func(*planning.Assignment){func(a *planning.Assignment) { a.GoalDigest = "other" }, func(a *planning.Assignment) { a.PlanRevision++ }, func(a *planning.Assignment) { a.AttemptKey = "other" }} {
		changed := original
		f(&changed)
		bridge := planningApprover{changed}
		_, err := bridge.Approve(ctx, req)
		newWait := durable.Waits(err)
		require.Len(t, newWait, 1)
		steps := newWait[0].Path
		key := steps[len(steps)-1].Key
		_, err = bridge.Approve(durable.WithResume(ctx, key, string(raw)), req)
		rejected := durable.Waits(err)
		require.Len(t, rejected, 1)
		require.NoError(t, json.Unmarshal(rejected[0].Continuation, &c))
		require.True(t, c.Rejected)
	}
}

func TestPlanningExplicitSubstitutionRejectedBeforeApproval(t *testing.T) {
	asm, a := planningFixture(t, func(_ context.Context, child *Assembly, _ planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
			out, err := child.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "search", Input: &interfaces.ToolInput{CallID: "lookup", Args: map[string]any{"query": "原先请求"}}})
			require.NoError(t, err)
			message, rejected := tools.RejectedResult(out)
			require.True(t, rejected)
			require.Contains(t, message, "规划成员不支持工具替代")
			return &builder.AgentOutput{Result: `{"outcome":"produced"}`}, err
		})
		return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { return nil }}, nil
	})
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	_, err = e.Execute(planningTestContext(t), a)
	require.NoError(t, err)
}

type interruptedPlanningModel struct {
	finalModel
	started chan struct{}
	calls   int
}

func (m *interruptedPlanningModel) CompleteToolsStream(ctx context.Context, _ *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	m.calls++
	if m.calls == 1 {
		close(m.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return m.stream(), nil
}
func (m *interruptedPlanningModel) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return m.CompleteToolsStream(ctx, r, nil)
}

func TestPlanningBuilderLeavesInterruptedModelForHost(t *testing.T) {
	m := &interruptedPlanningModel{started: make(chan struct{}), finalModel: finalModel{answer: "意外重发"}}
	factory := PlanningBuilderFactory(m, 2)
	ctx := planningTestContext(t)
	asm := &Assembly{Manifest: Manifest{AgentID: "member"}}
	run, err := factory(ctx, asm, planning.Assignment{})
	require.NoError(t, err)
	stopping, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { _, err := run.Runnable.Invoke(stopping, "同一请求"); done <- err }()
	select {
	case <-m.started:
	case <-time.After(5 * time.Second):
		t.Fatal("模型未接到请求")
	}
	stop()
	require.ErrorIs(t, <-done, context.Canceled)
	require.NoError(t, run.Close(ctx))
	run, err = factory(ctx, asm, planning.Assignment{})
	require.NoError(t, err)
	_, err = run.Runnable.Invoke(ctx, "同一请求")
	require.ErrorIs(t, err, agentErrors.ErrOperationAttention)
	require.Equal(t, 1, m.calls)
	require.NoError(t, run.Close(ctx))
}

func TestPlanningExecutorHonorsFrozenArtifactSize(t *testing.T) {
	content := strings.Repeat("A", 70000)
	raw, err := json.Marshal(map[string]any{"outcome": "produced", "artifacts": []map[string]any{{"name": "report", "media_type": "application/json", "content": content}}})
	require.NoError(t, err)
	m := &planningRequestModel{finalModel: finalModel{answer: string(raw)}}
	asm, a := planningFixture(t, PlanningBuilderFactory(m, 2))
	a.Task.Outputs = []planning.OutputSpec{{Name: "report", MediaType: "application/json", MaxBytes: 80000}}
	e, err := asm.PlanningExecutor()
	require.NoError(t, err)
	out, err := e.Execute(planningTestContext(t), a)
	require.NoError(t, err)
	require.Equal(t, "produced", out.Outcome)
	limited, err := planning.ValidateTaskResult(a, out, planning.Config{MaxArtifactBytes: 65000})
	require.NoError(t, err)
	require.Equal(t, "size_limit", limited.FailureKind)
}

type sizeTestPlanner struct{ task planning.TaskSpec }

func (p sizeTestPlanner) Plan(context.Context, planning.PlanningInput) (planning.PlanDraft, error) {
	return planning.PlanDraft{Tasks: []planning.TaskSpec{p.task}}, nil
}

type sizeTestReviewer struct{}

func (sizeTestReviewer) Review(_ context.Context, in planning.ReviewInput) (planning.Review, error) {
	r := planning.Review{SubjectDigest: in.SubjectDigest, Verdict: "accept", Reason: "符合大小约定"}
	criterion := "local"
	if in.Final {
		criterion = "goal"
	}
	res := in.Results[0]
	if res.Outcome == "rejected" {
		r.Verdict = "block"
		r.Reason = res.Reason
		r.Criteria = []planning.CriterionResult{{CriterionID: criterion, Status: "fail"}}
		r.Issues = []planning.Issue{{ID: "size", Category: "execution_rejected", Severity: "error", Reason: res.Reason, TaskID: res.TaskID, AttemptKey: res.AttemptKey, Location: "report"}}
		r.Repair = &planning.RepairDecision{Action: "block", IssueIDs: []string{"size"}}
	} else {
		r.Criteria = []planning.CriterionResult{{CriterionID: criterion, Status: "pass", EvidenceIDs: []string{res.Artifacts[0].ID}}}
	}
	return r, nil
}
func TestPlanningGraphUsesActualGlobalArtifactLimits(t *testing.T) {
	content := strings.Repeat("A", 70000)
	result, err := json.Marshal(map[string]any{"outcome": "produced", "artifacts": []map[string]any{{"name": "report", "media_type": "application/json", "content": content}}})
	require.NoError(t, err)
	goal := planning.GoalSpec{ID: "g", Revision: 1, Objective: "生成报告", Scope: planning.Scope{Included: []string{"报告"}}, Output: planning.OutputProfile{Language: "zh", Format: "json"}, Criteria: []planning.Criterion{{ID: "goal", Description: "报告完整"}}, Deliverables: []planning.Deliverable{{ID: "doc", Description: "报告", MediaType: "application/json"}}}
	task := planning.TaskSpec{ID: "new:report", MemberID: "analyst", Instruction: "生成报告", CriterionIDs: []string{"goal"}, Criteria: []planning.Criterion{{ID: "local", Description: "输出符合约定"}}, Outputs: []planning.OutputSpec{{Name: "report", MediaType: "application/json", DeliverableID: "doc", MaxBytes: 80000}}}
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{{"放宽", 90000, "succeeded"}, {"收紧", 68000, "blocked"}} {
		t.Run(tc.name, func(t *testing.T) {
			m := &planningRequestModel{finalModel: finalModel{answer: string(result)}}
			asm, _ := planningFixture(t, PlanningBuilderFactory(m, 2))
			exec, err := asm.PlanningExecutor()
			require.NoError(t, err)
			cfg := planning.Config{Version: "1", Members: asm.PlanningMembers(), MaxArtifactBytes: 80000, MaxArtifactTotalBytes: tc.limit}
			deps := planning.Dependencies{Planner: sizeTestPlanner{task}, Executor: exec, Reviewer: sizeTestReviewer{}, PlannerVersion: "1", ReviewerVersion: "1"}
			ctx := planningTestContext(t)
			graph, err := planning.NewGraph(cfg, deps)
			require.NoError(t, err)
			dc, err := planning.DurableConfig(cfg, deps, durable.JournalStore(ctx))
			require.NoError(t, err)
			compiled, err := graph.CompileDurable(ctx, dc)
			require.NoError(t, err)
			snap, err := compiled.Run(ctx, "run", planning.State{Goal: goal}, "")
			require.NoError(t, err)
			require.True(t, snap.Done)
			require.Equal(t, tc.want, snap.State.Outcome.Status, snap.State.Outcome.Reason)
			require.Len(t, m.inputs, 1)
			if tc.want == "blocked" {
				require.Equal(t, "size_limit", snap.State.Attempts[0].Result.FailureKind)
				require.Empty(t, snap.State.Accepted)
			} else {
				require.Len(t, snap.State.Accepted, 1)
				require.Len(t, snap.State.Accepted[0].Content, len(content)+2)
			}
		})
	}
}

func TestPlanningSingleCallEscalationResumesOnlyBeforeWrite(t *testing.T) {
	for _, mode := range []string{"批准", "撤销", "回执丢失", "替代"} {
		t.Run(mode, func(t *testing.T) {
			p := mustGetID(t, "lead", trusted(`presets:
- id: lead
  components:
  - {id: sub, use: test-planning-substitute-write}
  - {id: w, use: workspace, config: {executor: native}}
  - {id: c, use: coding-tools, config: {tools: [write_file]}}
  - {id: perms, use: permissions, config: {rules: [{tool: write_file, decision: allow}]}}
  - {id: team, use: planning-members, config: {members: [{id: writer, preset: child}]}}
- id: child
  components:
  - {id: src, use: test-planning-ambiguous}
  - {id: w, use: workspace, config: {executor: native, sandbox: read-only, maxSandbox: workspace-write}}
  - {id: c, use: coding-tools, config: {tools: [write_file]}}
  - {id: perms, use: permissions, config: {rules: [{tool: write_file, decision: allow}]}}`))
			root := t.TempDir()
			child, _ := p.set.Get("child")
			template, err := Build(t.Context(), child, Host{Root: root, AgentID: "lead/team/writer"})
			require.NoError(t, err)
			m := planning.Member{ID: "writer", Version: "1", ManifestDigest: template.Manifest.Digest(), EffectClass: "write", Claims: []planning.ResourceClaim{{Key: "dir", Mode: "write"}}}
			require.NoError(t, template.Close())
			h := Host{Root: root, PlanningMembers: []planning.Member{m}, PlanningAgentFactory: func(_ context.Context, child *Assembly, _ planning.Assignment) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
				run := core.NewRunnableFunc(func(ctx context.Context, _ any) (*builder.AgentOutput, error) {
					if mode == "替代" {
						out, err := child.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "external", Input: &interfaces.ToolInput{CallID: "source", Args: map[string]any{}}})
						require.NoError(t, err)
						message, rejected := tools.RejectedResult(out)
						require.True(t, rejected)
						require.Contains(t, message, "规划成员不支持工具替代")
						return &builder.AgentOutput{Result: `{"outcome":"rejected","reason":"工具替代被拒绝"}`}, nil
					}
					out, err := child.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "write_file", Input: &interfaces.ToolInput{CallID: "write", Args: map[string]any{"path": "report", "content": "approved", "sandbox_permissions": "workspace-write", "justification": "生成约定输出"}}})
					if err != nil {
						return nil, err
					}
					if !out.Success {
						return &builder.AgentOutput{Result: `{"outcome":"rejected","reason":"权限已拒绝"}`}, nil
					}
					return &builder.AgentOutput{Result: `{"outcome":"produced"}`}, nil
				})
				return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: run, Close: func(context.Context) error { return nil }}, nil
			}}
			asm, err := Build(t.Context(), p, h)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, asm.Close()) })
			exec, err := asm.PlanningExecutor()
			require.NoError(t, err)
			a := planning.Assignment{Task: planning.TaskSpec{ID: "t", MemberID: m.ID}, MemberVersion: m.Version, ManifestDigest: m.ManifestDigest, Claims: m.Claims, PlanRevision: 1, GoalDigest: "g", AttemptKey: "attempt"}
			ctx := tools.WithSessionID(planningTestContext(t), "session")
			store := &lostWriteReceipt{Store: durable.JournalStore(ctx), armed: mode == "回执丢失"}
			ctx = durable.WithJournal(ctx, store)
			_, err = exec.Execute(ctx, a)
			if mode == "替代" {
				require.NoError(t, err)
				require.NoFileExists(t, filepath.Join(root, "report"))
				return
			}
			waits := durable.Waits(err)
			require.Len(t, waits, 1, "%v", err)
			require.NoFileExists(t, filepath.Join(root, "report"))
			var cont durable.InterruptContinuation
			require.NoError(t, json.Unmarshal(waits[0].Continuation, &cont))
			var answer map[string]any
			require.NoError(t, json.Unmarshal(cont.Info, &answer))
			answer["decision"] = "allow"
			raw, err := json.Marshal(answer)
			require.NoError(t, err)
			if mode == "撤销" {
				require.NoError(t, asm.Permissions.SetSession(ctx, "session", sandbox.ReadOnly))
			}
			path := waits[0].Path
			res, err := exec.Execute(durable.WithResume(ctx, path[len(path)-1].Key, string(raw)), a)
			if mode == "回执丢失" {
				require.ErrorContains(t, err, "模拟持久存储离线")
				require.True(t, store.dropped)
				file := filepath.Join(root, "report")
				body, err := os.ReadFile(file)
				require.NoError(t, err)
				require.Equal(t, "approved", string(body))
				require.NoError(t, os.WriteFile(file, []byte("宿主核对中"), 0600))
				store.armed = false
				store.dropped = false
				_, err = exec.Execute(ctx, a)
				require.ErrorIs(t, err, agentErrors.ErrOperationAttention)
				body, err = os.ReadFile(file)
				require.NoError(t, err)
				require.Equal(t, "宿主核对中", string(body))
				return
			}
			require.NoError(t, err)
			if mode == "撤销" {
				require.Equal(t, "rejected", res.Outcome)
				require.NoFileExists(t, filepath.Join(root, "report"))
			} else {
				require.Equal(t, "produced", res.Outcome)
				body, err := os.ReadFile(filepath.Join(root, "report"))
				require.NoError(t, err)
				require.Equal(t, "approved", string(body))
			}
		})
	}
}

func TestPlanningWriteMemberNeverRetriesUndeclaredAmbiguousEffect(t *testing.T) {
	var calls int
	// 注入未显式声明持久策略的第三方工具，以未知效果模拟回执丢失。
	p := mustGetID(t, "lead", trusted(`presets:
- id: lead
  components: [{id: team, use: planning-members, config: {members: [{id: m, preset: child}]}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]
- id: child
  components: [{id: third, use: test-planning-ambiguous}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]`))
	child, _ := p.set.Get("child")
	template, err := Build(t.Context(), child, Host{AgentID: "lead/team/m"})
	require.NoError(t, err)
	m := planning.Member{ID: "m", Version: "1", ManifestDigest: template.Manifest.Digest(), EffectClass: "write", Claims: []planning.ResourceClaim{{Key: "external", Mode: "write"}}}
	require.NoError(t, template.Close())
	asm, err := Build(t.Context(), p, Host{PlanningMembers: []planning.Member{m}, PlanningAgentFactory: PlanningBuilderFactory(finalModel{}, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	ctx := context.WithValue(planningTestContext(t), ambiguousCounter{}, &calls)
	for range 2 {
		instance, err := asm.planning.children["m"].build(ctx, planning.Assignment{})
		require.NoError(t, err)
		_, err = instance.Dispatcher.Invoke(ctx, tools.ToolRequest{Name: "external", Input: &interfaces.ToolInput{CallID: "same", Args: map[string]any{}}})
		require.ErrorIs(t, err, agentErrors.ErrOperationAttention)
		require.NoError(t, instance.Close())
	}
	require.Equal(t, 1, calls)
}

type ambiguousCounter struct{}

type lostWriteReceipt struct {
	durable.Store
	armed, dropped bool
}

func (s *lostWriteReceipt) Save(ctx context.Context, id string, snap *durable.Snapshot) error {
	if s.armed {
		var r durable.OperationRecord
		if json.Unmarshal(snap.Data, &r) == nil && r.ID == "tool/write" && r.Status == "completed" {
			s.dropped = true
		}
		if s.dropped {
			return errors.New("模拟持久存储离线")
		}
	}
	return s.Store.Save(ctx, id, snap)
}
