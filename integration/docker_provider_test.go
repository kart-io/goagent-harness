package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/sandboxtest"
	"github.com/kart-io/goagent/harness/env"
	"github.com/stretchr/testify/require"
)

const (
	dockerTestsEnv  = "GOAGENT_DOCKER_TESTS"
	dockerTestImage = "alpine:3.20"
	dockerWorkspace = "/workspace"
	dockerLabelRun  = "goagent.test.run"
	dockerCrashExit = 94
)

func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv(dockerTestsEnv) == "" {
		t.Skipf("未设置 %s，跳过 Docker 测试", dockerTestsEnv)
	}
	require.NoError(t, exec.Command("docker", "version").Run(), "需要可用的 Docker")
}

// dockerProvider 是真实的 Provider：每个实例是一个容器，文件和命令都在容器里。容器名由 Scope 决定，同一 Scope 重复分配会被 Docker 拒绝。
type dockerProvider struct {
	run string
	// crash 非空时在分配时真实退出进程：before 在分配前，after 在分配后、返回前。
	crash string
	// plainExec 不提供按键执行，代表没有这项能力的后端。
	plainExec bool
}

func (p *dockerProvider) Version() string { return "docker-test-v1" }

func scopeHash(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])[:24]
}

func dockerRun(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (p *dockerProvider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	if p.crash == "before" {
		os.Exit(dockerCrashExit)
	}
	network := "none"
	if r.Network {
		network = "bridge"
	}
	hash := scopeHash(r.Scope)
	id, err := dockerRun(ctx, "run", "-d", "--name", "goagent-sbx-"+hash, "--label", dockerLabelRun+"="+p.run, "--label", "goagent.scope="+hash,
		"--network", network, "--workdir", dockerWorkspace, r.Image, "tail", "-f", "/dev/null")
	if err != nil {
		return nil, err
	}
	if p.crash == "after" {
		os.Exit(dockerCrashExit)
	}
	environment := &dockerEnvironment{id: id, network: r.Network, plainExec: p.plainExec}
	if err := waitDockerReady(ctx, id); err != nil {
		// 容器已经存在，连同错误一起返回，调用方才能负责清理。
		return environment, err
	}
	return environment, nil
}

// waitDockerReady 等容器能响应 exec 再交出去。容器刚启动时 docker exec 偶尔卸住（Docker 29.9.2，纯 shell 可复现）；
// 空操作可以放心重试，交出去之后的命令有副作用，不能重试。
func waitDockerReady(ctx context.Context, id string) error {
	var err error
	for range 6 {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err = dockerRun(probe, "exec", id, "true")
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
	}
	return err
}

func (p *dockerProvider) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	running, err := dockerRun(ctx, "inspect", "-f", "{{.State.Running}}", id)
	if err != nil {
		if dockerMissing(err) {
			return nil, fmt.Errorf("容器 %s: %w", id, sandbox.ErrInstanceNotFound)
		}
		return nil, err
	}
	if running != "true" {
		return nil, fmt.Errorf("容器 %s 没有运行", id)
	}
	return &dockerEnvironment{id: id, network: r.Network, plainExec: p.plainExec}, nil
}

// DestroyInstance 不需要容器在运行：已停止的容器同样要能删除。
func (p *dockerProvider) DestroyInstance(ctx context.Context, id string, _ sandbox.Request) error {
	_, err := dockerRun(ctx, "rm", "-f", id)
	if err != nil && dockerMissing(err) {
		return sandbox.ErrInstanceNotFound
	}
	return err
}

// dockerMissing 不区分大小写：各版本 Docker 的报错写法不同。
func dockerMissing(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such object") || strings.Contains(message, "no such container")
}

// findingDockerProvider 另外支持按 Scope 查询。
type findingDockerProvider struct{ *dockerProvider }

func (p findingDockerProvider) FindByScope(ctx context.Context, r sandbox.Request) (string, bool, error) {
	out, err := dockerRun(ctx, "ps", "-aq", "--no-trunc", "--filter", "label=goagent.scope="+scopeHash(r.Scope))
	if err != nil || out == "" {
		return "", false, err
	}
	return out, true, nil
}

type dockerEnvironment struct {
	id        string
	network   bool
	plainExec bool
}

// plainDockerExecutor 只暴露普通执行。
type plainDockerExecutor struct{ e *dockerEnvironment }

func (x plainDockerExecutor) Capabilities() env.Capabilities { return x.e.Capabilities() }
func (x plainDockerExecutor) Run(ctx context.Context, c env.Command) (env.Result, error) {
	return x.e.Run(ctx, c)
}
func (x plainDockerExecutor) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	return x.e.CheckPolicy(ctx, p)
}
func (x plainDockerExecutor) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	return x.e.RunWithPolicy(ctx, c, p)
}

func (e *dockerEnvironment) supervisor() *sandbox.CommandSupervisor {
	return &sandbox.CommandSupervisor{Exec: e, RequestTimeout: 5 * time.Second}
}

func (e *dockerEnvironment) RunKeyed(ctx context.Context, c sandbox.KeyedCommand, p sandbox.Policy) (sandbox.KeyedResult, error) {
	return e.supervisor().RunKeyed(ctx, c, p)
}

func (e *dockerEnvironment) CancelKeyed(ctx context.Context, key string, p sandbox.Policy) (bool, error) {
	return e.supervisor().CancelKeyed(ctx, key, p)
}

func (e *dockerEnvironment) ID() string                 { return e.id }
func (e *dockerEnvironment) Root() string               { return dockerWorkspace }
func (e *dockerEnvironment) FileSystem() env.FileSystem { return dockerFS{e} }
func (e *dockerEnvironment) Executor() env.Executor {
	if e.plainExec {
		return plainDockerExecutor{e}
	}
	return e
}
func (e *dockerEnvironment) Isolated() bool              { return true }
func (e *dockerEnvironment) Close(context.Context) error { return nil }
func (e *dockerEnvironment) Destroy(ctx context.Context) error {
	_, err := dockerRun(ctx, "rm", "-f", e.id)
	if err != nil && dockerMissing(err) {
		return nil
	}
	return err
}
func (e *dockerEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Remote: true, Container: true, Network: e.network}
}

func (e *dockerEnvironment) exec(ctx context.Context, dir string, stdin []byte, argv ...string) (stdout, stderr []byte, code int, err error) {
	args := append([]string{"exec", "-i", "-w", path.Join(dockerWorkspace, dir), e.id}, argv...)
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(stdin), &out, &errOut
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return out.Bytes(), errOut.Bytes(), -1, ctx.Err()
	}
	var exited *exec.ExitError
	if errors.As(runErr, &exited) {
		return out.Bytes(), errOut.Bytes(), exited.ExitCode(), nil
	}
	return out.Bytes(), errOut.Bytes(), 0, runErr
}

func (e *dockerEnvironment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	stdout, stderr, code, err := e.exec(ctx, c.Dir, nil, c.Argv...)
	return env.Result{Output: append(stdout, stderr...), ExitCode: code}, err
}

func (e *dockerEnvironment) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.AdditionalWritableRoots) != 0 || (p.Network == sandbox.NetworkInherit) != e.network {
		return sandbox.ErrUnsupported
	}
	return nil
}

func (e *dockerEnvironment) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return env.Result{}, err
	}
	if p.Mode == sandbox.ReadOnly {
		return env.Result{}, sandbox.ErrDenied
	}
	return e.Run(ctx, c)
}

// instanceExec 在实例内运行命令，stdout 和 stderr 须逐字节保留。
type instanceExec interface {
	exec(ctx context.Context, dir string, stdin []byte, argv ...string) (stdout, stderr []byte, code int, err error)
}

// dockerFS 用实例内的命令实现文件操作，条件写入由实例内的 flock 协调。OpenSandbox Provider 也用它。
type dockerFS struct{ e instanceExec }

func (f dockerFS) Resolve(p string) (string, error) {
	return f.ResolveContext(context.Background(), p)
}

func (f dockerFS) ResolveContext(ctx context.Context, p string) (string, error) {
	if strings.HasPrefix(p, dockerWorkspace) {
		p = strings.TrimPrefix(strings.TrimPrefix(p, dockerWorkspace), "/")
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if clean == "" {
		clean = "."
	}
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", env.ErrOutsideWorkspace
	}
	// 符号链接指向工作区外时拒绝；目标不存在时 readlink 没有输出，按词法结果处理。
	stdout, _, _, err := f.e.exec(ctx, "", nil, "readlink", "-f", path.Join(dockerWorkspace, clean))
	if err != nil {
		return "", err
	}
	if real := strings.TrimSpace(string(stdout)); real != "" && real != dockerWorkspace && !strings.HasPrefix(real, dockerWorkspace+"/") {
		return "", env.ErrOutsideWorkspace
	}
	return clean, nil
}

func (f dockerFS) abs(p string) string { return path.Join(dockerWorkspace, p) }

func notFound(stderr []byte) bool { return bytes.Contains(stderr, []byte("No such file")) }

func parseStat(line string) (env.Info, error) {
	fields := strings.Split(line, "|")
	if len(fields) != 4 {
		return env.Info{}, fmt.Errorf("无效的 stat 输出: %q", line)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return env.Info{}, err
	}
	raw, err := strconv.ParseUint(fields[2], 16, 32)
	if err != nil {
		return env.Info{}, err
	}
	modified, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return env.Info{}, err
	}
	mode := fs.FileMode(raw & 0o777)
	if raw&0o170000 == 0o040000 {
		mode |= fs.ModeDir
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(fields[0], dockerWorkspace), "/")
	if rel == "" {
		rel = "."
	}
	return env.Info{Path: rel, Size: size, Mode: mode, ModTime: time.Unix(modified, 0)}, nil
}

func (f dockerFS) Stat(ctx context.Context, p string) (env.Info, error) {
	stdout, stderr, code, err := f.e.exec(ctx, "", nil, "stat", "-c", "%n|%s|%f|%Y", f.abs(p))
	if err != nil {
		return env.Info{}, err
	}
	if code != 0 {
		if notFound(stderr) {
			return env.Info{}, fs.ErrNotExist
		}
		return env.Info{}, fmt.Errorf("stat: %s", stderr)
	}
	return parseStat(strings.TrimSpace(string(stdout)))
}

func dockerVersion(data []byte) env.Version {
	sum := sha256.Sum256(data)
	return env.Version(hex.EncodeToString(sum[:]))
}

func (f dockerFS) ReadFile(ctx context.Context, p string) ([]byte, env.Version, error) {
	stdout, stderr, code, err := f.e.exec(ctx, "", nil, "cat", f.abs(p))
	if err != nil {
		return nil, "", err
	}
	if code != 0 {
		if notFound(stderr) {
			return nil, "", fs.ErrNotExist
		}
		return nil, "", fmt.Errorf("read: %s", stderr)
	}
	return stdout, dockerVersion(stdout), nil
}

const dockerWriteScript = `set -e
mkdir -p "$(dirname "$1")"
tmp=$(mktemp "$1.XXXXXX")
cat > "$tmp"
exec 9>/tmp/.goagent-fs.lock
flock 9
if [ "$2" = 1 ] && [ -e "$1" ]; then rm -f "$tmp"; exit 17; fi
if [ -n "$3" ]; then
  current=$(sha256sum "$1" 2>/dev/null | cut -d' ' -f1)
  if [ "$current" != "$3" ]; then rm -f "$tmp"; exit 18; fi
fi
if [ -n "$4" ]; then chmod "$4" "$tmp"; elif [ -e "$1" ]; then chmod "$(stat -c %a "$1")" "$tmp"; else chmod 644 "$tmp"; fi
mv "$tmp" "$1"`

func (f dockerFS) WriteFile(ctx context.Context, p string, data []byte, intent env.WriteIntent) (env.Version, error) {
	create, perm := "0", ""
	if intent.CreateIfAbsent {
		create = "1"
	}
	if intent.Perm != 0 {
		perm = strconv.FormatUint(uint64(intent.Perm.Perm()), 8)
	}
	_, stderr, code, err := f.e.exec(ctx, "", data, "sh", "-c", dockerWriteScript, "write", f.abs(p), create, string(intent.IfVersion), perm)
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
		return dockerVersion(data), nil
	case 17:
		return "", env.ErrExists
	case 18:
		return "", env.ErrStale
	}
	return "", fmt.Errorf("write: %s", stderr)
}

func (f dockerFS) Remove(ctx context.Context, p string) error {
	_, stderr, code, err := f.e.exec(ctx, "", nil, "rm", "-r", f.abs(p))
	if err != nil {
		return err
	}
	if code != 0 {
		if notFound(stderr) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("remove: %s", stderr)
	}
	return nil
}

func (f dockerFS) Walk(ctx context.Context, p string, fn func(env.Info) error) error {
	stdout, stderr, code, err := f.e.exec(ctx, "", nil, "find", f.abs(p), "-exec", "stat", "-c", "%n|%s|%f|%Y", "{}", "+")
	if err != nil {
		return err
	}
	if code != 0 {
		if notFound(stderr) {
			return fs.ErrNotExist
		}
		return fmt.Errorf("walk: %s", stderr)
	}
	var infos []env.Info
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		info, err := parseStat(line)
		if err != nil {
			return err
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Path < infos[j].Path })
	skipped := ""
	for _, info := range infos {
		if skipped != "" && strings.HasPrefix(info.Path, skipped+"/") {
			continue
		}
		switch err := fn(info); {
		case errors.Is(err, fs.SkipAll):
			return nil
		case errors.Is(err, fs.SkipDir):
			skipped = info.Path
		case err != nil:
			return err
		}
	}
	return nil
}

// 这个 Provider 也要满足文件系统契约，否则后面的结论没有意义。
// dockerAdmin 让契约能制造已停止的容器。
type dockerAdmin struct{}

func (dockerAdmin) Stop(ctx context.Context, id string) error {
	_, err := dockerRun(ctx, "stop", "-t", "0", id)
	return err
}

func (dockerAdmin) Exists(ctx context.Context, id string) (bool, error) {
	_, err := dockerRun(ctx, "inspect", id)
	if err != nil && dockerMissing(err) {
		return false, nil
	}
	return err == nil, err
}

func TestDockerProviderContract(t *testing.T) {
	requireDocker(t)
	run := dockerRunID(t)
	sandboxtest.Run(t, sandboxtest.Harness{Provider: findingDockerProvider{&dockerProvider{run: run}}, Admin: dockerAdmin{}, Image: dockerTestImage})
	waitContainers(t, run, 0)
}
