package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

// localPolicyExecutor 在本机运行脚本，代替真实沙箱。hang 大于零时让前几次请求执行后不返回，模拟 docker exec 卡住。
type localPolicyExecutor struct {
	dir   string
	hang  atomic.Int32
	calls atomic.Int32
	fail  atomic.Int32
}

func (e *localPolicyExecutor) Capabilities() env.Capabilities { return env.Capabilities{} }
func (e *localPolicyExecutor) Run(ctx context.Context, c env.Command) (env.Result, error) {
	return e.RunWithPolicy(ctx, c, Policy{})
}
func (e *localPolicyExecutor) CheckPolicy(context.Context, Policy) error { return nil }
func (e *localPolicyExecutor) RunWithPolicy(ctx context.Context, c env.Command, _ Policy) (env.Result, error) {
	e.calls.Add(1)
	if e.fail.Load() > 0 {
		e.fail.Add(-1)
		return env.Result{}, errors.New("连接断开")
	}
	cmd := exec.Command(c.Argv[0], c.Argv[1:]...)
	cmd.Dir = e.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if e.hang.Load() > 0 {
		e.hang.Add(-1)
		<-ctx.Done()
		return env.Result{}, ctx.Err()
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		return env.Result{Output: out.Bytes(), ExitCode: exited.ExitCode()}, nil
	}
	return env.Result{Output: out.Bytes()}, err
}

type supervisorRig struct {
	t      *testing.T
	exec   *localPolicyExecutor
	s      *CommandSupervisor
	marker string
}

func newSupervisorRig(t *testing.T) *supervisorRig {
	t.Helper()
	for _, tool := range []string{"sh", "setsid", "flock", "mkfifo", "head"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("需要 %s", tool)
		}
	}
	dir := t.TempDir()
	e := &localPolicyExecutor{dir: dir}
	s := &CommandSupervisor{Exec: e, StateDir: filepath.Join(t.TempDir(), "state"), PollInterval: 10 * time.Millisecond, RequestTimeout: 2 * time.Second, LostGrace: 200 * time.Millisecond}
	return &supervisorRig{t: t, exec: e, s: s, marker: filepath.Join(dir, "effect")}
}

func (r *supervisorRig) command(key, script string) KeyedCommand {
	r.t.Cleanup(func() { _, _ = r.s.CancelKeyed(context.Background(), key, Policy{}) })
	return KeyedCommand{Key: key, Command: env.Command{Argv: []string{"sh", "-c", script}}, Timeout: time.Minute}
}

func (r *supervisorRig) effects() int {
	data, _ := os.ReadFile(r.marker)
	return strings.Count(string(data), "\n")
}

func TestSupervisorReturnsOutputAndExitCode(t *testing.T) {
	r := newSupervisorRig(t)
	res, err := r.s.RunKeyed(context.Background(), r.command("k", "echo 输出; echo 错误 >&2; exit 7"), Policy{})
	require.NoError(t, err)
	require.Equal(t, 7, res.ExitCode)
	require.False(t, res.TimedOut)
	require.Contains(t, string(res.Output), "输出")
	require.Contains(t, string(res.Output), "错误")
}

func TestSupervisorRunsInCommandDirectory(t *testing.T) {
	r := newSupervisorRig(t)
	res, err := r.s.RunKeyed(context.Background(), r.command("k", "pwd"), Policy{})
	require.NoError(t, err)
	real, err := filepath.EvalSymlinks(r.exec.dir)
	require.NoError(t, err)
	require.Contains(t, string(res.Output), real)
}

func TestSupervisorSameKeyStartsOnce(t *testing.T) {
	r := newSupervisorRig(t)
	c := r.command("k", "echo x >> "+shellQuote(r.marker)+"; echo 结果")
	first, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	second, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	require.Equal(t, first.Output, second.Output)
	require.Equal(t, 1, r.effects())
}

// 断开连接不影响命令：调用方走了，命令还在跑，之后附着取回结果。
func TestSupervisorCommandSurvivesDisconnectAndReattaches(t *testing.T) {
	r := newSupervisorRig(t)
	release := filepath.Join(r.exec.dir, "release")
	script := "echo x >> " + shellQuote(r.marker) + "; while [ ! -e " + shellQuote(release) + " ]; do sleep 0.02; done; echo 完成"
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.s.RunKeyed(ctx, r.command("k", script), Policy{})
		done <- err
	}()
	require.Eventually(t, func() bool { return r.effects() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	attach := r.command("k", script)
	attach.AttachOnly = true
	waiting := make(chan KeyedResult, 1)
	go func() {
		res, err := r.s.RunKeyed(context.Background(), attach, Policy{})
		if err == nil {
			waiting <- res
		}
	}()
	select {
	case <-waiting:
		t.Fatal("命令还没结束")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, os.WriteFile(release, nil, 0o600))
	require.Contains(t, string((<-waiting).Output), "完成")
	require.Equal(t, 1, r.effects())
}

// 启动请求卡住：命令可能已经启动。重试只会附着，不会多跑一次。
func TestSupervisorStartRetryDoesNotRepeatCommand(t *testing.T) {
	r := newSupervisorRig(t)
	r.s.RequestTimeout = 100 * time.Millisecond
	r.exec.hang.Store(2)
	res, err := r.s.RunKeyed(context.Background(), r.command("k", "echo x >> "+shellQuote(r.marker)+"; echo 完成"), Policy{})
	require.NoError(t, err)
	require.Contains(t, string(res.Output), "完成")
	require.Equal(t, 1, r.effects())
}

func TestSupervisorAttachOnlyNeverStarts(t *testing.T) {
	r := newSupervisorRig(t)
	c := r.command("k", "echo x >> "+shellQuote(r.marker))
	c.AttachOnly = true
	_, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.ErrorIs(t, err, ErrCommandLost)
	require.Zero(t, r.effects(), "状态被删和崩溃在启动前都查不到记录，不能因此重跑")
}

// 进程没了、结果也没有（如容器重启）：是 lost，不是还在运行。
func TestSupervisorDeadCommandIsLost(t *testing.T) {
	r := newSupervisorRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.s.RunKeyed(ctx, r.command("k", "echo x >> "+shellQuote(r.marker)+"; sleep 60"), Policy{})
		done <- err
	}()
	require.Eventually(t, func() bool { return r.effects() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	dir := r.s.stateDir("k")
	pid, err := os.ReadFile(filepath.Join(dir, "supervisor"))
	require.NoError(t, err)
	require.NoError(t, exec.Command("sh", "-c", "kill -KILL "+strings.TrimSpace(string(pid))).Run())
	_, _ = r.s.CancelKeyed(context.Background(), "k", Policy{})

	c := r.command("k", "true")
	c.AttachOnly = true
	_, err = r.s.RunKeyed(context.Background(), c, Policy{})
	require.ErrorIs(t, err, ErrCommandLost)
}

// 宿主可能不在，超时由沙箱内执行，并且要杀掉整个进程组。
func TestSupervisorEnforcesTimeoutInSandbox(t *testing.T) {
	r := newSupervisorRig(t)
	child := filepath.Join(r.exec.dir, "child-pid")
	c := r.command("k", "sleep 300 & echo $! > "+shellQuote(child)+"; wait")
	c.Timeout = 300 * time.Millisecond
	begin := time.Now()
	res, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	require.True(t, res.TimedOut)
	require.Less(t, time.Since(begin), 30*time.Second)
	pid, err := os.ReadFile(child)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return exec.Command("sh", "-c", "kill -0 "+strings.TrimSpace(string(pid))).Run() != nil
	}, 5*time.Second, 10*time.Millisecond, "子进程也被杀掉")
}

func TestSupervisorCapsOutputWithoutKillingCommand(t *testing.T) {
	r := newSupervisorRig(t)
	// 写输出的是 shell 本身：管道被关闭的话，它会被 SIGPIPE 杀掉，走不到后面。
	c := r.command("k", "i=0; while [ $i -lt 2000 ]; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; i=$((i+1)); done; echo x >> "+shellQuote(r.marker)+"; exit 3")
	c.MaxOutput = 1000
	res, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	require.Equal(t, 3, res.ExitCode)
	require.Len(t, res.Output, 1000)
	require.Equal(t, 1, r.effects(), "输出超限不影响命令")
}

func TestSupervisorCancel(t *testing.T) {
	r := newSupervisorRig(t)
	started, err := r.s.CancelKeyed(context.Background(), "never", Policy{})
	require.NoError(t, err)
	require.False(t, started, "没有启动过")

	done := make(chan KeyedResult, 1)
	go func() {
		res, err := r.s.RunKeyed(context.Background(), r.command("k", "echo x >> "+shellQuote(r.marker)+"; sleep 60"), Policy{})
		if err == nil {
			done <- res
		}
	}()
	require.Eventually(t, func() bool { return r.effects() == 1 }, 5*time.Second, 5*time.Millisecond)
	started, err = r.s.CancelKeyed(context.Background(), "k", Policy{})
	require.NoError(t, err)
	require.True(t, started)
	select {
	case res := <-done:
		require.NotZero(t, res.ExitCode)
	case <-time.After(5 * time.Second):
		t.Fatal("命令没有被终止")
	}
}

// 查询偶尔失败不影响结果；一直失败要报告结果未知，不能假装成功或无限等待。
func TestSupervisorPollingFailures(t *testing.T) {
	r := newSupervisorRig(t)
	c := r.command("k", "echo 完成")
	res, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	r.exec.fail.Store(2)
	again, err := r.s.RunKeyed(context.Background(), c, Policy{})
	require.NoError(t, err)
	require.Equal(t, res.Output, again.Output)

	r.exec.fail.Store(1000)
	_, err = r.s.RunKeyed(context.Background(), c, Policy{})
	require.ErrorIs(t, err, ErrUnconfirmed, "启动一直失败")

	c.AttachOnly = true
	_, err = r.s.RunKeyed(context.Background(), c, Policy{})
	require.ErrorIs(t, err, ErrUnconfirmed, "查询一直失败")
}

func TestSupervisorRequiresTools(t *testing.T) {
	r := newSupervisorRig(t)
	r.t.Setenv("PATH", t.TempDir())
	sh, err := exec.LookPath("/bin/sh")
	require.NoError(t, err)
	c := KeyedCommand{Key: "k", Command: env.Command{Argv: []string{"true"}}, Timeout: time.Second}
	r.s.Shell = sh
	_, err = r.s.RunKeyed(context.Background(), c, Policy{})
	require.ErrorIs(t, err, ErrUnsupported)
}
