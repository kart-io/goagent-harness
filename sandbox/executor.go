package sandbox

import (
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

type Executor struct {
	Policy       Policy
	Backend      string
	Image        string
	EnvAllowlist []string
}
type Report struct {
	Backend       string
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
	if _, err = e.probe(ctx, p); err != nil {
		return env.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return env.Result{}, err
	}
	return e.raw(ctx, p, c)
}
func (e *Executor) Probe(ctx context.Context) (Report, error) {
	p, err := effective(ctx, e.Policy)
	if err != nil {
		return Report{}, err
	}
	return e.probe(ctx, p)
}
func (e *Executor) probe(ctx context.Context, p Policy) (Report, error) {
	r := Report{Backend: e.Backend, Enforcement: "unavailable", Network: p.Network}
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
				return r, fmt.Errorf("%w: network-deny probe", ErrUnavailable)
			}
		}
	}
	if e.Backend == "native" && (p.Mode != DangerFullAccess || p.Network == NetworkDeny) {
		if err := e.probeUnixSocket(ctx, q); err != nil {
			return r, err
		}
	}
	r.Enforcement = "full"
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
func (e *Executor) raw(ctx context.Context, p Policy, c env.Command) (env.Result, error) {
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
	var cleanup func()
	var seccomp *os.File
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
		args := []string{"docker", "run", "--rm", "--security-opt", "seccomp=" + profilePath, "--name", name, "--network"}
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
		argv = append(append(args, e.Image), argv...)
		cleanup = func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = exec.CommandContext(c, "docker", "rm", "-f", name).Run()
		}
	default:
		return env.Result{}, ErrUnavailable
	}
	if cleanup != nil {
		defer cleanup()
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
	err := cmd.Run()
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
