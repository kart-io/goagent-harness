package preset

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/interfaces"
	"github.com/stretchr/testify/require"
)

const backgroundSet = `presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: agent_worker, decision: allow}]}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: 你是助手}}]}`

type noticeLog struct {
	mu   sync.Mutex
	list []background.Notice
}

func (n *noticeLog) add(_ context.Context, notice background.Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, notice)
	return nil
}

func (n *noticeLog) all() []background.Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]background.Notice(nil), n.list...)
}

// startManager 创建 Manager，由 Build 登记 Runner 后再调用返回的 run。
func newManager(t *testing.T, dir string, notify func(context.Context, background.Notice) error) *background.Manager {
	t.Helper()
	store, err := durable.NewFileStore(dir)
	require.NoError(t, err)
	m, err := background.New(background.Config{Store: store, Name: "m", Notify: notify, Poll: 5 * time.Millisecond})
	require.NoError(t, err)
	return m
}

func runManager(t *testing.T, m *background.Manager) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	require.NoError(t, m.Ready(context.Background()))
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			require.NoError(t, <-done)
		})
	}
	t.Cleanup(stop)
	return stop
}

func spawnTool(t *testing.T, asm *Assembly, key string) *interfaces.ToolOutput {
	t.Helper()
	out, err := byToolName(asm)["agent_worker"].Invoke(context.Background(), &interfaces.ToolInput{ExecutionKey: key, Args: map[string]any{"task": "整理文档"}})
	require.NoError(t, err)
	require.True(t, out.Success, "%+v", out)
	return out
}

func TestBackgroundSubagentNeedsManagerAndValidMode(t *testing.T) {
	lead := mustGetID(t, "lead", trusted(backgroundSet))
	_, err := Build(context.Background(), lead, Host{ArtifactDir: t.TempDir(), RunAgent: BuilderRunner(finalModel{answer: "x"}, 3)})
	require.ErrorContains(t, err, "Host.Background")
	_, err = Load(trusted(`presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: other}}]}
  - {id: worker, components: [{id: persona, use: persona, config: {prompt: x}}]}`))
	require.ErrorContains(t, err, "mode")
}

// 真实的 builder 循环：工具立即返回 child_id，子 Agent 在持久日志中完成并通知所有者；重放同一调用不启动第二份工作。
func TestBackgroundSubagentCompletesAndNotifies(t *testing.T) {
	var log noticeLog
	m := newManager(t, t.TempDir(), log.add)
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(backgroundSet)),
		Host{AgentID: "lead-agent", ArtifactDir: t.TempDir(), Background: m, RunAgent: BuilderRunner(finalModel{answer: "子 Agent 完成"}, 3)})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	stop := runManager(t, m)
	defer stop()

	out := spawnTool(t, asm, "call-key")
	result := out.Result.(map[string]any)
	require.Equal(t, "call-key", result["child_id"])
	require.Eventually(t, func() bool { return len(log.all()) == 1 }, 5*time.Second, 2*time.Millisecond)
	notice := log.all()[0]
	require.Equal(t, background.StatusCompleted, notice.Status)
	require.Equal(t, "子 Agent 完成", notice.Result)
	require.Equal(t, "lead-agent/sub", notice.Owner)

	again := spawnTool(t, asm, "call-key")
	require.Equal(t, string(background.StatusCompleted), again.Result.(map[string]any)["status"])
	require.Len(t, log.all(), 1)
}

func TestBackgroundSubagentRequiresExecutionKey(t *testing.T) {
	m := newManager(t, t.TempDir(), (&noticeLog{}).add)
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(backgroundSet)),
		Host{ArtifactDir: t.TempDir(), Background: m, RunAgent: BuilderRunner(finalModel{answer: "x"}, 3)})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	_, err = byToolName(asm)["agent_worker"].Invoke(context.Background(), &interfaces.ToolInput{Args: map[string]any{"task": "x"}})
	require.ErrorContains(t, err, "ExecutionKey")
}

// 后台模式不放宽权限：父 Agent 的 deny 同样约束后台子 Agent。
func TestBackgroundSubagentKeepsParentPolicy(t *testing.T) {
	var log noticeLog
	m := newManager(t, t.TempDir(), log.add)
	var runs atomic.Int32
	lead := mustGetID(t, "lead", subagentSet(`{base: coding, rules: [{tool: run_command, command: rm, decision: deny}, {tool: agent_worker, decision: allow}]}`,
		`{preset: worker, mode: background}`, workerComponents))
	asm, err := Build(context.Background(), lead, Host{Root: t.TempDir(), ArtifactDir: t.TempDir(), Background: m, RunAgent: taskRunner(&runs)})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, m)
	out, err := byToolName(asm)["agent_worker"].Invoke(context.Background(), &interfaces.ToolInput{ExecutionKey: "deny-key",
		Args: map[string]any{"task": `run_command {"command":"rm -rf x"}`}})
	require.NoError(t, err)
	require.True(t, out.Success)
	require.Eventually(t, func() bool { return len(log.all()) == 1 }, 5*time.Second, 2*time.Millisecond)
	require.Equal(t, "rejected: run_command is denied by policy", log.all()[0].Result)
}

// 重启：新的 Manager 和重新 Build 的 Assembly 找回未完成的子 Agent，用同一执行键继续。
func TestBackgroundSubagentResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	entered := make(chan struct{})
	first := newManager(t, dir, (&noticeLog{}).add)
	asm, err := Build(context.Background(), mustGetID(t, "lead", trusted(backgroundSet)),
		Host{ArtifactDir: t.TempDir(), Background: first, RunAgent: func(ctx context.Context, _ *Assembly, _ string) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", ctx.Err()
		}})
	require.NoError(t, err)
	stop := runManager(t, first)
	spawnTool(t, asm, "call-key")
	<-entered
	stop()
	require.NoError(t, asm.Close())

	var log noticeLog
	var key string
	second := newManager(t, dir, log.add)
	asm, err = Build(context.Background(), mustGetID(t, "lead", trusted(backgroundSet)),
		Host{ArtifactDir: t.TempDir(), Background: second, RunAgent: func(ctx context.Context, _ *Assembly, task string) (string, error) {
			key = durable.ExecutionKey(ctx)
			return "恢复后完成:" + task, nil
		}})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()
	runManager(t, second)
	require.Eventually(t, func() bool { return len(log.all()) == 1 }, 5*time.Second, 2*time.Millisecond)
	require.Equal(t, "恢复后完成:整理文档", log.all()[0].Result)
	require.Equal(t, background.KeyOf("m", "call-key"), key)
}
