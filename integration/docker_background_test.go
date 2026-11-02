package integration_test

import (
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/sandboxtest"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/interfaces"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/constants"
	"github.com/stretchr/testify/require"
)

const dockerSet = `presets:
  - {id: lead, components: [{id: sub, use: subagent, config: {preset: worker, mode: background}}, {id: perms, use: permissions, config: {rules: [{tool: "*", decision: allow}]}}]}
  - id: worker
    components:
     - {id: w, use: workspace, config: {executor: sandbox, provider: docker, image: "` + dockerTestImage + `", sandbox: workspace-write}}
     - {id: c, use: coding-tools, config: {tools: [read_file, write_file, run_command]}}
     - {id: perms, use: permissions, config: {rules: [{tool: run_command, decision: RULE}, {tool: "*", decision: allow}]}}`

func dockerRunIDOnly() string { return "t" + strings.ToLower(crand.Text()[:12]) }

func dockerRunID(t *testing.T) string {
	t.Helper()
	run := dockerRunIDOnly()
	dockerCleanup(t, run)
	return run
}

func dockerCleanup(t *testing.T, run string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		ids := dockerContainers(t, ctx, run)
		if len(ids) > 0 {
			_, _ = dockerRun(ctx, append([]string{"rm", "-f"}, ids...)...)
		}
	})
}

func dockerContainers(t *testing.T, ctx context.Context, run string) []string {
	t.Helper()
	out, err := dockerRun(ctx, "ps", "-aq", "--no-trunc", "--filter", "label="+dockerLabelRun+"="+run)
	require.NoError(t, err)
	return strings.Fields(out)
}

// dockerExec 只用于只读核对和可重复的命令。Docker 29.9.2 并发 exec 偶尔会卡住（纯 shell 也能复现），所以限时并重试。
func dockerExec(t *testing.T, id string, argv ...string) string {
	t.Helper()
	var lastErr error
	lastHung := false
	for range 6 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err := dockerRun(ctx, append([]string{"exec", id}, argv...)...)
		hung := ctx.Err() != nil
		cancel()
		if err == nil {
			return out
		}
		// 上一次卡住时命令可能已经生效，子工作结束后容器被回收。后续断言会确认结果。
		if lastErr != nil && lastHung && dockerMissing(err) {
			return ""
		}
		lastErr, lastHung = err, hung
	}
	require.NoError(t, lastErr)
	return ""
}

// scriptedModel 按脚本回复，blockAt 处停住直到请求被取消，模拟进程在运行中停止。
type scriptedModel struct {
	mu      sync.Mutex
	steps   []llm.ToolCall
	answer  string
	blockAt int
	blocked chan struct{}
	// exitAt 非零时在第 exitAt 次请求时真实退出进程。
	exitAt   int
	requests [][]llm.Message
}

func dockerCall(id, name string, args map[string]any) llm.ToolCall {
	raw, _ := json.Marshal(args)
	call := llm.ToolCall{ID: id, Type: "function"}
	call.Function.Name, call.Function.Arguments = name, string(raw)
	return call
}

func (m *scriptedModel) CompleteToolsStream(ctx context.Context, req *llm.CompletionRequest, _ []interfaces.Tool) (<-chan *llm.StreamChunk, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req.Messages)
	index := len(m.requests)
	block := m.blocked != nil && index == m.blockAt
	m.mu.Unlock()
	if m.exitAt != 0 && index == m.exitAt {
		os.Exit(dockerCrashExit)
	}
	if block {
		close(m.blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan *llm.StreamChunk, 2)
	if len(m.steps) > 0 {
		ch <- &llm.StreamChunk{Done: true, FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{m.steps[0]}}
		m.steps = m.steps[1:]
	} else {
		ch <- &llm.StreamChunk{Delta: m.answer}
		ch <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
	}
	close(ch)
	return ch, nil
}

func (m *scriptedModel) seen() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, _ := json.Marshal(m.requests)
	return string(raw)
}

func (m *scriptedModel) Complete(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{Content: m.answer}, nil
}
func (m *scriptedModel) Chat(context.Context, []llm.Message) (*llm.CompletionResponse, error) {
	return &llm.CompletionResponse{Content: m.answer}, nil
}
func (*scriptedModel) Provider() constants.Provider { return constants.ProviderCustom }
func (*scriptedModel) IsAvailable() bool            { return true }
func (m *scriptedModel) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return closedStream(m.answer), nil
}
func (m *scriptedModel) ChatStream(context.Context, []llm.Message) (<-chan *llm.StreamChunk, error) {
	return closedStream(m.answer), nil
}

func closedStream(answer string) <-chan *llm.StreamChunk {
	ch := make(chan *llm.StreamChunk, 2)
	ch <- &llm.StreamChunk{Delta: answer}
	ch <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
	close(ch)
	return ch
}

// dockerRig 是一次“进程”：Registry、Manager 和 Assembly。stop 模拟进程停止。
type dockerRig struct {
	t        *testing.T
	manager  *background.Manager
	assembly *preset.Assembly
	registry *sandbox.Registry
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
}

type rigConfig struct {
	dir, namespace string
	provider       sandbox.Provider
	rule           string
	run            func(context.Context, *preset.Assembly, string) (string, error)
	resolve        func(sandbox.Request) (sandbox.Binding, error)
}

func startRig(t *testing.T, cfg rigConfig) *dockerRig {
	t.Helper()
	if cfg.rule == "" {
		cfg.rule = "allow"
	}
	store, err := durable.NewFileStore(cfg.dir + "/store")
	require.NoError(t, err)
	manager, err := background.New(background.Config{Store: store, Name: "m", Poll: 10 * time.Millisecond,
		Notify:  func(context.Context, background.Notice) error { return nil },
		Effects: effects.Config{Poll: 10 * time.Millisecond, MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, LaterDelay: 10 * time.Millisecond}})
	require.NoError(t, err)
	registry := &sandbox.Registry{Namespace: cfg.namespace, Providers: map[string]sandbox.Provider{"docker": cfg.provider}, Resolve: cfg.resolve}
	set, err := preset.Load(preset.Source{Layer: preset.LayerHost, Trusted: true, Data: []byte(strings.ReplaceAll(dockerSet, "RULE", cfg.rule))})
	require.NoError(t, err)
	lead, ok := set.Get("lead")
	require.True(t, ok)
	asm, err := preset.Build(context.Background(), lead, preset.Host{Root: cfg.dir, ArtifactDir: t.TempDir(), Sandboxes: registry, Background: manager, RunAgent: cfg.run})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	rig := &dockerRig{t: t, manager: manager, assembly: asm, registry: registry, cancel: cancel, done: make(chan error, 1)}
	go func() { rig.done <- manager.Run(ctx) }()
	require.NoError(t, manager.Ready(context.Background()))
	t.Cleanup(rig.stop)
	return rig
}

func (r *dockerRig) stop() {
	r.once.Do(func() {
		r.cancel()
		require.NoError(r.t, <-r.done)
		require.NoError(r.t, r.assembly.Close())
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		require.NoError(r.t, r.registry.Close(cleanup))
	})
}

func (r *dockerRig) spawn(id, task string) {
	r.t.Helper()
	for _, tool := range r.assembly.Tools {
		if tool.Name() == "agent_worker" {
			out, err := tool.Invoke(context.Background(), &interfaces.ToolInput{ExecutionKey: id, Args: map[string]any{"task": task}})
			require.NoError(r.t, err)
			require.True(r.t, out.Success, "%+v", out)
			return
		}
	}
	r.t.Fatal("没有 agent_worker")
}

func (r *dockerRig) wait(id string, status background.Status) background.Record {
	r.t.Helper()
	var last background.Record
	var lastErr error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		last, lastErr = r.manager.Get(context.Background(), id)
		if lastErr == nil && last.Status == status {
			return last
		}
	}
	require.Failf(r.t, "等待超时", "%s 没有进入 %s，最后为 %+v，错误 %v", id, status, last, lastErr)
	return last
}

func waitContainers(t *testing.T, run string, count int) []string {
	t.Helper()
	var ids []string
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		ids = dockerContainers(t, context.Background(), run)
		if len(ids) == count {
			return ids
		}
	}
	require.Failf(t, "容器数量不符", "期望 %d 个，实际 %v", count, ids)
	return ids
}

func workspaceID(asm *preset.Assembly) string { return asm.Manifest.Workspace.SandboxID }

// runCommand 经 Dispatcher 和权限管线调用 run_command。
func runCommand(ctx context.Context, asm *preset.Assembly, callID, command string) (string, error) {
	for _, tool := range asm.Tools {
		if tool.Name() == "run_command" {
			out, err := tool.Invoke(ctx, &interfaces.ToolInput{CallID: callID, Args: map[string]any{"command": command}})
			if err != nil {
				return "", err
			}
			raw, err := json.Marshal(out.Result)
			return string(raw), err
		}
	}
	return "", errors.New("没有 run_command")
}

// 完整链路：真实 builder 循环在容器里写文件，停在审批等待这个已提交的边界上；进程停止后容器还在；
// 重启后连回同一容器读到文件，批准后继续，结束后容器被删除。
func TestDockerChildSurvivesRestartThenContainerRemoved(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	first := &scriptedModel{steps: []llm.ToolCall{
		dockerCall("c1", "write_file", map[string]any{"path": "notes.txt", "content": "容器里的内容"}),
		dockerCall("c2", "run_command", map[string]any{"command": "cat notes.txt; hostname"}),
	}}
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, rule: "ask", run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "整理")
	rig.wait("child", background.StatusWaiting)
	ids := waitContainers(t, run, 1)
	require.Equal(t, "容器里的内容", dockerExec(t, ids[0], "cat", "/workspace/notes.txt"), "文件真实写进了容器")
	rig.stop()
	require.Equal(t, ids, waitContainers(t, run, 1), "进程停止不删除容器")

	second := &scriptedModel{steps: []llm.ToolCall{dockerCall("c3", "read_file", map[string]any{"path": "notes.txt"})}, answer: "完成"}
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, rule: "ask", run: preset.BuilderRunner(second, 8)})
	record := rig.wait("child", background.StatusWaiting)
	key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
	require.NoError(t, rig.manager.Resume(context.Background(), "child", map[string]string{key: preset.ApprovalAllow}))
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	seen := second.seen()
	require.Contains(t, seen, "容器里的内容", "重启后读到停止前写的文件")
	require.Contains(t, seen, ids[0][:12], "命令在同一个容器里执行")
	waitContainers(t, run, 0)
}

// 停在模型请求中途：重启后重发这次请求并继续，连回同一个容器，停止前写的文件还在。
func TestDockerStopDuringModelRequestContinues(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	blocked := make(chan struct{})
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "write_file", map[string]any{"path": "notes.txt", "content": "停止前写的"})}, blockAt: 2, blocked: blocked}
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "整理")
	<-blocked
	rig.stop()
	waitContainers(t, run, 1)

	second := &scriptedModel{steps: []llm.ToolCall{dockerCall("c2", "read_file", map[string]any{"path": "notes.txt"})}, answer: "完成"}
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "停止前写的", "重启后读到同一个容器里的文件")
	waitContainers(t, run, 0)
}

// 真实崩溃：停在模型请求中途，日志里的记录来不及收尾。重启后同样重发并继续。
func TestDockerCrashDuringModelRequestContinues(t *testing.T) {
	const envName = "GOAGENT_DOCKER_CRASH_MODEL"
	if dir := os.Getenv(envName); dir != "" {
		run := "c" + scopeHash(dir)[:12]
		model := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "write_file", map[string]any{"path": "notes.txt", "content": "崩溃前写的"})}, exitAt: 2}
		rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, run: preset.BuilderRunner(model, 8)})
		rig.spawn("child", "整理")
		time.Sleep(time.Minute)
		t.Fatal("期望在模型请求时退出进程")
	}
	requireDocker(t)
	dir, run := runCrashProcess(t, "TestDockerCrashDuringModelRequestContinues", envName)
	waitContainers(t, run, 1)

	model := &scriptedModel{steps: []llm.ToolCall{dockerCall("c2", "read_file", map[string]any{"path": "notes.txt"})}, answer: "完成"}
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, run: preset.BuilderRunner(model, 8)})
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, model.seen(), "崩溃前写的")
	waitContainers(t, run, 0)
}

// slowCommand 写一行副作用后等到 release 文件出现，再输出结果。
func slowCommand() string {
	return "echo once >> /tmp/effect; while [ ! -e /tmp/release ]; do sleep 0.05; done; echo 命令结果"
}

func waitEffect(t *testing.T, id string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(dockerExec(t, id, "sh", "-c", "cat /tmp/effect 2>/dev/null || true"), "once") {
			return
		}
	}
	t.Fatal("命令没有启动")
}

// commandRig 让子工作停在 slowCommand 上。
func commandRig(t *testing.T, provider *dockerProvider, dir, rule string) (*dockerRig, string) {
	t.Helper()
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "run_command", map[string]any{"command": slowCommand()})}}
	rig := startRig(t, rigConfig{dir: dir, namespace: provider.run, provider: provider, rule: rule, run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "执行")
	if rule == "ask" {
		record := rig.wait("child", background.StatusWaiting)
		key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
		require.NoError(t, rig.manager.Resume(context.Background(), "child", map[string]string{key: preset.ApprovalAllow}))
	}
	ids := waitContainers(t, provider.run, 1)
	waitEffect(t, ids[0])
	return rig, ids[0]
}

func secondModel() *scriptedModel { return &scriptedModel{answer: "完成"} }

// 进程停止时命令还在跑：重启后附着等结果，不重跑，模型拿到输出。
func TestDockerStopDuringCommandAttaches(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "")
	rig.stop()

	second := secondModel()
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: provider, run: preset.BuilderRunner(second, 8)})
	time.Sleep(500 * time.Millisecond)
	dockerExec(t, id, "touch", "/tmp/release")
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "命令结果")
	waitContainers(t, run, 0)
}

// 停机期间命令已经跑完：重启后直接取回结果，不重跑。
func TestDockerCommandFinishedWhileStoppedIsNotRerun(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "")
	rig.stop()
	dockerExec(t, id, "touch", "/tmp/release")
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if dockerExec(t, id, "sh", "-c", "ls /tmp/.goagent-commands/*/exit 2>/dev/null || true") != "" {
			break
		}
	}

	second := secondModel()
	gate := make(chan struct{})
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{provider, gate}, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "命令结果")
	require.Equal(t, "once", dockerExec(t, id, "cat", "/tmp/effect"), "命令没有被重跑")
	close(gate)
	waitContainers(t, run, 0)
}

// 真实崩溃：命令不受影响，重启后附着。
func TestDockerCrashDuringCommandAttaches(t *testing.T) {
	const envName = "GOAGENT_DOCKER_CRASH_COMMAND"
	if dir := os.Getenv(envName); dir != "" {
		run := "c" + scopeHash(dir)[:12]
		provider := &dockerProvider{run: run}
		rig, _ := commandRig(t, provider, dir, "")
		_ = rig
		os.Exit(dockerCrashExit)
	}
	requireDocker(t)
	dir, run := runCrashProcess(t, "TestDockerCrashDuringCommandAttaches", envName)
	ids := waitContainers(t, run, 1)

	second := secondModel()
	gate := make(chan struct{})
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{&dockerProvider{run: run}, gate}, run: preset.BuilderRunner(second, 8)})
	dockerExec(t, ids[0], "touch", "/tmp/release")
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "命令结果")
	require.Equal(t, "once", dockerExec(t, ids[0], "cat", "/tmp/effect"))
	close(gate)
	waitContainers(t, run, 0)
}

// 容器重启：进程没了，结果也没有。不重跑，告诉模型状态未知，由它决定下一步。
func TestDockerRestartedContainerCommandIsLost(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "")
	rig.stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := dockerRun(ctx, "restart", "-t", "0", id)
	require.NoError(t, err)

	second := secondModel()
	gate := make(chan struct{})
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{provider, gate}, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "没有重新执行")
	require.Equal(t, "once", dockerExec(t, id, "cat", "/tmp/effect"))
	close(gate)
	waitContainers(t, run, 0)
}

// 状态目录被删：看起来像没启动过，但这是第二次尝试，不能重跑。
func TestDockerDeletedCommandStateIsNotRerun(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "")
	rig.stop()
	dockerExec(t, id, "rm", "-rf", "/tmp/.goagent-commands")

	second := secondModel()
	gate := make(chan struct{})
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{provider, gate}, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "没有重新执行")
	require.Equal(t, "once", dockerExec(t, id, "cat", "/tmp/effect"), "副作用只有一次")
	close(gate)
	waitContainers(t, run, 0)
}

// 容器被删：子工作明确失败，不冒充原环境，也不在新容器里重跑。
func TestDockerRemovedContainerFailsChild(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "")
	rig.stop()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := dockerRun(ctx, "rm", "-f", id)
	require.NoError(t, err)

	second := secondModel()
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: provider, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "sandbox_gone", rig.wait("child", background.StatusFailed).FailureKind)
	second.mu.Lock()
	requests := len(second.requests)
	second.mu.Unlock()
	require.Zero(t, requests)
	waitContainers(t, run, 0)
}

// 超时由沙箱内部执行：宿主停着的时候命令也会被杀掉。
func TestDockerTimeoutEnforcedWhileHostStopped(t *testing.T) {
	requireDocker(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := dockerRunID(t)
	registry := &sandbox.Registry{Namespace: run, Providers: map[string]sandbox.Provider{"docker": &dockerProvider{run: run}}}
	defer func() { require.NoError(t, registry.Close(context.Background())) }()
	session, err := registry.Open(ctx, sandbox.Request{Provider: "docker", Scope: "timeout", Image: dockerTestImage})
	require.NoError(t, err)
	defer func() { require.NoError(t, session.Close(context.Background())) }()
	id := session.Info().ID
	policy := sandbox.Policy{WorkspaceRoot: "/", Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny}
	command := sandbox.KeyedCommand{Key: "k", Timeout: time.Second, Command: env.Command{Argv: []string{"sh", "-c", "sleep 271 & wait"}}}

	detached, disconnect := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := session.RunKeyed(detached, command, policy)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	disconnect()
	require.ErrorIs(t, <-done, context.Canceled)

	time.Sleep(2 * time.Second)
	require.NotContains(t, dockerExec(t, id, "ps"), "sleep 271", "没有宿主也被杀掉，包括子进程")
	command.AttachOnly = true
	res, err := session.RunKeyed(ctx, command, policy)
	require.NoError(t, err)
	require.True(t, res.TimedOut)
}

// 恢复时重新审批，这次被拒绝：命令被终止，模型知道它已经启动过。
func TestDockerDenyOnRestartStopsCommand(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	rig, id := commandRig(t, provider, dir, "ask")
	rig.stop()

	second := secondModel()
	gate := make(chan struct{})
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{provider, gate}, rule: "ask", run: preset.BuilderRunner(second, 8)})
	record := rig.wait("child", background.StatusWaiting)
	require.Contains(t, dockerExec(t, id, "ps"), "/tmp/release", "等待审批时命令还在跑")
	key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
	require.NoError(t, rig.manager.Resume(context.Background(), "child", map[string]string{key: "deny"}))
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "已经启动", "模型知道命令启动过")
	require.Eventually(t, func() bool {
		return dockerExec(t, id, "sh", "-c", "ls /tmp/.goagent-commands/*/exit 2>/dev/null || true") != ""
	}, 10*time.Second, 50*time.Millisecond, "命令被终止")
	require.Equal(t, "once", dockerExec(t, id, "cat", "/tmp/effect"))
	close(gate)
	waitContainers(t, run, 0)
}

// 不支持按键执行的后端保持原样：命令不重跑，子工作交给宿主。
func TestDockerPlainBackendNeedsAttention(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run, plainExec: true}
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "run_command", map[string]any{"command": "echo once >> /tmp/effect; sleep 300"})}}
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: provider, run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "执行")
	ids := waitContainers(t, run, 1)
	waitEffect(t, ids[0])
	rig.stop()
	require.Equal(t, "once", dockerExec(t, ids[0], "cat", "/tmp/effect"))

	second := &scriptedModel{answer: "不应运行"}
	gate := make(chan struct{})
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: gatedDestroy{provider, gate}, run: preset.BuilderRunner(second, 8)})
	require.Equal(t, "attention", rig.wait("child", background.StatusFailed).FailureKind)
	require.Equal(t, "once", dockerExec(t, ids[0], "cat", "/tmp/effect"), "命令没有被重跑")
	close(gate)
	waitContainers(t, run, 0)
}

// 容器已停止或已删除时，释放同样要能结束，不留下容器，也不留下问题。
func TestDockerDestroyHandlesStoppedAndMissingContainers(t *testing.T) {
	requireDocker(t)
	run := dockerRunID(t)
	provider := &dockerProvider{run: run}
	registry := &sandbox.Registry{Namespace: run, Providers: map[string]sandbox.Provider{"docker": provider}}
	defer func() { require.NoError(t, registry.Close(context.Background())) }()
	req := sandbox.Request{Provider: "docker", Scope: "destroy", Image: dockerTestImage}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id := func() string {
		session, err := registry.OpenWith(ctx, req, func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{Retain: true}, nil })
		require.NoError(t, err)
		id := session.Info().ID
		require.NoError(t, session.Close(ctx))
		return id
	}()
	_, err := dockerRun(ctx, "stop", "-t", "0", id)
	require.NoError(t, err)
	require.NoError(t, registry.Destroy(ctx, req, id), "已停止的容器")
	waitContainers(t, run, 0)
	require.NoError(t, registry.Destroy(ctx, req, id), "已删除的容器，重试不报错")
}

// 真实容器上的远端故障：创建响应丢失时容器不泄漏，子工作明确失败。
func TestDockerLostCreateResponseDoesNotLeak(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	faults := &sandboxtest.Faults{Provider: findingDockerProvider{&dockerProvider{run: run}}}
	faults.LoseCreateResponse.Store(1)
	faults.FailFind.Store(2)
	faults.FailDestroy.Store(2)
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: faults,
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "不应运行", nil }})
	rig.spawn("child", "任务")
	rig.wait("child", background.StatusFailed)
	waitContainers(t, run, 0)
	waitNoProblems(t, rig.manager)
	require.Zero(t, faults.FailFind.Load(), "查询和删除的故障都经历过")
	require.Zero(t, faults.FailDestroy.Load())
}

// 进程重启后第一次连接失败：结果未知，不能当作实例消失。子工作失败，容器被回收。
func TestDockerConnectFailureAfterRestartDoesNotLeak(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	provider := &dockerProvider{run: run}
	entered := make(chan struct{})
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: provider,
		run: func(ctx context.Context, _ *preset.Assembly, _ string) (string, error) {
			close(entered)
			<-ctx.Done()
			return "", ctx.Err()
		}})
	rig.spawn("child", "任务")
	<-entered
	rig.stop()
	waitContainers(t, run, 1)

	faults := &sandboxtest.Faults{Provider: provider}
	faults.FailConnect.Store(1)
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: faults,
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "不应运行", nil }})
	require.Equal(t, "execution", rig.wait("child", background.StatusFailed).FailureKind)
	waitContainers(t, run, 0)
}

// waitNoProblems 等释放收敛：实例删除之后，队列要过一会儿才把这条待办记为完成。
func waitNoProblems(t *testing.T, m *background.Manager) {
	t.Helper()
	require.Eventually(t, func() bool {
		problems, err := m.Problems(context.Background())
		return err == nil && len(problems) == 0
	}, 30*time.Second, 20*time.Millisecond, "问题没有收敛")
}

// gatedDestroy 让销毁等到测试核对完容器之后。
type gatedDestroy struct {
	*dockerProvider
	gate chan struct{}
}

func (p gatedDestroy) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	e, err := p.dockerProvider.Connect(ctx, id, r)
	if err != nil {
		return nil, err
	}
	return gatedEnvironment{e.(*dockerEnvironment), p.gate}, nil
}

func (p gatedDestroy) DestroyInstance(ctx context.Context, id string, r sandbox.Request) error {
	select {
	case <-p.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return p.dockerProvider.DestroyInstance(ctx, id, r)
}

type gatedEnvironment struct {
	*dockerEnvironment
	gate chan struct{}
}

func (e gatedEnvironment) Destroy(ctx context.Context) error {
	select {
	case <-e.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	return e.dockerEnvironment.Destroy(ctx)
}

// 每个 child 有自己的容器，文件互相看不到。
func TestDockerChildrenAreIsolated(t *testing.T) {
	requireDocker(t)
	run := dockerRunID(t)
	results := sync.Map{}
	release := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	runner := func(ctx context.Context, asm *preset.Assembly, task string) (string, error) {
		if _, err := runCommand(ctx, asm, "c1", "echo "+task+" > "+task+".txt"); err != nil {
			return "", err
		}
		entered.Done()
		<-release
		listed, err := runCommand(ctx, asm, "c2", "ls")
		if err != nil {
			return "", err
		}
		results.Store(task, listed)
		return workspaceID(asm), nil
	}
	rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: run, provider: &dockerProvider{run: run}, run: runner})
	rig.spawn("child-a", "a")
	rig.spawn("child-b", "b")
	entered.Wait()
	require.Len(t, waitContainers(t, run, 2), 2)
	close(release)
	a := rig.wait("child-a", background.StatusCompleted)
	b := rig.wait("child-b", background.StatusCompleted)
	require.NotEqual(t, a.Result, b.Result)
	seenA, _ := results.Load("a")
	seenB, _ := results.Load("b")
	require.Contains(t, seenA, "a.txt")
	require.NotContains(t, seenA, "b.txt")
	require.Contains(t, seenB, "b.txt")
	require.NotContains(t, seenB, "a.txt")
	waitContainers(t, run, 0)
}

func crashChildMain(t *testing.T, dir, mode string) {
	t.Helper()
	run := "c" + scopeHash(dir)[:12]
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run, crash: mode},
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "不应运行", nil }})
	rig.spawn("child", "任务")
	time.Sleep(time.Minute)
	t.Fatal("期望在分配实例时退出进程")
}

// runCrashProcess 在子进程里启动子工作，让它在分配容器时真实退出。
func runCrashProcess(t *testing.T, name, envName string) (dir, run string) {
	t.Helper()
	dir = t.TempDir()
	run = "c" + scopeHash(dir)[:12]
	dockerCleanup(t, run)
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), envName+"="+dir)
	output, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	require.ErrorAs(t, err, &exited, string(output))
	require.Equal(t, dockerCrashExit, exited.ExitCode(), string(output))
	return dir, run
}

// 真实崩溃：容器已创建，进程没来得及记录它。重启后按 Scope 查回同一个容器，结束后删除，没有泄漏。
func TestDockerCrashAfterAllocationRecoversContainer(t *testing.T) {
	const envName = "GOAGENT_DOCKER_CRASH_AFTER"
	if dir := os.Getenv(envName); dir != "" {
		crashChildMain(t, dir, "after")
	}
	requireDocker(t)
	dir, run := runCrashProcess(t, "TestDockerCrashAfterAllocationRecoversContainer", envName)
	ids := waitContainers(t, run, 1)
	used := make(chan string, 1)
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: findingDockerProvider{&dockerProvider{run: run}},
		run: func(_ context.Context, asm *preset.Assembly, _ string) (string, error) {
			used <- workspaceID(asm)
			return "完成", nil
		}})
	rig.wait("child", background.StatusCompleted)
	require.Equal(t, ids[0], <-used, "连回崩溃前创建的容器")
	waitContainers(t, run, 0)
}

// 真实崩溃：还没创建容器。重启后查不到，正常创建，结束后删除。
func TestDockerCrashBeforeAllocationCreatesContainer(t *testing.T) {
	const envName = "GOAGENT_DOCKER_CRASH_BEFORE"
	if dir := os.Getenv(envName); dir != "" {
		crashChildMain(t, dir, "before")
	}
	requireDocker(t)
	dir, run := runCrashProcess(t, "TestDockerCrashBeforeAllocationCreatesContainer", envName)
	waitContainers(t, run, 0)
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: findingDockerProvider{&dockerProvider{run: run}},
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "完成", nil }})
	rig.wait("child", background.StatusCompleted)
	waitContainers(t, run, 0)
	problems, err := rig.manager.Problems(context.Background())
	require.NoError(t, err)
	require.Empty(t, problems)
}

// Provider 不能查询：不新建第二个容器，问题交给宿主，崩溃前的容器原样保留。
func TestDockerCrashWithoutFinderIsReportedToHost(t *testing.T) {
	const envName = "GOAGENT_DOCKER_CRASH_NOFIND"
	if dir := os.Getenv(envName); dir != "" {
		crashChildMain(t, dir, "after")
	}
	requireDocker(t)
	dir, run := runCrashProcess(t, "TestDockerCrashWithoutFinderIsReportedToHost", envName)
	ids := waitContainers(t, run, 1)
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run},
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "不应运行", nil }})
	require.Equal(t, "sandbox_unknown", rig.wait("child", background.StatusFailed).FailureKind)
	var problems []effects.Record
	require.Eventually(t, func() bool {
		var err error
		problems, err = rig.manager.Problems(context.Background())
		return err == nil && len(problems) == 1
	}, 30*time.Second, 20*time.Millisecond)
	require.Equal(t, ids, waitContainers(t, run, 1))
	require.NoError(t, rig.manager.ResolveProblem(context.Background(), problems[0].Seq, true))
}

// 宿主借出或保留的容器由宿主负责，child 结束后还在。
func TestDockerHostManagedContainersSurvive(t *testing.T) {
	requireDocker(t)
	t.Run("borrowed", func(t *testing.T) {
		run := dockerRunID(t)
		provider := &dockerProvider{run: run}
		borrowed, err := provider.Create(context.Background(), sandbox.Request{Scope: "host-owned", Image: dockerTestImage})
		require.NoError(t, err)
		used := make(chan string, 1)
		rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: run, provider: provider,
			resolve: func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{ID: borrowed.ID()}, nil },
			run: func(_ context.Context, asm *preset.Assembly, _ string) (string, error) {
				used <- workspaceID(asm)
				return "完成", nil
			}})
		rig.spawn("child", "任务")
		rig.wait("child", background.StatusCompleted)
		require.Equal(t, borrowed.ID(), <-used)
		rig.stop()
		require.Equal(t, []string{borrowed.ID()}, waitContainers(t, run, 1))
	})
	t.Run("retained", func(t *testing.T) {
		run := dockerRunID(t)
		rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: run, provider: &dockerProvider{run: run},
			resolve: func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{Retain: true}, nil },
			run:     func(context.Context, *preset.Assembly, string) (string, error) { return "完成", nil }})
		rig.spawn("child", "任务")
		rig.wait("child", background.StatusCompleted)
		rig.stop()
		waitContainers(t, run, 1)
	})
}

// 真实的长命令运行中被终止：子工作结束，容器被删除，容器里的进程随之消失。
func TestDockerKillDuringCommandRemovesContainer(t *testing.T) {
	requireDocker(t)
	run := dockerRunID(t)
	started := make(chan struct{})
	rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: run, provider: &dockerProvider{run: run},
		run: func(ctx context.Context, asm *preset.Assembly, _ string) (string, error) {
			close(started)
			_, err := runCommand(ctx, asm, "c1", "sleep 300")
			return "", err
		}})
	rig.spawn("child", "长任务")
	<-started
	ids := waitContainers(t, run, 1)
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(dockerExec(t, ids[0], "ps"), "sleep 300") {
			break
		}
	}
	begin := time.Now()
	require.NoError(t, rig.manager.Kill(context.Background(), "child"))
	rig.wait("child", background.StatusKilled)
	waitContainers(t, run, 0)
	require.Less(t, time.Since(begin), 60*time.Second)
}

// 真实 builder 循环：需要审批的命令在批准前没有执行；重启后仍在等待；批准后在同一容器执行且只执行一次。
func TestDockerApprovalBlocksCommandUntilResumed(t *testing.T) {
	requireDocker(t)
	run, dir := dockerRunID(t), t.TempDir()
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "run_command", map[string]any{"command": "echo done >> marker.txt; wc -l marker.txt"})}, answer: "完成"}
	rig := startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, rule: "ask", run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "执行")
	record := rig.wait("child", background.StatusWaiting)
	approval, err := preset.ApprovalOf(record.Waits[0])
	require.NoError(t, err)
	require.Equal(t, "run_command", approval.Tool)
	require.Contains(t, approval.Args, "marker.txt")
	ids := waitContainers(t, run, 1)
	require.Equal(t, "absent", dockerExec(t, ids[0], "sh", "-c", "test -e /workspace/marker.txt && echo present || echo absent"), "批准前没有执行")
	rig.stop()

	second := &scriptedModel{answer: "完成"}
	rig = startRig(t, rigConfig{dir: dir, namespace: run, provider: &dockerProvider{run: run}, rule: "ask", run: preset.BuilderRunner(second, 8)})
	record = rig.wait("child", background.StatusWaiting)
	require.Equal(t, ids, waitContainers(t, run, 1))
	key := record.Waits[0].Path[len(record.Waits[0].Path)-1].Key
	require.NoError(t, rig.manager.Resume(context.Background(), "child", map[string]string{key: preset.ApprovalAllow}))
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "1 marker.txt", "批准后只执行了一次")
	waitContainers(t, run, 0)
}
