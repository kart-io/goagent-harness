package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/tools"
	"github.com/stretchr/testify/require"
)

const managedSet = `presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - id: worker
    components:
     - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}
     - {id: c, use: coding-tools, config: {tools: [read_file]}}
     - {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}`

func fastManager(t *testing.T, dir string, notify func(context.Context, background.Notice) error) *background.Manager {
	t.Helper()
	store, err := durable.NewFileStore(dir)
	require.NoError(t, err)
	m, err := background.New(background.Config{Store: store, Name: "m", Notify: notify, Poll: 5 * time.Millisecond,
		Effects: effects.Config{Poll: 5 * time.Millisecond, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, LaterDelay: 5 * time.Millisecond}})
	require.NoError(t, err)
	return m
}

func waitChild(t *testing.T, m *background.Manager, id string, status background.Status) background.Record {
	t.Helper()
	var last background.Record
	var lastErr error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		last, lastErr = m.Get(context.Background(), id)
		if lastErr == nil && last.Status == status {
			return last
		}
	}
	require.Failf(t, "等待超时", "子工作 %s 没有进入 %s，最后为 %+v，错误 %v", id, status, last, lastErr)
	return last
}

func (p *sandboxFixtureProvider) destroyedSnapshot() map[string]int32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]int32{}
	for id, e := range p.environments {
		out[id] = e.destroyed.Load()
	}
	return out
}

// Build 不分配实例；每个 child 有自己的实例，进程停止时保留，重启后连回同一个，结束后才销毁。
func TestBackgroundSandboxFollowsChildRun(t *testing.T) {
	dir := t.TempDir()
	host, provider := sandboxHost(t)
	entered := make(chan string, 2)
	host.RunAgent = func(ctx context.Context, asm *Assembly, _ string) (string, error) {
		entered <- asm.Manifest.Workspace.SandboxID
		<-ctx.Done()
		return "", ctx.Err()
	}
	first := fastManager(t, dir, (&noticeLog{}).add)
	host.Background = first
	lead := mustGetID(t, "lead", trusted(managedSet))
	asm, err := Build(context.Background(), lead, host)
	require.NoError(t, err)
	require.Empty(t, provider.requests, "Build 不分配实例")
	stop := runManager(t, first)
	spawnTool(t, asm, "child-a")
	idA := <-entered
	spawnTool(t, asm, "child-b")
	idB := <-entered
	require.NotEqual(t, idA, idB, "每个 child 有自己的实例")
	require.NotEqual(t, provider.requests[0].Scope, provider.requests[1].Scope, "Provider 按 Scope 识别实例，不同 child 不能相同")
	stop()
	require.NoError(t, asm.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
	require.Zero(t, provider.environments[idA].destroyed.Load(), "进程停止不是 child 结束")

	// 重启：新 Registry、新 Manager，用同一 Provider。
	restartHost := host
	restartHost.Sandboxes = &sandbox.Registry{Namespace: "job-1", Providers: map[string]sandbox.Provider{"fake": provider}}
	resumed := make(chan string, 2)
	finish := make(chan struct{})
	restartHost.RunAgent = func(ctx context.Context, asm *Assembly, _ string) (string, error) {
		resumed <- asm.Manifest.Workspace.SandboxID
		<-finish
		return "完成", nil
	}
	var log noticeLog
	second := fastManager(t, dir, log.add)
	restartHost.Background = second
	asm, err = Build(context.Background(), lead, restartHost)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	got := []string{<-resumed, <-resumed}
	require.ElementsMatch(t, []string{idA, idB}, got, "连回原实例，不新建")
	require.Len(t, provider.environments, 2)
	require.Zero(t, provider.environments[idA].destroyed.Load())
	close(finish)
	waitChild(t, second, "child-a", background.StatusCompleted)
	waitChild(t, second, "child-b", background.StatusCompleted)
	require.Eventually(t, func() bool {
		d := provider.destroyedSnapshot()
		return d[idA] == 1 && d[idB] == 1
	}, 30*time.Second, 2*time.Millisecond, "child 结束后销毁")
	require.NoError(t, restartHost.Sandboxes.Close(context.Background()))
	require.EqualValues(t, 1, provider.environments[idA].destroyed.Load(), "只销毁一次")
}

// 宿主保留或借出的实例由宿主负责：child 结束不销毁。
func TestBackgroundSandboxKeepsHostRetainedInstance(t *testing.T) {
	host, provider := sandboxHost(t)
	host.Sandboxes.Resolve = func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{Retain: true}, nil }
	host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "完成", nil }
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host.Background = m
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	record := waitChild(t, m, "child", background.StatusCompleted)
	require.Empty(t, record.Resources, "不是 child 持有的资源")
	time.Sleep(30 * time.Millisecond)
	require.Len(t, provider.environments, 1)
	for _, e := range provider.environments {
		require.Zero(t, e.destroyed.Load())
	}
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

func TestBackgroundSandboxKeepsBorrowedInstance(t *testing.T) {
	host, provider := sandboxHost(t)
	borrowed, err := provider.Create(context.Background(), sandbox.Request{Network: false})
	require.NoError(t, err)
	host.Sandboxes.Resolve = func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{ID: borrowed.ID()}, nil }
	var used atomic.Value
	host.RunAgent = func(_ context.Context, asm *Assembly, _ string) (string, error) {
		used.Store(asm.Manifest.Workspace.SandboxID)
		return "完成", nil
	}
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host.Background = m
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	waitChild(t, m, "child", background.StatusCompleted)
	require.Equal(t, borrowed.ID(), used.Load())
	time.Sleep(30 * time.Millisecond)
	require.Zero(t, provider.environments[borrowed.ID()].destroyed.Load(), "不删除借用的实例")
	require.NoError(t, host.Sandboxes.Close(context.Background()))
	require.Zero(t, provider.environments[borrowed.ID()].destroyed.Load())
}

// 中间层没有 workspace 时，祖先的沙箱和网络上限对后台子 Agent 同样有效，且在分配实例之前拒绝。
func TestBackgroundGrandchildCannotLoseAncestorCeilings(t *testing.T) {
	host, provider := sandboxHost(t)
	host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "", nil }
	host.Background = fastManager(t, t.TempDir(), (&noticeLog{}).add)
	p := mustGetID(t, "p", trusted(`presets:
 - id: p
   components:
    - {id: w, use: workspace, config: {executor: docker, image: unused, sandbox: read-only, network: true}}
    - {id: d, use: subagent, config: {preset: middle, maxDepth: 3, mode: background}}
 - id: middle
   components:
    - {id: d, use: subagent, config: {preset: leaf, maxDepth: 3}}
 - id: leaf
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime, sandbox: workspace-write, network: true}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
`))
	_, err := Build(context.Background(), p, host)
	require.ErrorContains(t, err, "宽于父 Agent")
	require.Empty(t, provider.requests)
}

// 中间层的托管沙箱同样跟随 child：孙辈的实例记在同一个 child 上。
func TestBackgroundSandboxNestedUnderMiddleLayer(t *testing.T) {
	host, provider := sandboxHost(t)
	host.RunAgent = func(_ context.Context, asm *Assembly, _ string) (string, error) {
		require.Len(t, asm.Tools, 1)
		return "完成", nil
	}
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host.Background = m
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
 - {id: lead, components: [{id: sub, use: subagent, config: {preset: middle, maxDepth: 3, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
 - id: middle
   components:
    - {id: d, use: subagent, config: {preset: leaf, maxDepth: 3}}
 - id: leaf
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
`)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	require.Empty(t, provider.requests)
	runManager(t, m)
	out, err := byToolName(asm)["agent_middle"].Invoke(context.Background(), &interfaces.ToolInput{ExecutionKey: "child", Args: map[string]any{"task": "x"}})
	require.NoError(t, err)
	require.True(t, out.Success)
	record := waitChild(t, m, "child", background.StatusCompleted)
	require.Len(t, record.Resources, 1)
	require.Eventually(t, func() bool {
		d := provider.destroyedSnapshot()
		return len(d) == 1 && d["fixture-1"] == 1
	}, 30*time.Second, 2*time.Millisecond)
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// 恢复时装配变了：不按新策略继续旧记录。
func TestBackgroundChildRejectsChangedManifestAfterRestart(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	first := fastManager(t, dir, (&noticeLog{}).add)
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(backgroundSet)),
		Host{ArtifactDir: t.TempDir(), Background: first, RunAgent: func(ctx context.Context, _ *Assembly, _ string) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", ctx.Err()
		}})
	require.NoError(t, err)
	stop := runManager(t, first)
	spawnTool(t, asm, "child")
	<-entered
	stop()
	require.NoError(t, asm.Close())

	changed := `presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: agent_worker, decision: allow}]}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: 策略已改变}}]}`
	var runs atomic.Int32
	second := fastManager(t, dir, (&noticeLog{}).add)
	asm, err = Build(context.Background(), mustGetID(t, "lead", trusted(changed)),
		Host{ArtifactDir: t.TempDir(), Background: second, RunAgent: func(context.Context, *Assembly, string) (string, error) {
			runs.Add(1)
			return "不应运行", nil
		}})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	record := waitChild(t, second, "child", background.StatusFailed)
	require.Equal(t, "manifest_changed", record.FailureKind)
	require.Zero(t, runs.Load())
}

// callModel 先按脚本调用工具，再给出答复，并记录收到的工具结果。
type callModel struct {
	finalModel
	mu    sync.Mutex
	calls []llm.ToolCall
	seen  [][]llm.Message
}

func (m *callModel) CompleteToolsStream(_ context.Context, req *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, req.Messages)
	ch := make(chan *llm.StreamChunk, 2)
	if len(m.calls) > 0 {
		call := m.calls[0]
		m.calls = m.calls[1:]
		ch <- &llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{call}}
	} else {
		ch <- &llm.StreamChunk{Delta: m.answer}
		ch <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
	}
	close(ch)
	return ch, nil
}

func toolCall(id, name, args string) llm.ToolCall {
	var call llm.ToolCall
	call.ID, call.Type = id, "function"
	call.Function.Name, call.Function.Arguments = name, args
	return call
}

const approvalSet = `presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - id: worker
    components:
     - ` + localWorkspace + `
     - {id: c, use: coding-tools}
     - {id: perms, use: permissions, config: {rules: [{tool: read_file, decision: ask}]}}`

func approvalHost(t *testing.T, dir string, m *background.Manager, model llm.Client) Host {
	t.Helper()
	return Host{Root: dir, ArtifactDir: t.TempDir(), Background: m, RunAgent: BuilderRunner(model, 5),
		// 宿主的同步审批人不应被后台子 Agent 调用：没有人在等它。
		Approver: tools.ApproverFunc(func(context.Context, tools.ApprovalRequest) (tools.ApprovalResult, error) {
			t.Error("后台子 Agent 不应阻塞在同步审批上")
			return tools.ApprovalResult{Outcome: tools.ApprovalRejected}, nil
		})}
}

// 真实 builder 循环：需要审批的工具让 child 进入等待，重启后不重跑，批准后只执行一次并完成。
func TestBackgroundApprovalWaitsAndResumesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	root := t.TempDir()
	lead := mustGetID(t, "lead", trusted(approvalSet))
	var firstLog noticeLog
	first := fastManager(t, dir, firstLog.add)
	model := &callModel{finalModel: finalModel{answer: "读完了"}, calls: []llm.ToolCall{toolCall("c1", "read_file", `{"path":"missing.txt"}`)}}
	asm, err := Build(context.Background(), lead, approvalHost(t, root, first, model))
	require.NoError(t, err)
	stop := runManager(t, first)
	spawnTool(t, asm, "child")
	record := waitChild(t, first, "child", background.StatusWaiting)
	require.Len(t, record.Waits, 1)
	require.Eventually(t, func() bool { return len(firstLog.all()) == 1 }, 30*time.Second, 2*time.Millisecond)
	require.Equal(t, background.StatusWaiting, firstLog.all()[0].Status)
	approval, err := ApprovalOf(record.Waits[0])
	require.NoError(t, err)
	require.Equal(t, "read_file", approval.Tool)
	require.Contains(t, approval.Args, "missing.txt")
	stop()
	require.NoError(t, asm.Close())

	var log noticeLog
	second := fastManager(t, dir, log.add)
	next := &callModel{finalModel: finalModel{answer: "读完了"}}
	asm, err = Build(context.Background(), lead, approvalHost(t, root, second, next))
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	time.Sleep(30 * time.Millisecond)
	record = waitChild(t, second, "child", background.StatusWaiting)
	key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
	require.ErrorContains(t, second.Resume(context.Background(), "child", map[string]string{"forged": ApprovalAllow}), "forged")
	require.NoError(t, second.Resume(context.Background(), "child", map[string]string{key: ApprovalAllow}))
	record = waitChild(t, second, "child", background.StatusCompleted)
	require.Equal(t, "读完了", record.Result)
	next.mu.Lock()
	defer next.mu.Unlock()
	require.Len(t, next.seen, 1, "已提交的模型回合回放，批准后只再请求一次")
}

func TestBackgroundApprovalDenyAndUnknownInputReject(t *testing.T) {
	for _, input := range []string{ApprovalDeny, "随便什么"} {
		t.Run(input, func(t *testing.T) {
			m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
			model := &callModel{finalModel: finalModel{answer: "被拒绝了"}, calls: []llm.ToolCall{toolCall("c1", "read_file", `{"path":"a.txt"}`)}}
			asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(approvalSet)), approvalHost(t, t.TempDir(), m, model))
			require.NoError(t, err)
			defer func() { require.NoError(t, asm.Close()) }()
			runManager(t, m)
			spawnTool(t, asm, "child")
			record := waitChild(t, m, "child", background.StatusWaiting)
			key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
			require.NoError(t, m.Resume(context.Background(), "child", map[string]string{key: input}))
			require.Equal(t, "被拒绝了", waitChild(t, m, "child", background.StatusCompleted).Result)
			model.mu.Lock()
			defer model.mu.Unlock()
			last, _ := json.Marshal(model.seen[len(model.seen)-1])
			require.Contains(t, string(last), "denied", "未知输入按拒绝处理")
		})
	}
}

// approval: never 优先：不进入等待，直接拒绝。
func TestBackgroundApprovalNeverStillRejects(t *testing.T) {
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	model := &callModel{finalModel: finalModel{answer: "被拒绝了"}, calls: []llm.ToolCall{toolCall("c1", "read_file", `{"path":"a.txt"}`)}}
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {approval: never, rules: [{tool: "*", decision: allow}]}}]}
  - id: worker
    components:
     - `+localWorkspace+`
     - {id: c, use: coding-tools}
     - {id: perms, use: permissions, config: {rules: [{tool: read_file, decision: ask}]}}`)), approvalHost(t, t.TempDir(), m, model))
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	waitChild(t, m, "child", background.StatusCompleted)
}

// 重启后没有父调用的 ctx：预算和会话身份都要恢复，权限判断与开始时相同。
func TestBackgroundChildKeepsBudgetAndSessionAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := durable.NewFileStore(dir)
	require.NoError(t, err)
	controller, err := budget.NewDurable(budget.Config{RootKey: "root", Version: "1", MaxModelRequests: 3}, store)
	require.NoError(t, err)
	entered := make(chan struct{})
	first := fastManager(t, dir, (&noticeLog{}).add)
	lead := mustGetID(t, "lead", trusted(backgroundSet))
	asm, err := Build(context.Background(), lead, Host{ArtifactDir: t.TempDir(), Background: first, RunAgent: func(ctx context.Context, _ *Assembly, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	require.NoError(t, err)
	stop := runManager(t, first)
	ctx := tools.WithSessionID(budget.WithController(context.Background(), controller), "session-1")
	out, err := byToolName(asm)["agent_worker"].Invoke(ctx, &interfaces.ToolInput{ExecutionKey: "child", Args: map[string]any{"task": "x"}})
	require.NoError(t, err)
	require.True(t, out.Success)
	<-entered
	stop()
	require.NoError(t, asm.Close())

	var session atomic.Value
	var hasBudget atomic.Bool
	second := fastManager(t, dir, (&noticeLog{}).add)
	asm, err = Build(context.Background(), lead, Host{ArtifactDir: t.TempDir(), Background: second, RunAgent: func(ctx context.Context, _ *Assembly, _ string) (string, error) {
		session.Store(tools.SessionIDFrom(ctx))
		_, ok := budget.FromContext(ctx)
		hasBudget.Store(ok)
		return "完成", nil
	}})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	waitChild(t, second, "child", background.StatusCompleted)
	require.Equal(t, "session-1", session.Load())
	require.True(t, hasBudget.Load())
}

// 查询工具只看得到当前所有者的子工作。
func TestBackgroundStatusToolIsScopedToOwner(t *testing.T) {
	var log noticeLog
	m := fastManager(t, t.TempDir(), log.add)
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: 你是助手}}]}`)),
		Host{ArtifactDir: t.TempDir(), Background: m, RunAgent: BuilderRunner(finalModel{answer: "子 Agent 完成"}, 3)})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	owner := tools.WithSessionID(context.Background(), "owner")
	tool := byToolName(asm)["agent_worker"]
	status := byToolName(asm)["agent_worker_status"]
	require.NotNil(t, status)
	_, err = tool.Invoke(owner, &interfaces.ToolInput{ExecutionKey: "child", Args: map[string]any{"task": "x"}})
	require.NoError(t, err)
	waitChild(t, m, "child", background.StatusCompleted)

	out, err := status.Invoke(owner, &interfaces.ToolInput{Args: map[string]any{"child_id": "child"}})
	require.NoError(t, err)
	result := out.Result.(map[string]any)
	require.Equal(t, "completed", result["status"])
	require.Equal(t, "子 Agent 完成", result["result"])
	require.NotContains(t, result, "input")

	for name, ctx := range map[string]context.Context{"别的会话": tools.WithSessionID(context.Background(), "other"), "无会话": context.Background()} {
		out, err = status.Invoke(ctx, &interfaces.ToolInput{Args: map[string]any{"child_id": "child"}})
		require.NoError(t, err, name)
		_, rejected := tools.RejectedResult(out)
		require.True(t, rejected, name)
	}
	out, err = status.Invoke(owner, &interfaces.ToolInput{Args: map[string]any{"child_id": "missing"}})
	require.NoError(t, err)
	missing, rejected := tools.RejectedResult(out)
	require.True(t, rejected)
	denied, _ := tools.RejectedResult(func() *interfaces.ToolOutput {
		o, _ := status.Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"child_id": "child"}})
		return o
	}())
	require.Equal(t, missing, denied, "不泄露其他所有者的子工作是否存在")
}

// 前台模式不增加工具，也不改变审批路径。
func TestForegroundSubagentHasNoStatusTool(t *testing.T) {
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: x}}]}`)),
		Host{ArtifactDir: t.TempDir(), RunAgent: BuilderRunner(finalModel{answer: "x"}, 3)})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	require.NotContains(t, byToolName(asm), "agent_worker_status")
}

// 实例已分配但校验失败：记录还没写入，没有人会释放它，必须当场删除。
func TestBackgroundSandboxDestroyedWhenOpenFailsAfterAllocation(t *testing.T) {
	host, provider := sandboxHost(t)
	provider.networkOverride = true
	host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil }
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host.Background = m
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	waitChild(t, m, "child", background.StatusFailed)
	snapshot := provider.destroyedSnapshot()
	require.Len(t, snapshot, 1)
	for id, destroyed := range snapshot {
		require.EqualValues(t, 1, destroyed, id)
	}
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// 持有托管沙箱的 child 可以再启动后台 child：内层在 Build 时登记 Runner，外层结束、进程重启后都能继续。
func TestBackgroundSandboxChildCanNestBackground(t *testing.T) {
	const set = `presets:
 - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, maxDepth: 3, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
 - id: worker
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}
    - {id: d, use: subagent, config: {preset: leaf, maxDepth: 3, mode: background}}
    - {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}
 - id: leaf
   components:
    - {id: w, use: workspace, config: {executor: sandbox, provider: fake, image: runtime}}
    - {id: c, use: coding-tools, config: {tools: [read_file]}}
    - {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}
`
	dir := t.TempDir()
	host, provider := sandboxHost(t)
	inner := make(chan string, 1)
	host.RunAgent = func(ctx context.Context, asm *Assembly, task string) (string, error) {
		if tool, ok := byToolName(asm)["agent_leaf"]; ok {
			out, err := tool.Invoke(ctx, &interfaces.ToolInput{ExecutionKey: "inner", Args: map[string]any{"task": "内层"}})
			if err != nil {
				return "", err
			}
			return out.Result.(map[string]any)["child_id"].(string), nil
		}
		inner <- asm.Manifest.Workspace.SandboxID
		<-ctx.Done()
		return "", ctx.Err()
	}
	first := fastManager(t, dir, (&noticeLog{}).add)
	host.Background = first
	lead := mustGetID(t, "lead", trusted(set))
	asm, err := Build(context.Background(), lead, host)
	require.NoError(t, err)
	require.Empty(t, provider.requests, "Build 不分配实例")
	stop := runManager(t, first)
	spawnTool(t, asm, "outer")
	innerChild := waitChild(t, first, "outer", background.StatusCompleted).Result
	innerID := <-inner
	stop()
	require.NoError(t, asm.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))

	// 重启时外层已结束，没有人会重新装配它；内层靠 Build 时登记的 Runner 恢复，连回自己的实例。
	restart := host
	restart.Sandboxes = &sandbox.Registry{Namespace: "job-1", Providers: map[string]sandbox.Provider{"fake": provider}}
	resumed := make(chan string, 1)
	restart.RunAgent = func(_ context.Context, asm *Assembly, _ string) (string, error) {
		resumed <- asm.Manifest.Workspace.SandboxID
		return "内层完成", nil
	}
	second := fastManager(t, dir, (&noticeLog{}).add)
	restart.Background = second
	asm, err = Build(context.Background(), lead, restart)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	require.Equal(t, innerID, <-resumed)
	waitChild(t, second, innerChild, background.StatusCompleted)
	require.Eventually(t, func() bool { return provider.destroyedSnapshot()[innerID] == 1 }, 30*time.Second, 2*time.Millisecond)
	require.NoError(t, restart.Sandboxes.Close(context.Background()))
}

// 清单不随实例变化：托管沙箱的目标 preset 在 Build 时得到稳定的摘要。
func TestBackgroundSandboxTemplateManifestIsStable(t *testing.T) {
	lead := mustGetID(t, "lead", trusted(managedSet))
	root := t.TempDir()
	digest := func() string {
		host, _ := sandboxHost(t)
		host.Root = root
		host.Background = fastManager(t, t.TempDir(), (&noticeLog{}).add)
		host.RunAgent = func(context.Context, *Assembly, string) (string, error) { return "", nil }
		asm, err := Build(context.Background(), lead, host)
		require.NoError(t, err)
		defer func() { require.NoError(t, asm.Close()) }()
		return asm.Manifest.Digest()
	}
	require.Equal(t, digest(), digest())
}

// 真实 builder 循环：前台孙辈需要审批时，等待穿过中间层到达后台 child，批准后只执行一次并完成。
func TestBackgroundForegroundGrandchildApprovalResumes(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.txt"), []byte("孙辈读到的内容"), 0o600))
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	model := &callModel{finalModel: finalModel{answer: "完成"}, calls: []llm.ToolCall{
		toolCall("c1", "agent_leaf", `{"task":"读文件"}`),
		toolCall("c2", "read_file", `{"path":"a.txt"}`),
	}}
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, maxDepth: 3, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - id: worker
    components:
     - {id: d, use: subagent, config: {preset: leaf, maxDepth: 3}}
     - {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}
  - id: leaf
    components:
     - `+localWorkspace+`
     - {id: c, use: coding-tools}
     - {id: perms, use: permissions, config: {rules: [{tool: read_file, decision: ask}]}}`)), approvalHost(t, root, m, model))
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	record := waitChild(t, m, "child", background.StatusWaiting)
	approval, err := ApprovalOf(record.Waits[0])
	require.NoError(t, err)
	require.Equal(t, "read_file", approval.Tool)
	key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
	require.NoError(t, m.Resume(context.Background(), "child", map[string]string{key: ApprovalAllow}))
	require.Equal(t, "完成", waitChild(t, m, "child", background.StatusCompleted).Result)
	model.mu.Lock()
	defer model.mu.Unlock()
	seen, _ := json.Marshal(model.seen)
	require.Contains(t, string(seen), "孙辈读到的内容", "批准后工具真的执行了")
}

// 普通前台子 Agent 的日志行为不变：整次委派记在父调用上。
func TestForegroundSubagentOutsideBackgroundDropsJournal(t *testing.T) {
	var hadJournal atomic.Bool
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: x}}]}`)),
		Host{ArtifactDir: t.TempDir(), RunAgent: func(ctx context.Context, _ *Assembly, _ string) (string, error) {
			hadJournal.Store(durable.HasJournal(ctx))
			return "x", nil
		}})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	store, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	ctx := durable.WithJournal(durable.WithExecutionKey(context.Background(), "run"), store)
	_, err = byToolName(asm)["agent_worker"].Invoke(ctx, &interfaces.ToolInput{CallID: "c", Args: map[string]any{"task": "x"}})
	require.NoError(t, err)
	require.False(t, hadJournal.Load())
}

// fileProvider 把实例状态放在文件里，真实退出进程后仍能查到。
type fileProvider struct {
	dir string
	// exitIn 非空时在对应时刻退出进程：before 在分配前，after 在分配后、返回前。
	exitIn string
}

const sandboxCrashExit = 93

func (p *fileProvider) Version() string { return "file-v1" }
func (p *fileProvider) idOf(r sandbox.Request) string {
	return "file-" + sha256Hex(r.Scope)[:12]
}
func (p *fileProvider) open(id string, r sandbox.Request) (sandbox.Environment, error) {
	root := filepath.Join(p.dir, id, "root")
	fs, err := env.NewLocalFS(root)
	if err != nil {
		return nil, err
	}
	return &fileEnvironment{sandboxFixtureEnvironment: sandboxFixtureEnvironment{id: id, fs: fs, network: r.Network, isolated: true}, dir: filepath.Join(p.dir, id), log: filepath.Join(p.dir, "destroyed.log")}, nil
}
func (p *fileProvider) Create(_ context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if p.exitIn == "before" {
		os.Exit(sandboxCrashExit)
	}
	id := p.idOf(r)
	if _, err := os.Stat(filepath.Join(p.dir, id)); err == nil {
		return nil, errors.New("同一 Scope 重复分配实例")
	}
	if err := os.MkdirAll(filepath.Join(p.dir, id, "root"), 0o700); err != nil {
		return nil, err
	}
	if p.exitIn == "after" {
		os.Exit(sandboxCrashExit)
	}
	return p.open(id, r)
}
func (p *fileProvider) Connect(_ context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	return p.open(id, r)
}
func (p *fileProvider) instances() []string {
	entries, _ := os.ReadDir(p.dir)
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			ids = append(ids, entry.Name())
		}
	}
	return ids
}

// findingFileProvider 另外支持按 Scope 查询。
type findingFileProvider struct{ *fileProvider }

func (p findingFileProvider) FindByScope(_ context.Context, r sandbox.Request) (string, bool, error) {
	id := p.idOf(r)
	_, err := os.Stat(filepath.Join(p.dir, id))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return id, err == nil, err
}

type fileEnvironment struct {
	sandboxFixtureEnvironment
	dir, log string
}

func (e *fileEnvironment) Destroy(context.Context) error {
	file, err := os.OpenFile(e.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(e.id + "\n"); err != nil {
		return err
	}
	return os.RemoveAll(e.dir)
}

// crashHost 的 Root 须在重启前后相同：它是装配清单的一部分。
func crashHost(t *testing.T, root string, provider sandbox.Provider, m *background.Manager, run func(context.Context, *Assembly, string) (string, error)) Host {
	t.Helper()
	return Host{Root: root, ArtifactDir: t.TempDir(), Background: m, RunAgent: run,
		Sandboxes: &sandbox.Registry{Namespace: "job-1", Providers: map[string]sandbox.Provider{"fake": provider}}}
}

// runCrashChild 在子进程里 Spawn，并在 Provider 分配实例时真实退出。
func runCrashChild(t *testing.T, name, env, dir string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), env+"="+dir)
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited, string(output))
	require.Equal(t, sandboxCrashExit, exited.ExitCode(), string(output))
}

func crashChild(t *testing.T, dir, exitIn string) {
	t.Helper()
	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, &fileProvider{dir: filepath.Join(dir, "instances"), exitIn: exitIn}, m,
		func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil })
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	runManager(t, m)
	spawnTool(t, asm, "child")
	select {}
}

func crashDirs(t *testing.T) (dir, instances string) {
	t.Helper()
	dir = t.TempDir()
	instances = filepath.Join(dir, "instances")
	require.NoError(t, os.MkdirAll(instances, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "store"), 0o700))
	return dir, instances
}

// 真实崩溃：实例已分配、记录还没写入实例 ID。Provider 能按 Scope 查回时连回同一个，结束后销毁，没有泄漏。
func TestCrashAfterAllocationRecoversInstanceByScope(t *testing.T) {
	const env = "GOAGENT_SANDBOX_CRASH_AFTER"
	if dir := os.Getenv(env); dir != "" {
		crashChild(t, dir, "after")
	}
	dir, instances := crashDirs(t)
	runCrashChild(t, "TestCrashAfterAllocationRecoversInstanceByScope", env, dir)
	provider := &fileProvider{dir: instances}
	require.Len(t, provider.instances(), 1, "子进程已分配实例")
	leaked := provider.instances()[0]

	var used atomic.Value
	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, findingFileProvider{provider}, m, func(_ context.Context, asm *Assembly, _ string) (string, error) {
		used.Store(asm.Manifest.Workspace.SandboxID)
		return "完成", nil
	})
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	waitChild(t, m, "child", background.StatusCompleted)
	require.Equal(t, leaked, used.Load(), "连回崩溃前分配的实例")
	require.Eventually(t, func() bool { return len(provider.instances()) == 0 }, 30*time.Second, 2*time.Millisecond, "结束后销毁")
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// 真实崩溃：分配前退出，查询不到实例，重新分配；不把这种情况当作问题上报。
func TestCrashBeforeAllocationCreatesInstance(t *testing.T) {
	const env = "GOAGENT_SANDBOX_CRASH_BEFORE"
	if dir := os.Getenv(env); dir != "" {
		crashChild(t, dir, "before")
	}
	dir, instances := crashDirs(t)
	runCrashChild(t, "TestCrashBeforeAllocationCreatesInstance", env, dir)
	provider := &fileProvider{dir: instances}
	require.Empty(t, provider.instances())

	// 重新分配的实例同样归 child：再次停止进程时保留。
	entered := make(chan struct{})
	first := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, findingFileProvider{provider}, first, func(ctx context.Context, _ *Assembly, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	stop := runManager(t, first)
	<-entered
	stop()
	require.NoError(t, asm.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))
	require.Len(t, provider.instances(), 1, "进程停止不销毁")

	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host = crashHost(t, dir, findingFileProvider{provider}, m, func(context.Context, *Assembly, string) (string, error) { return "完成", nil })
	asm, err = Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	waitChild(t, m, "child", background.StatusCompleted)
	require.Eventually(t, func() bool { return len(provider.instances()) == 0 }, 30*time.Second, 2*time.Millisecond)
	problems, err := m.Problems(context.Background())
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// 崩溃后 child 还没重新进入就被终止：记录里没有实例 ID，释放时按 Scope 查回并销毁。
func TestKilledBeforeReentryDestroysRecoveredInstance(t *testing.T) {
	const env = "GOAGENT_SANDBOX_CRASH_KILLED"
	if dir := os.Getenv(env); dir != "" {
		crashChild(t, dir, "after")
	}
	dir, instances := crashDirs(t)
	runCrashChild(t, "TestKilledBeforeReentryDestroysRecoveredInstance", env, dir)
	provider := &fileProvider{dir: instances}
	require.Len(t, provider.instances(), 1)

	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, findingFileProvider{provider}, m, func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil })
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	// 占满普通槽位，child 恢复后停在等待槽位，不会重新进入。
	for range 4 {
		release, err := m.Slots().Acquire(context.Background(), background.PriorityLaunch)
		require.NoError(t, err)
		defer release()
	}
	runManager(t, m)
	require.NoError(t, m.Kill(context.Background(), "child"))
	require.Eventually(t, func() bool { return len(provider.instances()) == 0 }, 30*time.Second, 2*time.Millisecond, "实例被销毁，没有泄漏")
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// Provider 不能查询时无法判断是否泄漏：子工作失败，问题交给宿主，不新建第二个实例掩盖它。
func TestCrashWithoutFinderIsReportedToHost(t *testing.T) {
	const env = "GOAGENT_SANDBOX_CRASH_NOFIND"
	if dir := os.Getenv(env); dir != "" {
		crashChild(t, dir, "after")
	}
	dir, instances := crashDirs(t)
	runCrashChild(t, "TestCrashWithoutFinderIsReportedToHost", env, dir)
	provider := &fileProvider{dir: instances}

	var runs atomic.Int32
	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, provider, m, func(context.Context, *Assembly, string) (string, error) {
		runs.Add(1)
		return "不应运行", nil
	})
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	require.Equal(t, "sandbox_unknown", waitChild(t, m, "child", background.StatusFailed).FailureKind)
	require.Zero(t, runs.Load())
	require.Len(t, provider.instances(), 1, "没有新建第二个实例")
	var problems []effects.Record
	require.Eventually(t, func() bool {
		problems, err = m.Problems(context.Background())
		return err == nil && len(problems) == 1
	}, 30*time.Second, 2*time.Millisecond)
	require.NoError(t, m.ResolveProblem(context.Background(), problems[0].Seq, true))
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// Provider 明确分配失败时没有实例：撤销意图，不上报问题。
func TestCreateFailureLeavesNoProblem(t *testing.T) {
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host := crashHost(t, t.TempDir(), failingProvider{}, m, func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil })
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	spawnTool(t, asm, "child")
	record := waitChild(t, m, "child", background.StatusFailed)
	require.Empty(t, record.Resources)
	time.Sleep(30 * time.Millisecond)
	problems, err := m.Problems(context.Background())
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

type failingProvider struct{}

func (failingProvider) Version() string { return "failing-v1" }
func (failingProvider) Create(context.Context, sandbox.Request) (sandbox.Environment, error) {
	return nil, fmt.Errorf("配额不足: %w", sandbox.ErrNotCreated)
}
func (failingProvider) Connect(context.Context, string, sandbox.Request) (sandbox.Environment, error) {
	return nil, errors.New("配额不足")
}

// lostResponseProvider 模拟 Create 的响应丢失：实例已创建，调用方只收到错误。
type lostResponseProvider struct{ *fileProvider }

func (p lostResponseProvider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if _, err := p.fileProvider.Create(ctx, r); err != nil {
		return nil, err
	}
	return nil, errors.New("连接断开，响应丢失")
}

// lostResponseFindingProvider 另外支持按 Scope 查询。
type lostResponseFindingProvider struct{ lostResponseProvider }

func (p lostResponseFindingProvider) FindByScope(ctx context.Context, r sandbox.Request) (string, bool, error) {
	return findingFileProvider{p.fileProvider}.FindByScope(ctx, r)
}

func spawnSandboxChild(t *testing.T, provider sandbox.Provider) (*background.Manager, Host) {
	t.Helper()
	m := fastManager(t, t.TempDir(), (&noticeLog{}).add)
	host := crashHost(t, t.TempDir(), provider, m, func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil })
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, asm.Close()) })
	runManager(t, m)
	spawnTool(t, asm, "child")
	return m, host
}

// 响应丢失时实例其实存在：释放时按 Scope 查回并删除，不留孤儿。
func TestCreateResponseLostInstanceIsDestroyed(t *testing.T) {
	files := &fileProvider{dir: t.TempDir()}
	m, host := spawnSandboxChild(t, lostResponseFindingProvider{lostResponseProvider{files}})
	waitChild(t, m, "child", background.StatusFailed)
	require.Eventually(t, func() bool { return len(files.instances()) == 0 }, 30*time.Second, 2*time.Millisecond, "实例被查回并删除")
	problems, err := m.Problems(context.Background())
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// Provider 查不了时无法判断有没有孤儿，交给宿主，不能静默放过。
func TestCreateResponseLostWithoutFinderIsReported(t *testing.T) {
	files := &fileProvider{dir: t.TempDir()}
	m, host := spawnSandboxChild(t, lostResponseProvider{files})
	waitChild(t, m, "child", background.StatusFailed)
	var problems []effects.Record
	require.Eventually(t, func() bool {
		var err error
		problems, err = m.Problems(context.Background())
		return err == nil && len(problems) == 1
	}, 30*time.Second, 2*time.Millisecond)
	require.NoError(t, m.ResolveProblem(context.Background(), problems[0].Seq, true))
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}

// expiringProvider 连接时实例已不存在，如 TTL 到期。
type expiringProvider struct {
	*fileProvider
	expired  atomic.Bool
	connects atomic.Int32
}

func (p *expiringProvider) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	if p.expired.Load() {
		p.connects.Add(1)
		return nil, fmt.Errorf("实例 %s: %w", id, sandbox.ErrInstanceNotFound)
	}
	return p.fileProvider.Connect(ctx, id, r)
}

// 实例在进程停止期间到期：子工作明确记为实例已消失，释放不重试也不上报。
func TestChildReportsInstanceGoneAfterRestart(t *testing.T) {
	dir, instances := crashDirs(t)
	provider := &expiringProvider{fileProvider: &fileProvider{dir: instances}}
	entered := make(chan struct{})
	first := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host := crashHost(t, dir, provider, first, func(ctx context.Context, _ *Assembly, _ string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	})
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	stop := runManager(t, first)
	spawnTool(t, asm, "child")
	<-entered
	stop()
	require.NoError(t, asm.Close())
	require.NoError(t, host.Sandboxes.Close(context.Background()))

	provider.expired.Store(true)
	m := fastManager(t, filepath.Join(dir, "store"), (&noticeLog{}).add)
	host = crashHost(t, dir, provider, m, func(context.Context, *Assembly, string) (string, error) { return "不应运行", nil })
	asm, err = Build(context.Background(), mustGetID(t, "lead", trusted(managedSet)), host)
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	require.Equal(t, "sandbox_gone", waitChild(t, m, "child", background.StatusFailed).FailureKind)
	// 重新进入连接一次，释放连接一次；实例不存在就算删除成功，不反复重试。
	require.Eventually(t, func() bool { return provider.connects.Load() == 2 }, 30*time.Second, 2*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	require.EqualValues(t, 2, provider.connects.Load())
	problems, err := m.Problems(context.Background())
	require.NoError(t, err)
	require.Empty(t, problems)
	require.NoError(t, host.Sandboxes.Close(context.Background()))
}
