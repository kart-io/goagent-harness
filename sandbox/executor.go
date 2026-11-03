package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/kart-io/goagent/harness/env"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Linux 上 native 后端的隔离实现。
const (
	RunnerBubblewrap = "bwrap"
	RunnerLandlock   = "landlock"
)

type Executor struct {
	Policy  Policy
	Backend string
	Image   string
	// Runner 固定 Linux native 的隔离实现。留空时优先 bwrap，探测不通过再用 Landlock。
	Runner       string
	EnvAllowlist []string
}
type Report struct {
	Backend string
	// Runner 是实际使用的隔离实现。
	Runner string
	// Enforcement 为 partial 时见 Runner 对应的限制说明，要求绝对边界的调用方应当拒绝。
	Enforcement   string
	Network       Network
	WritableRoots []string
}

func (e *Executor) Capabilities() env.Capabilities {
	return env.Capabilities{Container: e.Backend == "docker", Network: e.Policy.Network == NetworkInherit, ReadOnlyRoot: e.Policy.Mode == ReadOnly}
}
func (e *Executor) Run(ctx context.Context, c env.Command) (env.Result, error) {
	p, err := effective(ctx, e.Policy)
	if err != nil {
		return env.Result{}, err
	}
	if len(c.Argv) == 0 {
		return env.Result{}, ErrDenied
	}
	if filepath.IsAbs(c.Dir) {
		return env.Result{}, ErrDenied
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(p.WorkspaceRoot, c.Dir))
	if err != nil || !contains(p.WorkspaceRoot, dir) {
		return env.Result{}, ErrDenied
	}
	report, err := e.cachedProbe(ctx, p)
	if err != nil {
		return env.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return env.Result{}, err
	}
	// 只用探测通过的实现执行。
	verified := *e
	verified.Runner = report.Runner
	return verified.raw(ctx, p, c)
}

// Probe 总是完整探测，成功时刷新 Run 使用的缓存。
func (e *Executor) Probe(ctx context.Context) (Report, error) {
	p, err := effective(ctx, e.Policy)
	if err != nil {
		return Report{}, err
	}
	report, err := e.probe(ctx, p)
	if err == nil {
		probeCache.Store(e.probeKey(p), report.Runner)
	}
	return report, err
}

// probeKey 是探测结果依赖的全部输入。探测只用临时目录，结果与工作区路径无关；
// 后端和 bwrap、curl 等工具按 PATH 查找，所以 PATH 也在其中。
type probeKey struct {
	backend, image, runner, path string
	mode                         Mode
	network                      Network
}

func (e *Executor) probeKey(p Policy) probeKey {
	return probeKey{e.Backend, e.Image, e.Runner, os.Getenv("PATH"), p.Mode, p.Network}
}

// probeCache 记录探测通过的组合与选中的实现。探测要起多次沙箱，Docker 后端每次都要创建容器，
// 每条命令都探测会让一条命令多花数十秒；失败不缓存，下次 Run 重新探测。
var probeCache sync.Map // probeKey -> runner

// cachedProbe 返回探测通过时选中的实现；Run 只用到 Runner。
func (e *Executor) cachedProbe(ctx context.Context, p Policy) (Report, error) {
	key := e.probeKey(p)
	if runner, ok := probeCache.Load(key); ok {
		return Report{Backend: e.Backend, Runner: runner.(string)}, nil
	}
	report, err := e.probe(ctx, p)
	if err == nil {
		probeCache.Store(key, report.Runner)
	}
	return report, err
}

// probe 选出第一个通过完整探测的实现。探测包含越界写入和网络的反向验证，所以 bwrap 能启动但兑现不了策略时也不会被选中。
func (e *Executor) probe(ctx context.Context, p Policy) (Report, error) {
	if e.Backend != "native" || runtime.GOOS != "linux" || e.Runner != "" {
		report, err := e.probeRunner(ctx, p)
		if err != nil && ctx.Err() != nil {
			return report, ctx.Err()
		}
		return report, err
	}
	var failures error
	for _, runner := range []string{RunnerBubblewrap, RunnerLandlock} {
		candidate := *e
		candidate.Runner = runner
		report, err := candidate.probeRunner(ctx, p)
		if err == nil {
			return report, nil
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		if !errors.Is(err, ErrUnavailable) {
			return report, err
		}
		failures = errors.Join(failures, fmt.Errorf("%s: %w", runner, err))
	}
	return Report{Backend: e.Backend, Enforcement: "unavailable", Network: p.Network}, failures
}

func (e *Executor) probeRunner(ctx context.Context, p Policy) (Report, error) {
	r := Report{Backend: e.Backend, Runner: e.Runner, Enforcement: "unavailable", Network: p.Network}
	if e.Runner != "" && (e.Backend != "native" || runtime.GOOS != "linux" || (e.Runner != RunnerBubblewrap && e.Runner != RunnerLandlock)) {
		return r, fmt.Errorf("%w: runner %q on %s/%s", ErrUnavailable, e.Runner, e.Backend, runtime.GOOS)
	}
	if e.Runner == RunnerLandlock && landlockABI() < landlockMinABI {
		return r, fmt.Errorf("%w: landlock ABI %d is below %d", ErrUnavailable, landlockABI(), landlockMinABI)
	}
	if e.Backend == "local" {
		if p.Mode != DangerFullAccess || p.Network != NetworkInherit {
			return r, ErrDenied
		}
		r.Enforcement = "none"
		r.WritableRoots = []string{"/"}
		return r, nil
	}
	if e.Backend != "native" && e.Backend != "docker" {
		return r, ErrUnavailable
	}
	if e.Backend == "native" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return r, ErrUnavailable
	}
	// Docker daemons commonly share the user home but not native OS temp paths.
	probeBase := ""
	if e.Backend == "docker" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return r, err
		}
		probeBase = filepath.Join(cache, "goagent-harness", "probes")
		if err := os.MkdirAll(probeBase, 0700); err != nil {
			return r, err
		}
	}
	// Probes use disposable files exclusively, never business paths.
	root, err := os.MkdirTemp(probeBase, "harness-probe-")
	if err != nil {
		return r, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	root, _ = filepath.EvalSymlinks(root)
	outside, err := os.MkdirTemp(probeBase, "harness-outside-")
	if err != nil {
		return r, err
	}
	defer func() { _ = os.RemoveAll(outside) }()
	outside, _ = filepath.EvalSymlinks(outside)
	q := p
	q.WorkspaceRoot = root
	q.AdditionalWritableRoots = nil
	res, err := e.raw(ctx, q, env.Command{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil || res.ExitCode != 0 {
		return r, fmt.Errorf("%w: backend startup: %v %s", ErrUnavailable, err, res.Output)
	}
	if p.Mode != DangerFullAccess && e.Backend != "docker" {
		res, err = e.raw(ctx, q, env.Command{Argv: []string{"/bin/sh", "-c", ": > " + shellQuote(filepath.Join(outside, "blocked"))}})
		if err != nil || res.ExitCode == 0 {
			return r, fmt.Errorf("%w: outside-write probe", ErrUnavailable)
		}
	}
	insideRoot := root
	if e.Backend == "docker" {
		insideRoot = "/workspace"
		marker := filepath.Join(outside, "host-marker")
		if err := os.WriteFile(marker, []byte("probe"), 0600); err != nil {
			return r, err
		}
		res, err = e.raw(ctx, q, env.Command{Argv: []string{"/bin/sh", "-c", "test ! -e " + shellQuote(marker)}})
		if err != nil || res.ExitCode != 0 {
			return r, fmt.Errorf("%w: unmounted host probe", ErrUnavailable)
		}
	}
	res, err = e.raw(ctx, q, env.Command{Argv: []string{"/bin/sh", "-c", ": > " + shellQuote(filepath.Join(insideRoot, "inside"))}})
	if err != nil || (p.Mode == ReadOnly && res.ExitCode == 0) || (p.Mode != ReadOnly && res.ExitCode != 0) {
		return r, fmt.Errorf("%w: workspace-write probe: %s", ErrUnavailable, res.Output)
	}
	if p.Network == NetworkDeny {
		if e.Backend == "docker" {
			res, err = e.raw(ctx, q, env.Command{Argv: []string{"/bin/sh", "-c", "test \"$(ls /sys/class/net)\" = lo"}})
			if err != nil || res.ExitCode != 0 {
				return r, fmt.Errorf("%w: network namespace probe", ErrUnavailable)
			}
		} else {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return r, err
			}
			stop := startProbeHTTP(listener)
			defer stop()
			curl, err := exec.LookPath("curl")
			if err != nil {
				return r, fmt.Errorf("%w: curl probe unavailable", ErrUnavailable)
			}
			positive := exec.CommandContext(ctx, curl, "--noproxy", "*", "--connect-timeout", "1", "--max-time", "2", "http://"+listener.Addr().String())
			positive.Env = e.environment()
			if err := positive.Run(); err != nil {
				return r, fmt.Errorf("%w: network positive control: %v", ErrUnavailable, err)
			}
			res, err = e.raw(ctx, q, env.Command{Argv: []string{curl, "--noproxy", "*", "--connect-timeout", "1", "--max-time", "2", "http://" + listener.Addr().String()}})
			if err != nil || res.ExitCode == 0 {
				return r, fmt.Errorf("%w: network-deny probe: exit %d: %v: %s", ErrUnavailable, res.ExitCode, err, res.Output)
			}
		}
	}
	if e.Backend == "native" && (p.Mode != DangerFullAccess || p.Network == NetworkDeny) {
		if err := e.probeUnixSocket(ctx, q); err != nil {
			return r, err
		}
	}
	r.Enforcement = "full"
	if e.Runner == RunnerLandlock && landlockABI() < landlockFullABI {
		r.Enforcement = "partial"
	}
	switch p.Mode {
	case DangerFullAccess:
		r.WritableRoots = []string{"/"}
	case WorkspaceWrite:
		r.WritableRoots = append([]string{p.WorkspaceRoot}, p.AdditionalWritableRoots...)
	}
	if e.Backend == "native" && p.Mode == WorkspaceWrite {
		r.WritableRoots = append(r.WritableRoots, "per-command private temporary directory (TMPDIR)")
	}
	if e.Backend == "docker" {
		r.WritableRoots = append(r.WritableRoots, "container writable layer")
	}
	return r, nil
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (e *Executor) environment() []string {
	names := append([]string{"PATH", "LANG", "LC_ALL", "TZ"}, e.EnvAllowlist...)
	out := []string{}
	for _, n := range names {
		if strings.ContainsAny(n, "=\x00") {
			continue
		}
		if v, ok := os.LookupEnv(n); ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// mountRaceAttempts 限制 raw 因宿主挂载变化重新启动沙箱的次数。
const mountRaceAttempts = 5

// raw 运行一条命令。bwrap 绑定 / 时逐个重挂宿主的挂载点，其他进程（如 containerd 的临时挂载）恰好卸载时启动失败。
// 这时命令还没开始执行（启动标记未出现），可以安全地重新启动。
func (e *Executor) raw(ctx context.Context, p Policy, c env.Command) (env.Result, error) {
	for attempt := 1; ; attempt++ {
		res, err := e.runOnce(ctx, p, c)
		if !errors.Is(err, ErrUnavailable) || !bytes.Contains(res.Output, []byte("Unable to apply mount flags")) || attempt == mountRaceAttempts {
			return res, err
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		}
	}
}

func (e *Executor) runOnce(ctx context.Context, p Policy, c env.Command) (env.Result, error) {
	if err := ctx.Err(); err != nil {
		return env.Result{}, err
	}
	argv := append([]string(nil), c.Argv...)
	var startMarker []byte
	if e.Backend == "native" || e.Backend == "docker" {
		var err error
		argv, startMarker, err = commandStart(argv)
		if err != nil {
			return env.Result{}, err
		}
	}
	dir := filepath.Join(p.WorkspaceRoot, c.Dir)
	var seccomp *os.File
	landlock := false
	commandEnv := e.environment()
	privateTemp := ""
	if e.Backend == "native" && p.Mode == WorkspaceWrite {
		var err error
		privateTemp, err = os.MkdirTemp("", "harness-command-")
		if err != nil {
			return env.Result{}, err
		}
		defer func() { _ = os.RemoveAll(privateTemp) }()
		privateTemp, err = filepath.EvalSymlinks(privateTemp)
		if err != nil {
			return env.Result{}, err
		}
		for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
			commandEnv = replaceEnv(commandEnv, name, privateTemp)
		}
	}
	switch e.Backend {
	case "local":
		if p.Mode != DangerFullAccess || p.Network != NetworkInherit {
			return env.Result{}, ErrDenied
		}
	case "native":
		switch runtime.GOOS {
		case "darwin":
			profile := "(version 1)(allow default)"
			if p.Mode != DangerFullAccess {
				profile += "(deny file-write*)(allow file-write-data (literal \"/dev/null\"))"
				if p.Mode == WorkspaceWrite {
					profile += "(allow file-write* (subpath " + strconv.Quote(privateTemp) + "))"
					for _, r := range append([]string{p.WorkspaceRoot}, p.AdditionalWritableRoots...) {
						profile += "(allow file-write* (subpath " + strconv.Quote(r) + "))"
					}
				}
			}
			if p.Mode != DangerFullAccess {
				profile += "(deny network-outbound (remote unix-socket))"
			}
			if p.Network == NetworkDeny {
				profile += "(deny network*)"
			}
			argv = append([]string{"/usr/bin/sandbox-exec", "-p", profile}, argv...)
		case "linux":
			if e.Runner == RunnerLandlock {
				landlock = true
				break
			}
			var filterErr error
			seccomp, filterErr = socketFilter(p)
			if filterErr != nil {
				return env.Result{}, filterErr
			}
			if seccomp != nil {
				defer func() { _ = seccomp.Close() }()
			}
			args := []string{"bwrap", "--die-with-parent", "--new-session", "--unshare-pid", "--ro-bind", "/", "/", "--proc", "/proc", "--ro-bind", "/dev", "/dev", "--dev-bind", "/dev/null", "/dev/null"}
			switch p.Mode {
			case DangerFullAccess:
				args = []string{"bwrap", "--die-with-parent", "--new-session", "--unshare-pid", "--bind", "/", "/", "--proc", "/proc", "--dev", "/dev"}
			case WorkspaceWrite:
				args = append(args, "--tmpfs", privateTemp)
				for _, r := range append([]string{p.WorkspaceRoot}, p.AdditionalWritableRoots...) {
					args = append(args, "--bind", r, r)
				}
			}
			if p.Network == NetworkDeny {
				args = append(args, "--unshare-net")
			}
			if seccomp != nil {
				args = append(args, "--seccomp", "3")
			}
			argv = append(append(args, "--"), argv...)
		default:
			return env.Result{}, ErrUnavailable
		}
	case "docker":
		if strings.Contains(p.WorkspaceRoot, ",") {
			return env.Result{}, ErrDenied
		}
		if e.Image == "" {
			return env.Result{}, fmt.Errorf("%w: image required", ErrUnavailable)
		}
		name := fmt.Sprintf("harness-sandbox-%d-%d", os.Getpid(), time.Now().UnixNano())
		profilePath, removeProfile, profileErr := writeDockerProfile(p)
		if profileErr != nil {
			return env.Result{}, profileErr
		}
		defer removeProfile()
		args := []string{"--rm", "--security-opt", "seccomp=" + profilePath, "--name", name, "--network"}
		if p.Network == NetworkDeny {
			args = append(args, "none")
		} else {
			args = append(args, "bridge")
		}
		mode := "rw"
		if p.Mode == ReadOnly {
			mode = "ro"
		}
		args = append(args, "--mount", "type=bind,src="+p.WorkspaceRoot+",dst=/workspace,"+map[string]string{"ro": "readonly", "rw": "readonly=false"}[mode], "-w", filepath.ToSlash(filepath.Join("/workspace", c.Dir)), "-e", "HOME=/tmp", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
		for _, r := range p.AdditionalWritableRoots {
			if strings.Contains(r, ",") {
				return env.Result{}, ErrDenied
			}
			mount := "type=bind,src=" + r + ",dst=" + r
			if p.Mode == ReadOnly {
				mount += ",readonly"
			}
			args = append(args, "--mount", mount)
		}
		for _, v := range e.containerEnvironment() {
			args = append(args, "-e", v)
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
		}()
		if out, err := createDockerContainer(ctx, e.Image, append(append(args, e.Image), argv...)); err != nil {
			if ctx.Err() != nil {
				return env.Result{}, ctx.Err()
			}
			return env.Result{Output: out}, fmt.Errorf("%w: docker runner did not start command: create: %v: %s", ErrUnavailable, err, out)
		}
		argv = []string{"docker", "start", "-a", name}
	default:
		return env.Result{}, ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if seccomp != nil {
		cmd.ExtraFiles = []*os.File{seccomp}
	}
	cmd.Dir = dir
	cmd.Env = commandEnv
	cmd.WaitDelay = time.Second
	isolate(cmd)
	defer func() { _ = kill(cmd) }()
	out := &boundedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	var err error
	if landlock {
		if err = startLandlock(cmd, p, privateTemp); err == nil {
			err = cmd.Wait()
		}
	} else {
		err = cmd.Run()
	}
	res := env.Result{Output: out.bytes()}
	started := true
	if startMarker != nil {
		res.Output, started = removeCommandStart(res.Output, startMarker)
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if !started {
		return res, fmt.Errorf("%w: %s runner did not start command: %v: %s", ErrUnavailable, e.Backend, err, res.Output)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res, nil
	}
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
		return res, nil
	}
	return res, err
}

// dockerCreateTimeout 限制不随 ctx 取消的 docker create。镜像已在本地，创建通常一两秒内完成。
const dockerCreateTimeout = time.Minute

// createDockerContainer 创建容器但不启动。docker run 被中断时守护进程可能仍在创建容器，之后的 rm 删不到它，
// 所以创建不随 ctx 取消，等容器确实存在后再删除。镜像不在本地时先拉取，拉取可以随 ctx 取消。
func createDockerContainer(ctx context.Context, image string, args []string) ([]byte, error) {
	for pulled := false; ; pulled = true {
		createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), dockerCreateTimeout)
		create := exec.CommandContext(createCtx, "docker", append([]string{"create", "--pull", "never"}, args...)...)
		create.WaitDelay = time.Second
		out, err := create.CombinedOutput()
		cancel()
		if err == nil || ctx.Err() != nil || pulled {
			return out, err
		}
		inspect := exec.CommandContext(ctx, "docker", "image", "inspect", image)
		inspect.WaitDelay = time.Second
		if inspect.Run() == nil {
			return out, err
		}
		pull := exec.CommandContext(ctx, "docker", "pull", "-q", image)
		pull.WaitDelay = time.Second
		if pullOut, pullErr := pull.CombinedOutput(); pullErr != nil {
			return append(out, pullOut...), pullErr
		}
	}
}

type boundedBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	remaining := (4 << 20) - len(b.b)
	if len(p) > remaining {
		p = p[:remaining]
	}
	b.b = append(b.b, p...)
	return n, nil
}
func (b *boundedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b...)
}

func replaceEnv(entries []string, name, value string) []string {
	out := make([]string, 0, len(entries)+1)
	for _, entry := range entries {
		if !strings.HasPrefix(entry, name+"=") {
			out = append(out, entry)
		}
	}
	return append(out, name+"="+value)
}
func (e *Executor) containerEnvironment() []string {
	var out []string
	for _, name := range e.EnvAllowlist {
		if strings.ContainsAny(name, "=\x00") {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			out = replaceEnv(out, name, value)
		}
	}
	return out
}

func (e *Executor) probeUnixSocket(ctx context.Context, p Policy) error {
	root, err := os.MkdirTemp("/tmp", "harness-unix-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	socket := filepath.Join(root, "probe.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	stop := startProbeHTTP(listener)
	defer stop()
	curl, err := exec.LookPath("curl")
	if err != nil {
		return fmt.Errorf("%w: Unix socket curl probe unavailable", ErrUnavailable)
	}
	args := []string{"--noproxy", "*", "--unix-socket", socket, "--max-time", "2", "http://localhost/"}
	positive := exec.CommandContext(ctx, curl, args...)
	positive.Env = e.environment()
	if output, err := positive.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: Unix socket positive control: %v: %s", ErrUnavailable, err, output)
	}
	res, err := e.raw(ctx, p, env.Command{Argv: append([]string{curl}, args...)})
	if err != nil || res.ExitCode == 0 {
		return fmt.Errorf("%w: Unix socket filter probe", ErrUnavailable)
	}
	return nil
}
