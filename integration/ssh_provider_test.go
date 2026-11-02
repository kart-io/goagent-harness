package integration_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/sandboxtest"
	"github.com/kart-io/goagent-harness/sandbox/sshsandbox"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const (
	sshTestImage  = "goagent-ssh-test:1"
	sshBaseDir    = "/home/agent/sandboxes"
	sshRunnerPath = "/usr/local/bin/sandbox-runner"
)

// 真实的 OpenSSH。容器里没有 bwrap，隔离由 Landlock 兑现。
const sshDockerfile = `FROM ubuntu:24.04
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends openssh-server curl && useradd -m -s /bin/sh agent && mkdir -p /run/sshd /home/agent/.ssh && chown agent /home/agent/.ssh && rm -f /etc/ssh/ssh_host_*
`

type sshFixture struct {
	container string
	config    sshsandbox.Config
}

func sshDocker(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := dockerRun(ctx, args...)
	require.NoError(t, err)
	return out
}

func startSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	requireDocker(t)
	if exec.Command("docker", "image", "inspect", sshTestImage).Run() != nil {
		build := exec.Command("docker", "build", "-q", "-t", sshTestImage, "-")
		build.Stdin = strings.NewReader(sshDockerfile)
		out, err := build.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	dir := t.TempDir()
	runner := filepath.Join(dir, "sandbox-runner")
	build := exec.Command("go", "build", "-o", runner, "github.com/kart-io/goagent-harness/cmd/sandbox-runner")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	data, err := os.ReadFile(runner)
	require.NoError(t, err)
	digest := sha256.Sum256(data)

	userPublic, userPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	userSigner, err := ssh.NewSignerFromKey(userPrivate)
	require.NoError(t, err)
	userKey, err := ssh.NewPublicKey(userPublic)
	require.NoError(t, err)
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(hostPrivate, "")
	require.NoError(t, err)
	hostFile := filepath.Join(dir, "host_key")
	require.NoError(t, os.WriteFile(hostFile, pem.EncodeToMemory(block), 0o600))
	keysFile := filepath.Join(dir, "authorized_keys")
	require.NoError(t, os.WriteFile(keysFile, ssh.MarshalAuthorizedKey(userKey), 0o644))

	run := dockerRunID(t)
	id := sshDocker(t, "run", "-d", "--label", dockerLabelRun+"="+run, "-p", "127.0.0.1::22", sshTestImage, "sleep", "infinity")
	sshDocker(t, "cp", hostFile, id+":/etc/ssh/ssh_host_ed25519_key")
	sshDocker(t, "cp", keysFile, id+":/home/agent/.ssh/authorized_keys")
	sshDocker(t, "cp", runner, id+":"+sshRunnerPath)
	sshDocker(t, "exec", id, "sh", "-c", "chown root /etc/ssh/ssh_host_ed25519_key; chmod 600 /etc/ssh/ssh_host_ed25519_key; chown agent /home/agent/.ssh/authorized_keys; chmod 755 "+sshRunnerPath+"; /usr/sbin/sshd -o MaxSessions=20")
	addr := strings.TrimSpace(sshDocker(t, "port", id, "22/tcp"))

	f := &sshFixture{container: id, config: sshsandbox.Config{
		Addr: addr, User: "agent", Auth: []ssh.AuthMethod{ssh.PublicKeys(userSigner)},
		HostKeyCallback: ssh.FixedHostKey(hostSigner.PublicKey()),
		BaseDir:         sshBaseDir, RunnerPath: sshRunnerPath, RunnerSHA256: hex.EncodeToString(digest[:]),
	}}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		p := f.provider(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, err := p.FindByScope(ctx, sandbox.Request{Scope: "ready"})
		cancel()
		if err == nil {
			return f
		}
		require.True(t, time.Now().Before(deadline), "sshd 没有就绪: %v", err)
	}
}

func (f *sshFixture) provider(t *testing.T) *sshsandbox.Provider {
	t.Helper()
	p, err := sshsandbox.New(f.config)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// sh 在容器里以 agent 身份运行，绕过 SSH，用来观察和破坏状态。
func (f *sshFixture) sh(t *testing.T, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-u", "agent", f.container, "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return string(out), exited.ExitCode()
	}
	require.NoError(t, err, string(out))
	return string(out), 0
}

func (f *sshFixture) Stop(context.Context, string) error { return sandbox.ErrUnsupported }
func (f *sshFixture) Exists(_ context.Context, id string) (bool, error) {
	out, err := exec.Command("docker", "exec", f.container, "sh", "-c", "test -e "+sshBaseDir+"/"+id+" && echo yes || echo no").CombinedOutput()
	return strings.TrimSpace(string(out)) == "yes", err
}

func (f *sshFixture) create(t *testing.T, network bool) (sandbox.Environment, *sshsandbox.Provider) {
	t.Helper()
	p := f.provider(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := p.Create(ctx, sandbox.Request{Provider: "ssh", Scope: rand.Text(), Network: network})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.Destroy(cleanup)
	})
	return e, p
}

func shellOf(script string) env.Command { return env.Command{Argv: []string{"sh", "-c", script}} }

func TestSSHProviderContract(t *testing.T) {
	f := startSSHFixture(t)
	sandboxtest.Run(t, sandboxtest.Harness{Provider: f.provider(t), Admin: f})
}

// 容器里没有 bwrap，这条能证明命令真的经过了 Landlock，而不是靠容器。
func TestSSHReportsRunnerAndConfinesCommands(t *testing.T) {
	f := startSSHFixture(t)
	e, p := f.create(t, false)
	ctx := context.Background()
	report, err := p.Probe(ctx, false)
	require.NoError(t, err)
	require.Equal(t, sandbox.RunnerLandlock, report.Runner)
	other, _ := f.create(t, false)
	_, err = other.FileSystem().WriteFile(ctx, "secret.txt", []byte("原文"), env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)

	run := func(script string) env.Result {
		t.Helper()
		res, err := e.Executor().Run(ctx, shellOf(script))
		require.NoError(t, err)
		return res
	}
	require.Zero(t, run("echo ok > inside").ExitCode)
	otherFile := other.Root() + "/secret.txt"
	for name, script := range map[string]string{
		"家目录":   ": > /home/agent/escaped",
		"/tmp":  ": > /tmp/escaped",
		"别的实例":  "echo x >> " + otherFile,
		"实例目录":  ": > ../escaped",
		"监管状态":  ": > ../state/forged",
		"删别的实例": "rm -f " + otherFile,
	} {
		require.NotZero(t, run(script).ExitCode, name)
	}
	data, _, err := other.FileSystem().ReadFile(ctx, "secret.txt")
	require.NoError(t, err)
	require.Equal(t, "原文", string(data))
	out, code := f.sh(t, "ls -A /home/agent; ls -A /tmp")
	require.Zero(t, code)
	require.NotContains(t, out, "escaped")

	policy := sandbox.Policy{Mode: sandbox.ReadOnly, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()}
	executor := e.Executor().(sandbox.PolicyExecutor)
	res, err := executor.RunWithPolicy(ctx, shellOf(": > readonly"), policy)
	require.NoError(t, err)
	require.NotZero(t, res.ExitCode)
	res, err = executor.RunWithPolicy(ctx, shellOf("cat inside"), policy)
	require.NoError(t, err)
	require.Zero(t, res.ExitCode)

	for name, bad := range map[string]sandbox.Policy{
		"放开网络": {Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkInherit, WorkspaceRoot: e.Root()},
		"完全访问": {Mode: sandbox.DangerFullAccess, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()},
		"额外目录": {Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root(), AdditionalWritableRoots: []string{"/tmp"}},
	} {
		_, err := executor.RunWithPolicy(ctx, shellOf(": > bad"), bad)
		require.ErrorIs(t, err, sandbox.ErrUnsupported, name)
	}
	// WorkspaceRoot 是宿主的授权域，写成 / 也不能让命令写到实例之外。
	res, err = executor.RunWithPolicy(ctx, shellOf(": > /home/agent/escaped-domain"), sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: "/"})
	require.NoError(t, err)
	require.NotZero(t, res.ExitCode)
	_, err = e.Executor().Run(ctx, env.Command{Argv: []string{"true"}, Dir: "../.."})
	require.ErrorIs(t, err, sandbox.ErrDenied)
}

func TestSSHNetworkPolicy(t *testing.T) {
	f := startSSHFixture(t)
	ctx := context.Background()
	// sshd 回的不是 HTTP，连上了 curl 也退出非零，所以看错误码：1 是连上了协议不对。
	probe := env.Command{Argv: []string{"curl", "--noproxy", "*", "--max-time", "3", "-s", "-S", "http://127.0.0.1:22/"}}
	denied, _ := f.create(t, false)
	res, err := denied.Executor().Run(ctx, probe)
	require.NoError(t, err)
	require.NotContains(t, []int{0, 1}, res.ExitCode, string(res.Output))
	allowed, _ := f.create(t, true)
	res, err = allowed.Executor().Run(ctx, probe)
	require.NoError(t, err)
	require.Contains(t, []int{0, 1}, res.ExitCode, string(res.Output))
}

func TestSSHRejectsWrongHostAndRunner(t *testing.T) {
	f := startSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	wrongHost := f.config
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherSigner, err := ssh.NewSignerFromKey(otherKey)
	require.NoError(t, err)
	wrongHost.HostKeyCallback = ssh.FixedHostKey(otherSigner.PublicKey())
	p, err := sshsandbox.New(wrongHost)
	require.NoError(t, err)
	defer p.Close()
	scope := rand.Text()
	_, err = p.Create(ctx, sandbox.Request{Provider: "ssh", Scope: scope})
	require.ErrorIs(t, err, sandbox.ErrNotCreated, "%v", err)

	wrongRunner := f.config
	wrongRunner.RunnerSHA256 = strings.Repeat("0", 64)
	p, err = sshsandbox.New(wrongRunner)
	require.NoError(t, err)
	defer p.Close()
	_, err = p.Create(ctx, sandbox.Request{Provider: "ssh", Scope: scope})
	require.ErrorIs(t, err, sandbox.ErrNotCreated)
	require.ErrorIs(t, err, sandbox.ErrUnavailable)
	out, _ := f.sh(t, "ls -A "+sshBaseDir+" 2>/dev/null | grep -c sbx-")
	require.Equal(t, "0", strings.TrimSpace(out), "校验失败不能留下实例")
}

func TestSSHInvalidIDsCannotReachOutside(t *testing.T) {
	f := startSSHFixture(t)
	p := f.provider(t)
	ctx := context.Background()
	f.sh(t, "mkdir -p "+sshBaseDir+"/keep && touch /home/agent/keep-me")
	for _, id := range []string{"..", "../..", "keep", "sbx-x/../../keep-me", "sbx-$(touch /tmp/pwn)"} {
		_, err := p.Connect(ctx, id, sandbox.Request{})
		require.ErrorIs(t, err, sandbox.ErrInstanceNotFound, id)
		require.ErrorIs(t, p.DestroyInstance(ctx, id, sandbox.Request{}), sandbox.ErrInstanceNotFound, id)
	}
	_, code := f.sh(t, "test -d "+sshBaseDir+"/keep && test -f /home/agent/keep-me && test ! -e /tmp/pwn")
	require.Zero(t, code)
}

func TestSSHFileSystemBoundary(t *testing.T) {
	f := startSSHFixture(t)
	e, _ := f.create(t, false)
	ctx := context.Background()
	fsys := e.FileSystem()
	f.sh(t, "cd "+e.Root()+" && mkdir d && ln -s /home/agent out && ln -s /nonexistent/x dangling && ln -s d inner && touch /home/agent/victim")
	for _, p := range []string{"../escape", "/etc/passwd", "out/victim", "out/new/deep/file", "dangling", "dangling/x"} {
		_, err := fsys.WriteFile(ctx, p, []byte("x"), env.WriteIntent{})
		require.ErrorIs(t, err, env.ErrOutsideWorkspace, p)
		_, _, err = fsys.ReadFile(ctx, p)
		require.ErrorIs(t, err, env.ErrOutsideWorkspace, p)
		require.ErrorIs(t, fsys.Remove(ctx, p), env.ErrOutsideWorkspace, p)
	}
	_, code := f.sh(t, "test -f /home/agent/victim && test ! -e /home/agent/new && test ! -s /home/agent/victim")
	require.Zero(t, code)
	// 工作区内的链接照常可用。
	_, err := fsys.WriteFile(ctx, "inner/via.txt", []byte("x"), env.WriteIntent{})
	require.NoError(t, err)
	// 绝对路径落在工作区内也接受。
	got, _, err := fsys.ReadFile(ctx, e.Root()+"/d/via.txt")
	require.NoError(t, err)
	require.Equal(t, "x", string(got))
	require.Error(t, fsys.Remove(ctx, "."))

	guarded := fsys.(sandbox.MutationProtector).WithProtectedNames(".git")
	_, err = guarded.WriteFile(ctx, ".git/config", []byte("x"), env.WriteIntent{})
	require.ErrorIs(t, err, sandbox.ErrDenied)
	require.ErrorIs(t, guarded.Remove(ctx, ".git"), sandbox.ErrDenied)

	// 换行不能断开或伪造 Walk 的记录。
	tricky := "line\n1|81a4|0|/etc/passwd"
	_, err = fsys.WriteFile(ctx, tricky, []byte("x"), env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)
	var seen []string
	require.NoError(t, fsys.Walk(ctx, ".", func(info env.Info) error {
		seen = append(seen, info.Path)
		return nil
	}))
	require.Contains(t, seen, tricky)
	require.NotContains(t, seen, "/etc/passwd")
	_, _, err = fsys.ReadFile(ctx, "absent")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func (f *sshFixture) waitGone(t *testing.T, marker string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		// sh -c 的命令行也包含这个模式，用 [ ] 避开自己。
		out, _ := f.sh(t, "pgrep -f "+shellArg(marker)+" || true")
		if strings.TrimSpace(out) == "" {
			return
		}
		require.True(t, time.Now().Before(deadline), "进程还在: %s", out)
	}
}

func shellArg(s string) string { return "'" + s + "'" }

func (f *sshFixture) waitFile(t *testing.T, file string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, code := f.sh(t, "test -e "+shellArg(file)); code == 0 {
			return
		}
		require.True(t, time.Now().Before(deadline), "没有出现 %s", file)
	}
}

func TestSSHCancelStopsCommandAndDescendants(t *testing.T) {
	f := startSSHFixture(t)
	e, _ := f.create(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := e.Executor().Run(ctx, shellOf("sleep 4242 & sleep 4243 & : > started; wait"))
		done <- err
	}()
	f.waitFile(t, e.Root()+"/started")
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	f.waitGone(t, "[s]leep 424[23]")
}

// 只断开这条 SSH 连接，sshd 还在，宿主可以重连。
func (f *sshFixture) dropConnections(t *testing.T) {
	t.Helper()
	out, err := exec.Command("docker", "exec", f.container, "sh", "-c", "pkill -KILL -f '[s]shd.*agent' || true").CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestSSHConnectionDrop(t *testing.T) {
	f := startSSHFixture(t)
	e, p := f.create(t, false)
	ctx := context.Background()
	keyed := e.Executor().(sandbox.KeyedExecutor)
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()}
	command := sandbox.KeyedCommand{Key: "long", Timeout: time.Minute, Command: shellOf("echo x >> effect; until [ -e release ]; do sleep 0.05; done; echo 结果; exit 9")}

	keyedDone := make(chan error, 1)
	go func() {
		_, err := keyed.RunKeyed(ctx, command, policy)
		keyedDone <- err
	}()
	f.waitFile(t, e.Root()+"/effect")
	plainDone := make(chan error, 1)
	go func() {
		_, err := e.Executor().Run(ctx, shellOf(": > plain-started; sleep 5151"))
		plainDone <- err
	}()
	f.waitFile(t, e.Root()+"/plain-started")

	f.dropConnections(t)
	require.Error(t, <-plainDone, "普通命令断线后要报错，结果未知")
	f.waitGone(t, "[s]leep 5151") // 没有宿主接收结果，普通命令不能留在远端

	require.NoError(t, p.Close())
	reconnected := f.provider(t)
	e2, err := reconnected.Connect(ctx, e.ID(), sandbox.Request{Provider: "ssh"})
	require.NoError(t, err)
	_, err = e2.FileSystem().WriteFile(ctx, "release", nil, env.WriteIntent{CreateIfAbsent: true})
	require.NoError(t, err)
	command.AttachOnly = true
	res, err := e2.Executor().(sandbox.KeyedExecutor).RunKeyed(ctx, command, policy)
	require.NoError(t, err)
	require.Equal(t, 9, res.ExitCode)
	require.Contains(t, string(res.Output), "结果")
	data, _, err := e2.FileSystem().ReadFile(ctx, "effect")
	require.NoError(t, err)
	require.Equal(t, "x\n", string(data), "副作用只能有一次")
	<-keyedDone
}

func TestSSHKeyedTimeoutKeepsOutputAndKillsDescendants(t *testing.T) {
	f := startSSHFixture(t)
	e, _ := f.create(t, false)
	keyed := e.Executor().(sandbox.KeyedExecutor)
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()}
	res, err := keyed.RunKeyed(context.Background(), sandbox.KeyedCommand{Key: "slow", Timeout: time.Second, Command: shellOf("echo 已有输出; sleep 6161 & wait")}, policy)
	require.NoError(t, err)
	require.True(t, res.TimedOut)
	require.Contains(t, string(res.Output), "已有输出")
	f.waitGone(t, "[s]leep 6161")
}

func TestSSHDestroyStopsKeyedCommands(t *testing.T) {
	f := startSSHFixture(t)
	e, _ := f.create(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyed := e.Executor().(sandbox.KeyedExecutor)
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()}
	go func() {
		_, _ = keyed.RunKeyed(ctx, sandbox.KeyedCommand{Key: "k", Timeout: time.Hour, Command: shellOf(": > started; sleep 7171")}, policy)
	}()
	f.waitFile(t, e.Root()+"/started")
	cancel()
	require.NoError(t, e.Destroy(context.Background()))
	f.waitGone(t, "[s]leep 7171")
	exists, err := f.Exists(context.Background(), e.ID())
	require.NoError(t, err)
	require.False(t, exists)
}

// 经过 preset 和后台子工作：宿主停止时命令还在跑，重启后连回实例取回结果，副作用只有一次，结束后实例被回收。
func TestSSHStopDuringCommandAttaches(t *testing.T) {
	f := startSSHFixture(t)
	p := f.provider(t)
	dir := t.TempDir()
	command := "echo once >> effect; while [ ! -e release ]; do sleep 0.05; done; echo 命令结果"
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "run_command", map[string]any{"command": command})}}
	rig := startRig(t, rigConfig{dir: dir, namespace: "ssh", provider: p, run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "执行")
	workspace := ""
	for deadline := time.Now().Add(60 * time.Second); workspace == ""; time.Sleep(50 * time.Millisecond) {
		out, _ := f.sh(t, "ls -d "+sshBaseDir+"/sbx-*/workspace/effect 2>/dev/null || true")
		workspace = strings.TrimSuffix(strings.TrimSpace(out), "/effect")
		require.True(t, time.Now().Before(deadline), "命令没有启动")
	}
	rig.stop()

	second := secondModel()
	rig = startRig(t, rigConfig{dir: dir, namespace: "ssh", provider: f.provider(t), run: preset.BuilderRunner(second, 8)})
	out, _ := f.sh(t, "cat "+workspace+"/effect; touch "+workspace+"/release")
	require.Equal(t, "once\n", out)
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "命令结果")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out, _ := f.sh(t, "ls -A "+sshBaseDir+" | grep -c sbx-")
		if strings.TrimSpace(out) == "0" {
			break
		}
		require.True(t, time.Now().Before(deadline), "实例没有回收: %s", out)
	}
}

// 分配成功但响应丢失：ID 由 Scope 决定，释放时能查回来删除，不靠 TTL。
func TestSSHLostCreateResponseDoesNotLeak(t *testing.T) {
	f := startSSHFixture(t)
	faults := &sandboxtest.Faults{Provider: f.provider(t)}
	faults.LoseCreateResponse.Store(1)
	var ran atomic.Bool
	rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: "ssh", provider: faults,
		run: func(context.Context, *preset.Assembly, string) (string, error) {
			ran.Store(true)
			return "", nil
		}})
	rig.spawn("child", "任务")
	rig.wait("child", background.StatusFailed)
	require.False(t, ran.Load())
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out, _ := f.sh(t, "ls -A "+sshBaseDir+" 2>/dev/null | grep -c sbx-")
		if strings.TrimSpace(out) == "0" {
			break
		}
		require.True(t, time.Now().Before(deadline), "实例泄漏: %s", out)
	}
	waitNoProblems(t, rig.manager)
}
