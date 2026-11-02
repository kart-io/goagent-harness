// Package sshsandbox 把一台 Linux SSH 主机上的目录作为沙箱实例。文件操作在实例内进行，命令由远端安装的 sandbox-runner
// 用远端的本机隔离后端（bwrap 或 Landlock）执行，策略和探测与本机 native 后端相同。
//
// 隔离只限制写入、网络和进程，不限制读取：同一台主机上的实例能读到彼此的文件。远端主机和 sandbox-runner 属于可信基础设施，
// 这里防的是沙箱内的命令，不防恶意主机。
package sshsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
	"golang.org/x/crypto/ssh"
)

const (
	defaultConnectTimeout = 15 * time.Second
	// sandbox-runner 自己先处理超时并交回已有输出，监管进程的强杀留在这之后。
	runnerKillGrace = 5 * time.Second
)

var instanceID = regexp.MustCompile(`^sbx-[0-9a-f]{24}$`)

// Config 配置远端主机。
type Config struct {
	Addr string
	User string
	Auth []ssh.AuthMethod
	// HostKeyCallback 必填，可用 ssh.FixedHostKey 或 knownhosts.New。
	HostKeyCallback ssh.HostKeyCallback
	// BaseDir 是远端存放实例的绝对路径，不能经过符号链接。
	BaseDir string
	// RunnerPath 是远端 sandbox-runner 的绝对路径，由运维安装。
	RunnerPath string
	// RunnerSHA256 是它的十六进制摘要，连接时校验。
	RunnerSHA256 string
	// ConnectTimeout 限制建立连接，默认 15 秒。
	ConnectTimeout time.Duration
}

// Provider 实现 sandbox.Provider、InstanceFinder 和 InstanceDestroyer。实例 ID 由 Scope 决定，分配是幂等可查的。
type Provider struct {
	cfg    Config
	client *client
}

// New 校验配置，不连接远端。
func New(cfg Config) (*Provider, error) {
	switch {
	case cfg.Addr == "" || cfg.User == "" || len(cfg.Auth) == 0:
		return nil, errors.New("sshsandbox: Addr、User 和 Auth 都必填")
	case cfg.HostKeyCallback == nil:
		return nil, errors.New("sshsandbox: HostKeyCallback 必填，不能跳过主机校验")
	case !validRemotePath(cfg.BaseDir) || path.Clean(cfg.BaseDir) == "/":
		return nil, fmt.Errorf("sshsandbox: BaseDir 无效: %q", cfg.BaseDir)
	case !validRemotePath(cfg.RunnerPath):
		return nil, fmt.Errorf("sshsandbox: RunnerPath 无效: %q", cfg.RunnerPath)
	}
	if digest, err := hex.DecodeString(cfg.RunnerSHA256); err != nil || len(digest) != sha256.Size {
		return nil, errors.New("sshsandbox: RunnerSHA256 必须是十六进制的 SHA-256")
	}
	cfg.BaseDir = path.Clean(cfg.BaseDir)
	cfg.RunnerSHA256 = strings.ToLower(cfg.RunnerSHA256)
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = defaultConnectTimeout
	}
	return &Provider{cfg: cfg, client: newClient(cfg)}, nil
}

func validRemotePath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p
}

// Version 包含辅助程序摘要：换了执行边界，恢复时要能发现。
func (p *Provider) Version() string { return "ssh-v1;runner-sha256=" + p.cfg.RunnerSHA256 }

// Close 断开连接。实例和运行中的按键命令不受影响。
func (p *Provider) Close() error { return p.client.close() }

// Probe 校验远端辅助程序并探测隔离能力，报告实际的 Runner 和 Enforcement。要求绝对边界的宿主应当拒绝 partial。
func (p *Provider) Probe(ctx context.Context, network bool) (sandbox.Report, error) {
	stdout, stderr, code, err := p.client.run(ctx, joinShell("sha256sum", p.cfg.RunnerPath), nil, false)
	if err != nil {
		return sandbox.Report{}, err
	}
	if fields := strings.Fields(string(stdout)); code != 0 || len(fields) == 0 || fields[0] != p.cfg.RunnerSHA256 {
		return sandbox.Report{}, fmt.Errorf("%w: sandbox-runner 摘要不符或无法读取: %s", sandbox.ErrUnavailable, stderr)
	}
	config, err := json.Marshal(runnerConfig{Workspace: p.cfg.BaseDir, Mode: sandbox.WorkspaceWrite, Network: networkOf(network)})
	if err != nil {
		return sandbox.Report{}, err
	}
	if _, _, code, err = p.client.run(ctx, joinShell("mkdir", "-p", p.cfg.BaseDir), nil, false); err != nil {
		return sandbox.Report{}, err
	}
	if code != 0 {
		return sandbox.Report{}, fmt.Errorf("%w: 无法创建 BaseDir", sandbox.ErrUnavailable)
	}
	stdout, stderr, code, err = p.client.run(ctx, joinShell(p.cfg.RunnerPath, "probe", string(config)), nil, false)
	if err != nil {
		return sandbox.Report{}, err
	}
	if code != 0 {
		return sandbox.Report{}, fmt.Errorf("%w: 远端探测失败，退出码 %d: %s", sandbox.ErrUnavailable, code, stderr)
	}
	var report sandbox.Report
	if err := json.Unmarshal(stdout, &report); err != nil {
		return sandbox.Report{}, fmt.Errorf("%w: 探测输出无效: %v", sandbox.ErrUnavailable, err)
	}
	return report, nil
}

func networkOf(network bool) sandbox.Network {
	if network {
		return sandbox.NetworkInherit
	}
	return sandbox.NetworkDeny
}

func idOf(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return "sbx-" + hex.EncodeToString(sum[:])[:24]
}

func (p *Provider) dir(id string) string { return path.Join(p.cfg.BaseDir, id) }

func (p *Provider) environment(id string, r sandbox.Request) *environment {
	return &environment{provider: p, id: id, dir: p.dir(id), network: r.Network}
}

const createScript = `set -e
mkdir -p "$(dirname "$1")"
mkdir "$1" 2>/dev/null || exit 17
mkdir "$1/workspace" "$1/tmp" "$1/state"
`

// Create 用 mkdir 原子占位。探测和连接失败都发生在占位之前，可以确认没有产生实例。
func (p *Provider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if _, err := p.Probe(ctx, r.Network); err != nil {
		return nil, notCreatedIfDefinite(err)
	}
	id := idOf(r.Scope)
	_, stderr, code, err := p.client.run(ctx, joinShell("sh", "-c", createScript, "sh", p.dir(id)), nil, false)
	if err != nil {
		return nil, fmt.Errorf("分配实例 %s: %w", id, err)
	}
	if code == 17 {
		return nil, fmt.Errorf("实例 %s 已存在", id)
	}
	if code != 0 {
		return p.environment(id, r), fmt.Errorf("分配实例 %s，退出码 %d: %s", id, code, stderr)
	}
	return p.environment(id, r), nil
}

func notCreatedIfDefinite(err error) error {
	if errors.Is(err, errDial) || errors.Is(err, sandbox.ErrUnavailable) {
		return errors.Join(sandbox.ErrNotCreated, err)
	}
	return err
}

func (p *Provider) exists(ctx context.Context, id string) (bool, error) {
	_, stderr, code, err := p.client.run(ctx, joinShell("test", "-d", p.dir(id)+"/workspace"), nil, false)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("查询实例 %s，退出码 %d: %s", id, code, stderr)
}

// Connect 连回实例。
func (p *Provider) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	if !instanceID.MatchString(id) {
		return nil, fmt.Errorf("实例 ID 无效 %q: %w", id, sandbox.ErrInstanceNotFound)
	}
	found, err := p.exists(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("实例 %s: %w", id, sandbox.ErrInstanceNotFound)
	}
	if _, err := p.Probe(ctx, r.Network); err != nil {
		return nil, err
	}
	return p.environment(id, r), nil
}

// FindByScope 查询结果是确定的：ID 由 Scope 决定，目录要么存在要么不存在。
func (p *Provider) FindByScope(ctx context.Context, r sandbox.Request) (string, bool, error) {
	id := idOf(r.Scope)
	_, stderr, code, err := p.client.run(ctx, joinShell("test", "-d", p.dir(id)), nil, false)
	if err != nil {
		return "", false, err
	}
	switch code {
	case 0:
		return id, true, nil
	case 1:
		return "", false, nil
	}
	return "", false, fmt.Errorf("查询实例 %s，退出码 %d: %s", id, code, stderr)
}

// 先杀监管进程，它不会再启动新的命令。再让 sandbox-runner 结束命令：Landlock 没有 PID 命名空间，直接强杀它会漏掉命令的进程组。
const destroyScript = `d=$1
[ -d "$d" ] || exit 0
for f in "$d"/state/*/supervisor; do
	[ -f "$f" ] && kill -KILL -"$(cat "$f")" 2>/dev/null
done
running=
for f in "$d"/state/*/pid; do
	[ -f "$f" ] || continue
	p=$(cat "$f")
	kill -TERM -"$p" 2>/dev/null && running="$running $p"
done
i=0
while [ -n "$running" ] && [ "$i" -lt 30 ]; do
	left=
	for p in $running; do kill -0 -"$p" 2>/dev/null && left="$left $p"; done
	running=$left
	[ -n "$running" ] && sleep 0.1
	i=$((i+1))
done
for p in $running; do kill -KILL -"$p" 2>/dev/null; done
chmod -R u+rwx "$d" 2>/dev/null
rm -rf "$d" || { sleep 0.2; chmod -R u+rwx "$d" 2>/dev/null; rm -rf "$d"; }
`

// DestroyInstance 删除实例并结束它的按键命令。实例已不存在也算成功。
func (p *Provider) DestroyInstance(ctx context.Context, id string, _ sandbox.Request) error {
	// 这个值会拼进 rm -rf 的路径，不能让持久化的内容决定删除范围。
	if !instanceID.MatchString(id) {
		return fmt.Errorf("实例 ID 无效 %q: %w", id, sandbox.ErrInstanceNotFound)
	}
	_, stderr, code, err := p.client.run(ctx, joinShell("sh", "-c", destroyScript, "sh", p.dir(id)), nil, false)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("删除实例 %s，退出码 %d: %s", id, code, stderr)
	}
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func joinShell(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shellQuote(a)
	}
	return strings.Join(quoted, " ")
}

type environment struct {
	provider *Provider
	id, dir  string
	network  bool
}

func (e *environment) ID() string   { return e.id }
func (e *environment) Root() string { return path.Join(e.dir, "workspace") }
func (e *environment) FileSystem() env.FileSystem {
	return &fileSystem{client: e.provider.client, root: e.Root(), lock: path.Join(e.dir, "state", ".fs.lock")}
}
func (e *environment) Executor() env.Executor { return e }

// Isolated 表示每条命令都经过远端隔离后端；实例之间仍能互相读取，见包说明。
func (e *environment) Isolated() bool { return true }

// Close 不做事：连接属于 Provider，断开也不影响实例。
func (e *environment) Close(context.Context) error { return nil }
func (e *environment) Destroy(ctx context.Context) error {
	return e.provider.DestroyInstance(ctx, e.id, sandbox.Request{})
}
func (e *environment) Capabilities() env.Capabilities {
	return env.Capabilities{Remote: true, Network: e.network}
}

func (e *environment) defaultPolicy() sandbox.Policy {
	return sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: networkOf(e.network), WorkspaceRoot: e.Root()}
}

// CheckPolicy 只接受创建时确定的网络能力。WorkspaceRoot 是宿主的授权域，不是远端路径：命令总是绑定到这个实例的工作区。
func (e *environment) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.AdditionalWritableRoots) != 0 {
		return sandbox.ErrUnsupported
	}
	if p.Network == sandbox.NetworkInherit && !e.network {
		return sandbox.ErrUnsupported
	}
	if p.Mode == sandbox.DangerFullAccess {
		return sandbox.ErrUnsupported
	}
	return nil
}

func (e *environment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	return e.RunWithPolicy(ctx, c, e.defaultPolicy())
}

// 宿主取消或断线时 stdin 收到 EOF，脚本让 sandbox-runner 终止命令；它处理不了时再强杀。
const watchedScript = `exec 3<&0
setsid "$@" 3<&- </dev/null & c=$!
{ cat <&3 >/dev/null 2>&1; kill -TERM -"$c" 2>/dev/null; sleep 2; kill -KILL -"$c" 2>/dev/null; } >/dev/null 2>&1 &
w=$!
wait "$c"; rc=$?
kill "$w" 2>/dev/null
exit "$rc"
`

// RunWithPolicy 在隔离中运行命令。
func (e *environment) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return env.Result{}, err
	}
	if len(c.Argv) == 0 {
		return env.Result{}, sandbox.ErrDenied
	}
	argv, err := e.runnerArgv(p, c, 0)
	if err != nil {
		return env.Result{}, err
	}
	cmdline := "cd " + shellQuote(e.Root()) + " && " + joinShell(append([]string{"sh", "-c", watchedScript, "sh"}, argv...)...)
	stdout, stderr, code, err := e.provider.client.run(ctx, cmdline, nil, true)
	if err != nil {
		return env.Result{Output: append(stdout, stderr...)}, err
	}
	if err := runnerError(code, stderr); err != nil {
		return env.Result{Output: stdout}, err
	}
	return env.Result{Output: append(stdout, stderr...), ExitCode: code}, nil
}

// runnerArgv 组装远端命令。TMPDIR 放在实例内，辅助程序被强杀时留下的临时目录会随实例删除。
func (e *environment) runnerArgv(p sandbox.Policy, c env.Command, timeout time.Duration) ([]string, error) {
	config, err := json.Marshal(runnerConfig{Workspace: e.Root(), Mode: p.Mode, Network: p.Network, Dir: c.Dir, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	return append([]string{"env", "TMPDIR=" + path.Join(e.dir, "tmp"), e.provider.cfg.RunnerPath, string(config), "--"}, c.Argv...), nil
}

func runnerError(code int, output []byte) error {
	if code != runnerFailedRC {
		return nil
	}
	text := string(output)
	switch {
	case strings.Contains(text, runnerDenied):
		return fmt.Errorf("%w: %s", sandbox.ErrDenied, text)
	case strings.Contains(text, runnerPrefix):
		return fmt.Errorf("%w: %s", sandbox.ErrUnavailable, text)
	}
	return nil
}

// supervisorExec 只运行监管脚本本身。脚本要写实例内的状态目录，不进沙箱；用户命令由 RunKeyed 包进 sandbox-runner。
type supervisorExec struct{ e *environment }

func (x supervisorExec) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	return x.e.CheckPolicy(ctx, p)
}

func (x supervisorExec) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	if err := x.e.CheckPolicy(ctx, p); err != nil {
		return env.Result{}, err
	}
	cmdline := "cd " + shellQuote(x.e.Root()) + " && " + joinShell(c.Argv...)
	stdout, stderr, code, err := x.e.provider.client.run(ctx, cmdline, nil, false)
	return env.Result{Output: append(stdout, stderr...), ExitCode: code}, err
}

func (e *environment) supervisor() *sandbox.CommandSupervisor {
	return &sandbox.CommandSupervisor{Exec: supervisorExec{e}, StateDir: path.Join(e.dir, "state")}
}

// RunKeyed 让命令脱离 SSH 连接继续运行，断线或宿主重启后可以取回结果。
func (e *environment) RunKeyed(ctx context.Context, c sandbox.KeyedCommand, p sandbox.Policy) (sandbox.KeyedResult, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return sandbox.KeyedResult{}, err
	}
	argv, err := e.runnerArgv(p, c.Command, c.Timeout)
	if err != nil {
		return sandbox.KeyedResult{}, err
	}
	supervised := c
	supervised.Command = env.Command{Argv: argv}
	supervised.Timeout = c.Timeout + runnerKillGrace
	res, err := e.supervisor().RunKeyed(ctx, supervised, p)
	if err != nil {
		return res, err
	}
	if err := runnerError(res.ExitCode, res.Output); err != nil {
		return res, err
	}
	if res.ExitCode == runnerTimedOutRC && strings.HasSuffix(string(res.Output), runnerTimedOut) {
		res.TimedOut = true
		res.Output = []byte(strings.TrimSuffix(string(res.Output), runnerTimedOut))
	}
	return res, nil
}

// 先让 sandbox-runner 终止命令和它的进程组，等不到再由监管器强杀。
const gracefulCancelScript = `d=$1
[ -f "$d/pid" ] || exit 0
c=$(cat "$d/pid")
kill -TERM -"$c" 2>/dev/null
i=0
while [ ! -f "$d/exit" ] && [ "$i" -lt 30 ]; do sleep 0.1; i=$((i+1)); done
exit 0
`

// CancelKeyed 终止命令。
func (e *environment) CancelKeyed(ctx context.Context, key string, p sandbox.Policy) (bool, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return false, err
	}
	supervisor := e.supervisor()
	if _, err := (supervisorExec{e}).RunWithPolicy(ctx, env.Command{Argv: []string{"sh", "-c", gracefulCancelScript, "sh", supervisor.KeyDir(key)}}, p); err != nil {
		return false, err
	}
	return supervisor.CancelKeyed(ctx, key, p)
}
