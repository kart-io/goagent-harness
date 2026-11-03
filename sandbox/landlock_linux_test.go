//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kart-io/goagent/harness/env"
)

func landlockExecutor(t *testing.T, p Policy) *Executor {
	t.Helper()
	if abi := landlockABI(); abi < landlockMinABI {
		t.Skipf("内核 Landlock ABI %d 不可用", abi)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("网络探测需要 curl")
	}
	return &Executor{Backend: "native", Runner: RunnerLandlock, Policy: p}
}

func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func runShell(t *testing.T, e *Executor, script string) env.Result {
	t.Helper()
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", script}})
	if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return r
}

func TestLandlockProbeReportsRunner(t *testing.T) {
	e := landlockExecutor(t, Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny})
	report, err := e.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "full"
	if landlockABI() < landlockFullABI {
		want = "partial"
	}
	if report.Runner != RunnerLandlock || report.Enforcement != want {
		t.Fatalf("%+v", report)
	}
}

func TestLandlockWriteBoundary(t *testing.T) {
	root, outside, extra := realTemp(t), realTemp(t), realTemp(t)
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny, AdditionalWritableRoots: []string{extra}})
	q := shellQuote
	denied := map[string]string{
		"create":   ": > " + q(filepath.Join(outside, "new")),
		"append":   "echo x >> " + q(victim),
		"truncate": "truncate -s 0 " + q(victim),
		"remove":   "rm -f " + q(victim),
		"rename":   "mv " + q(victim) + " " + q(filepath.Join(root, "moved")),
		"link":     "ln " + q(victim) + " " + q(filepath.Join(root, "linked")) + " && echo x >> " + q(filepath.Join(root, "linked")),
		"mkdir":    "mkdir " + q(filepath.Join(outside, "dir")),
		"symlink":  "ln -s /etc " + q(filepath.Join(outside, "sym")),
		"fifo":     "mkfifo " + q(filepath.Join(outside, "fifo")),
		"chain":    "sh -c " + q(": > "+q(filepath.Join(outside, "chain"))),
		"alias":    ": > escape/new",
		"procroot": ": > " + q(fmt.Sprintf("/proc/%d/root%s/proc", os.Getpid(), outside)),
	}
	for name, script := range denied {
		if r := runShell(t, e, script); r.ExitCode == 0 {
			t.Errorf("%s 未被拒绝: %s", name, r.Output)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("外部目录被修改: %v", entries)
	}
	if data, _ := os.ReadFile(victim); string(data) != "intact" {
		t.Errorf("外部文件被修改: %q", data)
	}
	allowed := "set -e; echo a > a; mkdir d; mv a d/a; mv d/a " + q(filepath.Join(extra, "a")) + "; truncate -s 0 " + q(filepath.Join(extra, "a")) + "; p=$(mktemp \"$TMPDIR/t.XXXXXX\"); echo t > \"$p\"; echo sink > /dev/null"
	if r := runShell(t, e, allowed); r.ExitCode != 0 {
		t.Fatalf("允许的操作失败: %s", r.Output)
	}
	e.Policy.Mode = ReadOnly
	if r := runShell(t, e, ": > ro"); r.ExitCode == 0 {
		t.Fatal("只读模式写入了工作区")
	}
	if r := runShell(t, e, "echo sink > /dev/null"); r.ExitCode != 0 {
		t.Fatalf("/dev/null: %s", r.Output)
	}
}

// 直接发系统调用：truncate、mv 这些命令会先 open 或在 EXDEV 时退回复制，测不到这两条路径。
func TestLandlockDirectSyscalls(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("没有 python3")
	}
	root, outside, extra := realTemp(t), realTemp(t), realTemp(t)
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny, AdditionalWritableRoots: []string{extra}})
	run := func(code string) env.Result {
		t.Helper()
		r, err := e.Run(context.Background(), env.Command{Argv: []string{python, "-c", "import os\n" + code}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := run(fmt.Sprintf("os.truncate(%q, 0)", victim)); r.ExitCode == 0 {
		t.Fatal("truncate(2) 越界成功")
	}
	if data, _ := os.ReadFile(victim); string(data) != "intact" {
		t.Fatalf("外部文件被截断: %q", data)
	}
	// git 这类工具依赖跨目录的原子 rename，不能退化成 EXDEV。
	ok := fmt.Sprintf("os.mkdir('a'); os.mkdir('b'); open('a/f','w').close(); os.rename('a/f','b/f'); os.link('b/f','a/l'); os.rename('b/f', %q)", filepath.Join(extra, "f"))
	if r := run(ok); r.ExitCode != 0 {
		t.Fatalf("工作区内的 rename 和 link 失败: %s", r.Output)
	}
}

// 命令退出后留下的后代已经脱离进程组，清理不到（见 README），但仍然受限。
func TestLandlockDetachedDescendantStaysConfined(t *testing.T) {
	root, outside := realTemp(t), realTemp(t)
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny})
	late := filepath.Join(root, "late")
	started := filepath.Join(root, "started")
	inner := ": > " + shellQuote(started) + "; sleep 0.3; sh -c " + shellQuote(": > "+shellQuote(filepath.Join(outside, "late"))) + "; echo $? > " + shellQuote(late)
	if r := runShell(t, e, "setsid sh -c "+shellQuote(inner)+" >/dev/null 2>&1 </dev/null & until [ -e "+shellQuote(started)+" ]; do sleep 0.01; done"); r.ExitCode != 0 {
		t.Fatalf("%s", r.Output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(late)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			if strings.TrimSpace(string(data)) == "0" {
				t.Fatal("后代写到了外部")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("后代没有结束")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(outside, "late")); !os.IsNotExist(err) {
		t.Fatal("外部文件被创建")
	}
}

func TestLandlockProcessIsolation(t *testing.T) {
	e := landlockExecutor(t, Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny})
	if r := runShell(t, e, `read -r _ _ _ _ _ sid _ < /proc/$$/stat; test "$sid" = "$$"`); r.ExitCode != 0 {
		t.Fatalf("没有新会话: %s", r.Output)
	}
	// ptrace 范围限制让沙箱读不到宿主进程的环境和内存。
	if r := runShell(t, e, fmt.Sprintf("cat /proc/%d/environ", os.Getpid())); r.ExitCode == 0 {
		t.Fatal("读到了宿主进程环境")
	}
	if landlockABI() < landlockFullABI {
		return
	}
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()
	if r := runShell(t, e, fmt.Sprintf("kill -9 %d", victim.Process.Pid)); r.ExitCode == 0 {
		t.Fatal("向沙箱外的进程发了信号")
	}
	if err := victim.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("受害进程已退出: %v", err)
	}
}

func TestLandlockNetworkInherit(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("没有 curl")
	}
	e := landlockExecutor(t, Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkInherit})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stop := startProbeHTTP(listener)
	defer stop()
	url := "http://" + listener.Addr().String()
	r, err := e.Run(context.Background(), env.Command{Argv: []string{curl, "--noproxy", "*", "--max-time", "5", url}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("inherit 下 TCP 被拦: %s %v", r.Output, err)
	}
	e.Policy.Network = NetworkDeny
	r, err = e.Run(context.Background(), env.Command{Argv: []string{curl, "--noproxy", "*", "--max-time", "5", url}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("deny 下 TCP 可用: %s %v", r.Output, err)
	}
}

// 受限的线程不能影响宿主：并发运行时，宿主的新旧线程都能写沙箱外。
func TestLandlockDoesNotConfineHost(t *testing.T) {
	root, outside := realTemp(t), realTemp(t)
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny})
	p, err := effective(context.Background(), e.Policy)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread() // 逼运行时不断创建新线程
			defer runtime.UnlockOSThread()
			path := filepath.Join(outside, fmt.Sprintf("host-%d", i))
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Errorf("宿主被限制: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	var runs sync.WaitGroup
	for range 8 {
		runs.Add(1)
		go func() {
			defer runs.Done()
			for range 25 {
				r, err := e.raw(context.Background(), p, env.Command{Argv: []string{"sh", "-c", ": > " + shellQuote(filepath.Join(outside, "denied"))}})
				if err != nil || r.ExitCode == 0 {
					t.Errorf("命令未被限制: %+v %v", r, err)
					return
				}
			}
		}()
	}
	runs.Wait()
	close(stop)
	wg.Wait()
	if _, err := os.Stat(filepath.Join(outside, "denied")); !os.IsNotExist(err) {
		t.Fatal("命令写到了外部")
	}
}

func TestLandlockCancelKillsCommand(t *testing.T) {
	root := realTemp(t)
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	p, _ := effective(context.Background(), e.Policy)
	if _, err := e.raw(ctx, p, env.Command{Argv: []string{"sh", "-c", "(sleep 1; : > late2) & wait"}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(root, "late2")); !os.IsNotExist(err) {
		t.Fatal("取消后同组的子进程还活着")
	}
}

// 没有 curl 时探测不了网络拒绝，宁可报不可用也不声称兑现了策略。
func TestNativeWithoutCurlIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	e := &Executor{Backend: "native", Runner: RunnerLandlock, Policy: Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if landlockABI() < landlockMinABI {
		t.Skip("Landlock 不可用")
	}
	_, err := e.Probe(context.Background())
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "curl") {
		t.Fatal(err)
	}
}

// 内核太旧时不能假装有隔离，也不能退化成不受限运行。
func TestLandlockBelowMinimumABIIsUnavailable(t *testing.T) {
	if landlockABI() >= landlockMinABI {
		t.Skipf("内核 ABI %d 不够旧", landlockABI())
	}
	root, outside := realTemp(t), realTemp(t)
	e := &Executor{Backend: "native", Runner: RunnerLandlock, Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	p, err := effective(context.Background(), e.Policy)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(outside, "ran")
	if _, err := e.raw(context.Background(), p, env.Command{Argv: []string{"sh", "-c", ": > " + shellQuote(marker)}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("命令在没有隔离的情况下运行了")
	}
}

// ABI 3 到 5 的已知缺口，也是上报 partial 的原因：写入仍受限，但能向沙箱外发信号。
func TestLandlockPartialABIGap(t *testing.T) {
	if abi := landlockABI(); abi < landlockMinABI || abi >= landlockFullABI {
		t.Skipf("内核 ABI %d 不在 partial 范围", abi)
	}
	root, outside := realTemp(t), realTemp(t)
	e := landlockExecutor(t, Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny})
	if report, err := e.Probe(context.Background()); err != nil || report.Enforcement != "partial" {
		t.Fatalf("%+v %v", report, err)
	}
	if r := runShell(t, e, ": > "+shellQuote(filepath.Join(outside, "no"))); r.ExitCode == 0 {
		t.Fatal("越界写入成功")
	}
	victim := exec.Command("sleep", "30")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()
	if r := runShell(t, e, fmt.Sprintf("kill -9 %d", victim.Process.Pid)); r.ExitCode != 0 {
		t.Fatalf("缺口已不存在，可以更新 partial 的判断: %s", r.Output)
	}
}

// 回退不是默认：bwrap 真实可用时不应选 Landlock。
func TestNativeDefaultOrderPrefersBubblewrap(t *testing.T) {
	bwrap := &Executor{Backend: "native", Runner: RunnerBubblewrap, Policy: Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := bwrap.Probe(context.Background()); err != nil {
		t.Skipf("这台机器上 bwrap 不可用: %v", err)
	}
	auto := &Executor{Backend: "native", Policy: bwrap.Policy}
	report, err := auto.Probe(context.Background())
	if err != nil || report.Runner != RunnerBubblewrap {
		t.Fatalf("%+v %v", report, err)
	}
}

func TestNativeFallsBackToLandlock(t *testing.T) {
	if landlockABI() < landlockMinABI {
		t.Skip("Landlock 不可用")
	}
	for name, failures := range map[string]int{
		"bwrap 启动失败":    1000,
		"bwrap 假装成功不隔离": 0, // 只有探测的反向验证才能发现
	} {
		t.Run(name, func(t *testing.T) {
			fakeBwrap(t, failures, "bwrap: setting up uid map: Permission denied")
			root, outside := realTemp(t), realTemp(t)
			e := &Executor{Backend: "native", Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny}}
			report, err := e.Probe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Runner != RunnerLandlock {
				t.Fatalf("%+v", report)
			}
			r := runShell(t, e, ": > ok; : > "+shellQuote(filepath.Join(outside, "no")))
			if r.ExitCode == 0 {
				t.Fatal("越界写入成功")
			}
			if _, err := os.Stat(filepath.Join(root, "ok")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(outside, "no")); !os.IsNotExist(err) {
				t.Fatal("外部文件被创建")
			}
		})
	}
}

func TestNativeUnavailableWhenNoRunnerWorks(t *testing.T) {
	fakeBwrap(t, 1000, "bwrap: nope")
	e := &Executor{Backend: "native", Runner: "nonsense", Policy: Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	e.Runner = RunnerBubblewrap
	if _, err := e.Run(context.Background(), env.Command{Argv: []string{"true"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestRunnerRejectedForOtherBackends(t *testing.T) {
	e := &Executor{Backend: "docker", Runner: RunnerLandlock, Image: "x", Policy: Policy{WorkspaceRoot: realTemp(t), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

// fakeCommand 在 PATH 最前面放一个假命令，让探测确定地卡在或失败于这一步，不依赖机器速度。
func fakeCommand(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// 调用方的超时和取消不能被报告成沙箱不可用：调用方靠 errors.Is 区分“重试”和“换后端”。
func TestProbeInterruptKeepsContextError(t *testing.T) {
	const hang = "[ \"$1\" = rm ] && exit 0\nexec sleep 30"
	backends := []struct {
		name     string
		executor Executor
		command  string
	}{
		{"docker", Executor{Backend: "docker", Image: "fake"}, "docker"},
		{"pinned-bwrap", Executor{Backend: "native", Runner: RunnerBubblewrap}, "bwrap"},
		{"pinned-landlock", Executor{Backend: "native", Runner: RunnerLandlock}, "curl"},
	}
	interrupts := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, context.DeadlineExceeded},
		{"cancel", func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(300*time.Millisecond, cancel)
			return ctx, cancel
		}, context.Canceled},
	}
	for _, b := range backends {
		for _, in := range interrupts {
			t.Run(b.name+"/"+in.name, func(t *testing.T) {
				if b.executor.Runner == RunnerLandlock && landlockABI() < landlockMinABI {
					t.Skip("Landlock 不可用")
				}
				fakeCommand(t, b.command, hang)
				e := b.executor
				e.Policy = Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}

				ctx, cancel := in.ctx()
				defer cancel()
				_, err := e.Probe(ctx)
				if !errors.Is(err, in.want) || errors.Is(err, ErrUnavailable) {
					t.Fatalf("Probe 返回 %v，应当是 %v", err, in.want)
				}

				ctx, cancel = in.ctx()
				defer cancel()
				_, err = e.Run(ctx, env.Command{Argv: []string{"true"}})
				if !errors.Is(err, in.want) || errors.Is(err, ErrUnavailable) {
					t.Fatalf("Run 返回 %v，应当是 %v", err, in.want)
				}
			})
		}
	}
}

func TestProbeFailureWithLiveContextIsUnavailable(t *testing.T) {
	fakeCommand(t, "bwrap", "exit 1")
	e := Executor{Backend: "native", Runner: RunnerBubblewrap, Policy: Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("探测返回 %v，应当是 ErrUnavailable", err)
	}
}
