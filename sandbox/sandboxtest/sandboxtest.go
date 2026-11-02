// Package sandboxtest 是 sandbox.Provider 实现者运行的契约测试，也提供模拟远端故障的包装层。
// 实现了可选接口（InstanceFinder、InstanceDestroyer、KeyedExecutor）的 Provider 会多跑对应的检查。
// 通过契约只说明约定被遵守；限流、配额和真实网络行为仍要在真实后端上验证。
package sandboxtest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent/harness/env"
)

// Admin 是测试才需要的后端操作，用来制造实例已停止的状态。
type Admin interface {
	// Stop 让实例停止但不删除；后端没有这种状态时返回 sandbox.ErrUnsupported。
	Stop(ctx context.Context, id string) error
	Exists(ctx context.Context, id string) (bool, error)
}

// Harness 描述被测 Provider。
type Harness struct {
	Provider sandbox.Provider
	Admin    Admin
	Image    string
	// Timeout 限制单项检查，默认 2 分钟。
	Timeout time.Duration
}

// Check 是一项契约检查。返回错误而不是直接失败，这样检查本身也能被测试。
type Check struct {
	Name string
	Run  func(ctx context.Context, h Harness) error
}

// Checks 返回全部检查。
func Checks() []Check {
	return []Check{
		{"lifecycle", checkLifecycle},
		{"connect-missing", checkConnectMissing},
		{"network", checkNetwork},
		{"find-by-scope", checkFindByScope},
		{"destroy-stopped", checkDestroyStopped},
		{"command", checkCommand},
		{"policy-domain", checkPolicyDomain},
		{"file-system", checkFileSystem},
		{"keyed-command", checkKeyedCommand},
	}
}

// Run 运行全部检查。
func Run(t *testing.T, h Harness) {
	t.Helper()
	for _, check := range Checks() {
		t.Run(check.Name, func(t *testing.T) {
			timeout := h.Timeout
			if timeout <= 0 {
				timeout = 2 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := check.Run(ctx, h); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func request(h Harness, name string) sandbox.Request {
	return sandbox.Request{Provider: "contract", Scope: name + "-" + rand.Text()[:12], Image: h.Image}
}

// create 分配实例，返回的 cleanup 保证不留实例，即使检查中途失败。
func create(ctx context.Context, h Harness, r sandbox.Request) (sandbox.Environment, func(), error) {
	e, err := h.Provider.Create(ctx, r)
	if err != nil {
		if e != nil {
			_ = e.Destroy(context.WithoutCancel(ctx))
		}
		return nil, nil, fmt.Errorf("调用 Create: %w", err)
	}
	if e == nil {
		return nil, nil, errors.New("调用 Create 没有返回实例，也没有报错")
	}
	return e, func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = e.Destroy(cleanup)
	}, nil
}

func want(ok bool, format string, args ...any) error {
	if ok {
		return nil
	}
	return fmt.Errorf(format, args...)
}

// 实例能被连回，连接断开不影响它，Destroy 后不存在且可以重试。
func checkLifecycle(ctx context.Context, h Harness) error {
	r := request(h, "lifecycle")
	e, cleanup, err := create(ctx, h, r)
	if err != nil {
		return err
	}
	defer cleanup()
	if e.ID() == "" || e.Root() == "" {
		return errors.New("实例没有 ID 或 Root")
	}
	if _, err := e.FileSystem().WriteFile(ctx, "kept.txt", []byte("内容"), env.WriteIntent{CreateIfAbsent: true}); err != nil {
		return fmt.Errorf("调用 WriteFile: %w", err)
	}
	if err := e.Close(ctx); err != nil {
		return fmt.Errorf("调用 Close: %w", err)
	}
	if err := e.Close(ctx); err != nil {
		return fmt.Errorf("调用 Close 要能重试: %w", err)
	}
	connected, err := h.Provider.Connect(ctx, e.ID(), r)
	if err != nil {
		return fmt.Errorf("断开后 Connect: %w", err)
	}
	defer func() { _ = connected.Close(ctx) }()
	if connected.ID() != e.ID() {
		return fmt.Errorf("调用 Connect 返回了别的实例: %s != %s", connected.ID(), e.ID())
	}
	data, _, err := connected.FileSystem().ReadFile(ctx, "kept.txt")
	if err != nil {
		return fmt.Errorf("连回后读文件: %w", err)
	}
	if string(data) != "内容" {
		return fmt.Errorf("连回后文件内容变了: %q", data)
	}
	if err := connected.Destroy(ctx); err != nil {
		return fmt.Errorf("调用 Destroy: %w", err)
	}
	if h.Admin != nil {
		exists, err := h.Admin.Exists(ctx, e.ID())
		if err != nil {
			return err
		}
		if exists {
			return errors.New("调用 Destroy 后实例仍然存在")
		}
	}
	if err := connected.Destroy(ctx); err != nil && !errors.Is(err, sandbox.ErrInstanceNotFound) {
		return fmt.Errorf("调用 Destroy 要能重试: %w", err)
	}
	return nil
}

// 实例不存在要明确说出来，否则释放会一直重试，子工作也无法区分网络错误。
func checkConnectMissing(ctx context.Context, h Harness) error {
	r := request(h, "missing")
	e, cleanup, err := create(ctx, h, r)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := e.Destroy(ctx); err != nil {
		return fmt.Errorf("调用 Destroy: %w", err)
	}
	connected, err := h.Provider.Connect(ctx, e.ID(), r)
	if connected != nil {
		_ = connected.Close(ctx)
	}
	return want(errors.Is(err, sandbox.ErrInstanceNotFound), "调用 Connect 已删除的实例要返回 ErrInstanceNotFound，实际: %v", err)
}

func checkNetwork(ctx context.Context, h Harness) error {
	e, cleanup, err := create(ctx, h, request(h, "network"))
	if err != nil {
		return err
	}
	defer cleanup()
	return want(!capabilities(e).Network, "没有请求网络，实际能力却声明有网络")
}

func capabilities(e sandbox.Environment) env.Capabilities {
	return e.Executor().Capabilities()
}

func checkFindByScope(ctx context.Context, h Harness) error {
	finder, ok := h.Provider.(sandbox.InstanceFinder)
	if !ok {
		return nil
	}
	r := request(h, "find")
	e, cleanup, err := create(ctx, h, r)
	if err != nil {
		return err
	}
	defer cleanup()
	id, found, err := finder.FindByScope(ctx, r)
	if err != nil {
		return fmt.Errorf("调用 FindByScope: %w", err)
	}
	if !found || id != e.ID() {
		return fmt.Errorf("刚创建的实例要能查到，实际 found=%v id=%q，查询有延迟时要返回错误而不是 found=false", found, id)
	}
	other := r
	other.Scope += "-other"
	if _, found, err = finder.FindByScope(ctx, other); err != nil || found {
		return fmt.Errorf("别的 Scope 不应查到实例: found=%v err=%v", found, err)
	}
	if err := e.Destroy(ctx); err != nil {
		return fmt.Errorf("调用 Destroy: %w", err)
	}
	_, found, err = finder.FindByScope(ctx, r)
	if err != nil {
		return fmt.Errorf("调用 FindByScope: %w", err)
	}
	return want(!found, "已删除的实例不应查到")
}

// 宿主重启后要能删除已停止和已删除的实例。
func checkDestroyStopped(ctx context.Context, h Harness) error {
	if h.Admin == nil {
		return nil
	}
	registry := &sandbox.Registry{Namespace: "contract", Providers: map[string]sandbox.Provider{"contract": h.Provider}}
	defer func() { _ = registry.Close(context.WithoutCancel(ctx)) }()
	r := request(h, "stopped")
	session, err := registry.OpenWith(ctx, r, func(sandbox.Request) (sandbox.Binding, error) { return sandbox.Binding{Retain: true}, nil })
	if err != nil {
		return fmt.Errorf("分配: %w", err)
	}
	id := session.Info().ID
	if err := session.Close(ctx); err != nil {
		return fmt.Errorf("调用 Close: %w", err)
	}
	stopErr := h.Admin.Stop(ctx, id)
	if stopErr != nil && !errors.Is(stopErr, sandbox.ErrUnsupported) {
		return fmt.Errorf("调用 Stop: %w", stopErr)
	}
	if err := registry.Destroy(ctx, r, id); err != nil {
		return fmt.Errorf("调用 Registry.Destroy 已停止的实例: %w", err)
	}
	exists, err := h.Admin.Exists(ctx, id)
	if err != nil {
		return err
	}
	if exists {
		return errors.New("调用 Registry.Destroy 后实例仍然存在")
	}
	if err := registry.Destroy(ctx, r, id); err != nil {
		return fmt.Errorf("调用 Registry.Destroy 要能重试: %w", err)
	}
	return nil
}

// 文件和命令要落在同一个工作区，非零退出码是结果不是错误。
func checkCommand(ctx context.Context, h Harness) error {
	e, cleanup, err := create(ctx, h, request(h, "command"))
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := e.FileSystem().WriteFile(ctx, "dir/from-fs.txt", []byte("文件内容"), env.WriteIntent{CreateIfAbsent: true}); err != nil {
		return fmt.Errorf("调用 WriteFile: %w", err)
	}
	res, err := e.Executor().Run(ctx, env.Command{Argv: []string{"sh", "-c", "cat from-fs.txt; echo 错误 >&2; exit 3"}, Dir: "dir"})
	if err != nil {
		return fmt.Errorf("调用 Run: %w", err)
	}
	if res.ExitCode != 3 {
		return fmt.Errorf("退出码 %d，期望 3", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), "文件内容") || !strings.Contains(string(res.Output), "错误") {
		return fmt.Errorf("要合并输出，并在 Dir 下执行: %q", res.Output)
	}
	return nil
}

func checkFileSystem(ctx context.Context, h Harness) error {
	e, cleanup, err := create(ctx, h, request(h, "fs"))
	if err != nil {
		return err
	}
	defer cleanup()
	fsys := e.FileSystem()
	if _, _, err = fsys.ReadFile(ctx, "missing.txt"); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("读不存在的文件要返回 fs.ErrNotExist: %v", err)
	}
	version, err := fsys.WriteFile(ctx, "dir/a.txt", []byte("一"), env.WriteIntent{CreateIfAbsent: true})
	if err != nil {
		return fmt.Errorf("调用 WriteFile: %w", err)
	}
	if _, err = fsys.WriteFile(ctx, "dir/a.txt", []byte("二"), env.WriteIntent{CreateIfAbsent: true}); !errors.Is(err, env.ErrExists) {
		return fmt.Errorf("写入 CreateIfAbsent 要返回 ErrExists: %v", err)
	}
	if _, err = fsys.WriteFile(ctx, "dir/a.txt", []byte("二"), env.WriteIntent{IfVersion: "stale"}); !errors.Is(err, env.ErrStale) {
		return fmt.Errorf("写入 IfVersion 要返回 ErrStale: %v", err)
	}
	if _, err = fsys.WriteFile(ctx, "dir/a.txt", []byte("二"), env.WriteIntent{IfVersion: version}); err != nil {
		return fmt.Errorf("版本匹配时要能写入: %w", err)
	}
	data, _, err := fsys.ReadFile(ctx, "dir/a.txt")
	if err != nil || string(data) != "二" {
		return fmt.Errorf("读回: %q %v", data, err)
	}
	info, err := fsys.Stat(ctx, "dir")
	if err != nil || !info.IsDir() {
		return fmt.Errorf("调用 Stat 目录: %+v %v", info, err)
	}
	var walked []string
	if err := fsys.Walk(ctx, ".", func(i env.Info) error { walked = append(walked, i.Path); return nil }); err != nil {
		return fmt.Errorf("调用 Walk: %w", err)
	}
	if !slices.Contains(walked, "dir/a.txt") {
		return fmt.Errorf("调用 Walk 漏了文件: %v", walked)
	}
	if _, err = fsys.Resolve("../etc/passwd"); !errors.Is(err, env.ErrOutsideWorkspace) {
		return fmt.Errorf("路径越界要返回 ErrOutsideWorkspace: %v", err)
	}
	if res, err := e.Executor().Run(ctx, env.Command{Argv: []string{"ln", "-s", "/etc", "link"}}); err != nil || res.ExitCode != 0 {
		return fmt.Errorf("创建符号链接: %d %v", res.ExitCode, err)
	}
	if _, err = fsys.Resolve("link/passwd"); !errors.Is(err, env.ErrOutsideWorkspace) {
		return fmt.Errorf("符号链接逃逸要返回 ErrOutsideWorkspace: %v", err)
	}
	if err = fsys.Remove(ctx, "dir/a.txt"); err != nil {
		return fmt.Errorf("调用 Remove: %w", err)
	}
	_, err = fsys.Stat(ctx, "dir/a.txt")
	return want(errors.Is(err, fs.ErrNotExist), "调用 Remove 后 Stat 要返回 fs.ErrNotExist: %v", err)
}

// 断开和 Close 都不影响命令，同键不重跑，AttachOnly 查不到时不启动，取消和超时都生效。
// Policy.WorkspaceRoot 是宿主的授权域，托管工作区传的是宿主路径，不是实例内的路径。Provider 要把它绑定到自己的工作区。
func checkPolicyDomain(ctx context.Context, h Harness) error {
	e, cleanup, err := create(ctx, h, request(h, "policy-domain"))
	if err != nil {
		return err
	}
	defer cleanup()
	executor, ok := e.Executor().(sandbox.PolicyExecutor)
	if !ok {
		return nil
	}
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: "/host/authorization/domain"}
	if err := executor.CheckPolicy(ctx, policy); err != nil {
		return fmt.Errorf("调用 CheckPolicy，WorkspaceRoot 是宿主路径: %w", err)
	}
	res, err := executor.RunWithPolicy(ctx, env.Command{Argv: []string{"sh", "-c", "echo x > bound.txt"}}, policy)
	if err != nil || res.ExitCode != 0 {
		return fmt.Errorf("调用 RunWithPolicy: %d %q %w", res.ExitCode, res.Output, err)
	}
	if _, _, err := e.FileSystem().ReadFile(ctx, "bound.txt"); err != nil {
		return fmt.Errorf("命令要落在实例的工作区: %w", err)
	}
	return nil
}

func checkKeyedCommand(ctx context.Context, h Harness) error {
	e, cleanup, err := create(ctx, h, request(h, "keyed"))
	if err != nil {
		return err
	}
	defer cleanup()
	keyed, ok := e.Executor().(sandbox.KeyedExecutor)
	if !ok {
		return nil
	}
	policy := sandbox.Policy{Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny, WorkspaceRoot: e.Root()}
	shell := func(key, script string, timeout time.Duration) sandbox.KeyedCommand {
		return sandbox.KeyedCommand{Key: key, Timeout: timeout, Command: env.Command{Argv: []string{"sh", "-c", script}}}
	}
	missing := shell("missing", "echo x >> never.txt", time.Minute)
	missing.AttachOnly = true
	if _, err := keyed.RunKeyed(ctx, missing, policy); !errors.Is(err, sandbox.ErrCommandLost) {
		return fmt.Errorf("调用 AttachOnly 查不到时要返回 ErrCommandLost: %v", err)
	}
	if _, _, err := e.FileSystem().ReadFile(ctx, "never.txt"); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("调用 AttachOnly 不能启动命令: %v", err)
	}

	script := "echo x >> effect.txt; while [ ! -e release.txt ]; do sleep 0.05; done; echo 结果; exit 7"
	running := shell("running", script, time.Minute)
	disconnect, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := keyed.RunKeyed(disconnect, running, policy)
		done <- err
	}()
	if err := waitFile(ctx, e, "effect.txt"); err != nil {
		return fmt.Errorf("命令没有启动: %w", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		return fmt.Errorf("ctx 结束时要返回取消: %v", err)
	}
	if err := e.Close(ctx); err != nil {
		return fmt.Errorf("调用 Close: %w", err)
	}
	if _, err := e.FileSystem().WriteFile(ctx, "release.txt", nil, env.WriteIntent{CreateIfAbsent: true}); err != nil {
		return fmt.Errorf("调用 WriteFile: %w", err)
	}
	running.AttachOnly = true
	res, err := keyed.RunKeyed(ctx, running, policy)
	if err != nil {
		return fmt.Errorf("断开后附着: %w", err)
	}
	if res.ExitCode != 7 || !strings.Contains(string(res.Output), "结果") {
		return fmt.Errorf("附着取回的结果不对: %d %q", res.ExitCode, res.Output)
	}
	running.AttachOnly = false
	if _, err = keyed.RunKeyed(ctx, running, policy); err != nil {
		return fmt.Errorf("同键再次运行: %w", err)
	}
	data, _, err := e.FileSystem().ReadFile(ctx, "effect.txt")
	if err != nil || strings.Count(string(data), "x") != 1 {
		return fmt.Errorf("同键只能执行一次: %q %v", data, err)
	}

	res, err = keyed.RunKeyed(ctx, shell("slow", "sleep 300", time.Second), policy)
	if err != nil {
		return fmt.Errorf("超时命令: %w", err)
	}
	if !res.TimedOut {
		return errors.New("超时要标记 TimedOut")
	}

	if started, err := keyed.CancelKeyed(ctx, "never-started", policy); err != nil || started {
		return fmt.Errorf("没启动过的键: started=%v err=%v", started, err)
	}
	cancelled := make(chan error, 1)
	go func() {
		_, err := keyed.RunKeyed(ctx, shell("cancel", "echo x > cancel.txt; sleep 300", time.Minute), policy)
		cancelled <- err
	}()
	if err := waitFile(ctx, e, "cancel.txt"); err != nil {
		return fmt.Errorf("命令没有启动: %w", err)
	}
	started, err := keyed.CancelKeyed(ctx, "cancel", policy)
	if err != nil || !started {
		return fmt.Errorf("取消运行中的命令: started=%v err=%v", started, err)
	}
	select {
	case err := <-cancelled:
		if err != nil {
			return fmt.Errorf("取消后等待结果: %w", err)
		}
		return nil
	case <-ctx.Done():
		return errors.New("取消后命令没有结束")
	}
}

func waitFile(ctx context.Context, e sandbox.Environment, name string) error {
	for {
		_, err := e.FileSystem().Stat(ctx, name)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
