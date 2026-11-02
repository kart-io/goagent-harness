package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/background"
	"github.com/kart-io/goagent-harness/preset"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/sandbox/sandboxtest"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/llm"
	"github.com/stretchr/testify/require"
)

const (
	openSandboxEnv  = "GOAGENT_OPENSANDBOX_URL"
	execdPort       = 44772
	openSandboxTTL  = 1800
	metadataScope   = "goagent-scope"
	metadataRun     = "goagent-run"
	stdoutSeparator = "==goagent-stdout=="
	stderrSeparator = "==goagent-stderr=="
)

func requireOpenSandbox(t *testing.T) string {
	t.Helper()
	base := os.Getenv(openSandboxEnv)
	if base == "" {
		t.Skipf("未设置 %s，跳过 OpenSandbox 测试", openSandboxEnv)
	}
	return strings.TrimSuffix(base, "/")
}

// openSandboxProvider 是经 HTTP 访问的真实远端 Provider，只用于测试。
// 这个后端没有幂等键：客户端放弃 Create 之后实例可能才出现，FindByScope 查不到。所以实例总是带 TTL，泄漏有上限。
type openSandboxProvider struct {
	base string
	run  string
}

type openSandboxInstance struct {
	ID     string `json:"id"`
	Status struct {
		State string `json:"state"`
	} `json:"status"`
}

func (p *openSandboxProvider) Version() string { return "opensandbox-test-v1" }

func (p *openSandboxProvider) call(ctx context.Context, method, route string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+"/v1"+route, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d: %s", method, route, resp.StatusCode, data)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func (p *openSandboxProvider) Create(ctx context.Context, r sandbox.Request) (sandbox.Environment, error) {
	body := map[string]any{
		"image":          map[string]string{"uri": r.Image},
		"entrypoint":     []string{"sh", "-c", "mkdir -p " + dockerWorkspace + "; exec tail -f /dev/null"},
		"timeout":        openSandboxTTL,
		"resourceLimits": map[string]string{"cpu": "500m", "memory": "256Mi"},
		"metadata":       map[string]string{metadataScope: scopeHash(r.Scope), metadataRun: p.run},
	}
	if !r.Network {
		body["networkPolicy"] = map[string]string{"defaultAction": "deny"}
	}
	var instance openSandboxInstance
	code, err := p.call(ctx, http.MethodPost, "/sandboxes", body, &instance)
	if err != nil {
		if code >= 400 && code < 500 {
			return nil, fmt.Errorf("%w: %v", sandbox.ErrNotCreated, err)
		}
		return nil, err
	}
	return p.attach(ctx, instance.ID, r)
}

// attach 等执行服务就绪：状态是 Running 时它还可能连不上。没就绪也要把实例交回去，调用方才能清理。
func (p *openSandboxProvider) attach(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	e := &openSandboxEnvironment{p: p, id: id, network: r.Network}
	var endpoint struct {
		Endpoint string `json:"endpoint"`
	}
	if _, err := p.call(ctx, http.MethodGet, fmt.Sprintf("/sandboxes/%s/endpoints/%d", id, execdPort), nil, &endpoint); err != nil {
		return e, err
	}
	e.execd = endpoint.Endpoint
	if !strings.Contains(e.execd, "://") {
		e.execd = "http://" + e.execd
	}
	var last error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, err := http.NewRequestWithContext(probe, http.MethodGet, e.execd+"/ping", nil)
		if err != nil {
			cancel()
			return e, err
		}
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return e, nil
			}
			err = fmt.Errorf("ping: %d", resp.StatusCode)
		}
		last = err
		select {
		case <-ctx.Done():
			return e, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return e, fmt.Errorf("执行服务没有就绪: %w", last)
}

func gone(state string) bool {
	return state == "Terminated" || state == "Failed" || state == "Stopping"
}

func (p *openSandboxProvider) Connect(ctx context.Context, id string, r sandbox.Request) (sandbox.Environment, error) {
	var instance openSandboxInstance
	code, err := p.call(ctx, http.MethodGet, "/sandboxes/"+id, nil, &instance)
	if code == http.StatusNotFound || (err == nil && gone(instance.Status.State)) {
		return nil, fmt.Errorf("实例 %s: %w", id, sandbox.ErrInstanceNotFound)
	}
	if err != nil {
		return nil, err
	}
	e, err := p.attach(ctx, id, r)
	if err != nil {
		return nil, err
	}
	return e, nil
}

func (p *openSandboxProvider) list(ctx context.Context, key, value string) ([]openSandboxInstance, error) {
	var page struct {
		Items []openSandboxInstance `json:"items"`
	}
	query := url.Values{"metadata": {key + "=" + value}, "pageSize": {"100"}}
	if _, err := p.call(ctx, http.MethodGet, "/sandboxes?"+query.Encode(), nil, &page); err != nil {
		return nil, err
	}
	live := page.Items[:0]
	for _, item := range page.Items {
		if !gone(item.Status.State) {
			live = append(live, item)
		}
	}
	return live, nil
}

func (p *openSandboxProvider) FindByScope(ctx context.Context, r sandbox.Request) (string, bool, error) {
	items, err := p.list(ctx, metadataScope, scopeHash(r.Scope))
	switch {
	case err != nil:
		return "", false, err
	case len(items) == 0:
		return "", false, nil
	case len(items) > 1:
		return "", false, fmt.Errorf("同一 Scope 有 %d 个实例", len(items))
	}
	return items[0].ID, true, nil
}

func (p *openSandboxProvider) DestroyInstance(ctx context.Context, id string, _ sandbox.Request) error {
	code, err := p.call(ctx, http.MethodDelete, "/sandboxes/"+id, nil, nil)
	if code == http.StatusNotFound {
		return sandbox.ErrInstanceNotFound
	}
	return err
}

func (p *openSandboxProvider) Stop(ctx context.Context, id string) error {
	_, err := p.call(ctx, http.MethodPost, "/sandboxes/"+id+"/pause", nil, nil)
	return err
}

func (p *openSandboxProvider) Exists(ctx context.Context, id string) (bool, error) {
	var instance openSandboxInstance
	code, err := p.call(ctx, http.MethodGet, "/sandboxes/"+id, nil, &instance)
	if code == http.StatusNotFound {
		return false, nil
	}
	return err == nil && !gone(instance.Status.State), err
}

type openSandboxEnvironment struct {
	p       *openSandboxProvider
	id      string
	execd   string
	network bool
}

func (e *openSandboxEnvironment) ID() string                  { return e.id }
func (e *openSandboxEnvironment) Root() string                { return dockerWorkspace }
func (e *openSandboxEnvironment) FileSystem() env.FileSystem  { return dockerFS{e} }
func (e *openSandboxEnvironment) Executor() env.Executor      { return e }
func (e *openSandboxEnvironment) Isolated() bool              { return true }
func (e *openSandboxEnvironment) Close(context.Context) error { return nil }
func (e *openSandboxEnvironment) Destroy(ctx context.Context) error {
	err := e.p.DestroyInstance(ctx, e.id, sandbox.Request{})
	if errors.Is(err, sandbox.ErrInstanceNotFound) {
		return nil
	}
	return err
}
func (e *openSandboxEnvironment) Capabilities() env.Capabilities {
	return env.Capabilities{Remote: true, Container: true, Network: e.network}
}
func (e *openSandboxEnvironment) CheckPolicy(ctx context.Context, p sandbox.Policy) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(p.AdditionalWritableRoots) != 0 || (p.Network == sandbox.NetworkInherit) != e.network {
		return sandbox.ErrUnsupported
	}
	return nil
}
func (e *openSandboxEnvironment) RunWithPolicy(ctx context.Context, c env.Command, p sandbox.Policy) (env.Result, error) {
	if err := e.CheckPolicy(ctx, p); err != nil {
		return env.Result{}, err
	}
	if p.Mode == sandbox.ReadOnly {
		return env.Result{}, sandbox.ErrDenied
	}
	return e.Run(ctx, c)
}
func (e *openSandboxEnvironment) Run(ctx context.Context, c env.Command) (env.Result, error) {
	stdout, stderr, code, err := e.exec(ctx, c.Dir, nil, c.Argv...)
	return env.Result{Output: append(stdout, stderr...), ExitCode: code}, err
}
func (e *openSandboxEnvironment) supervisor() *sandbox.CommandSupervisor {
	return &sandbox.CommandSupervisor{Exec: e, RequestTimeout: 10 * time.Second}
}
func (e *openSandboxEnvironment) RunKeyed(ctx context.Context, c sandbox.KeyedCommand, p sandbox.Policy) (sandbox.KeyedResult, error) {
	return e.supervisor().RunKeyed(ctx, c, p)
}
func (e *openSandboxEnvironment) CancelKeyed(ctx context.Context, key string, p sandbox.Policy) (bool, error) {
	return e.supervisor().CancelKeyed(ctx, key, p)
}

func shellWords(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// exec 经 HTTP 运行命令。这个版本的事件会吃掉换行，也不支持 stdin，所以在实例内捕获输出并用 base64 传回。
func (e *openSandboxEnvironment) exec(ctx context.Context, dir string, stdin []byte, argv ...string) ([]byte, []byte, int, error) {
	input := "</dev/null"
	command := shellWords(argv)
	if stdin != nil {
		command = "printf %s '" + base64.StdEncoding.EncodeToString(stdin) + "' | base64 -d | " + command
		input = ""
	}
	script := `o=$(mktemp); e=$(mktemp); ` + command + ` >"$o" 2>"$e" ` + input + `; rc=$?; echo ` + stdoutSeparator + `; base64 "$o"; echo; echo ` + stderrSeparator + `; base64 "$e"; echo; rm -f "$o" "$e"; exit $rc`
	body, err := json.Marshal(map[string]string{"command": script, "cwd": path.Join(dockerWorkspace, dir)})
	if err != nil {
		return nil, nil, -1, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.execd+"/command", bytes.NewReader(body))
	if err != nil {
		return nil, nil, -1, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, -1, unconfirmed(ctx, e.id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resp.Body)
		return nil, nil, -1, fmt.Errorf("command: %d: %s", resp.StatusCode, data)
	}
	var out strings.Builder
	code, finished := -1, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1<<16), 1<<26)
	for scanner.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Text  string `json:"text"`
			Error struct {
				Name  string `json:"ename"`
				Value string `json:"evalue"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil, nil, -1, fmt.Errorf("解析事件: %w", err)
		}
		switch event.Type {
		case "stdout":
			out.WriteString(event.Text + "\n")
		case "execution_complete":
			if !finished {
				code, finished = 0, true
			}
		case "error":
			parsed, err := strconv.Atoi(event.Error.Value)
			if err != nil {
				return nil, nil, -1, fmt.Errorf("执行失败: %s: %s", event.Error.Name, event.Error.Value)
			}
			code, finished = parsed, true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, -1, unconfirmed(ctx, e.id, err)
	}
	if !finished {
		return nil, nil, -1, unconfirmed(ctx, e.id, errors.New("事件流结束，但没有收到结果"))
	}
	return splitOutput(out.String(), code)
}

func unconfirmed(ctx context.Context, id string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return &sandbox.UnconfirmedError{SandboxID: id, Cause: cause}
}

func splitOutput(text string, code int) ([]byte, []byte, int, error) {
	_, rest, ok := strings.Cut(text, stdoutSeparator)
	if !ok {
		return nil, nil, -1, fmt.Errorf("输出缺少分隔符: %q", text)
	}
	encodedOut, encodedErr, ok := strings.Cut(rest, stderrSeparator)
	if !ok {
		return nil, nil, -1, fmt.Errorf("输出缺少分隔符: %q", text)
	}
	stdout, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encodedOut), ""))
	if err != nil {
		return nil, nil, -1, err
	}
	stderr, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encodedErr), ""))
	if err != nil {
		return nil, nil, -1, err
	}
	return stdout, stderr, code, nil
}

func openSandboxFixture(t *testing.T) *openSandboxProvider {
	t.Helper()
	p := &openSandboxProvider{base: requireOpenSandbox(t), run: dockerRunIDOnly()}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		items, err := p.list(ctx, metadataRun, p.run)
		if err != nil {
			t.Errorf("清理: %v", err)
			return
		}
		for _, item := range items {
			_ = p.DestroyInstance(ctx, item.ID, sandbox.Request{})
		}
	})
	return p
}

func (p *openSandboxProvider) instances(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := p.list(ctx, metadataRun, p.run)
	require.NoError(t, err)
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (p *openSandboxProvider) waitInstances(t *testing.T, count int) []string {
	t.Helper()
	var ids []string
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if ids = p.instances(t); len(ids) == count {
			return ids
		}
	}
	require.Failf(t, "实例数量不符", "期望 %d 个，实际 %v", count, ids)
	return ids
}

func (p *openSandboxProvider) shell(t *testing.T, id, script string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := p.Connect(ctx, id, sandbox.Request{})
	require.NoError(t, err)
	res, err := e.Executor().Run(ctx, env.Command{Argv: []string{"sh", "-c", script}})
	require.NoError(t, err)
	return strings.TrimSpace(string(res.Output))
}

func TestOpenSandboxProviderContract(t *testing.T) {
	p := openSandboxFixture(t)
	sandboxtest.Run(t, sandboxtest.Harness{Provider: p, Admin: p, Image: dockerTestImage, Timeout: 4 * time.Minute})
	p.waitInstances(t, 0)
}

// 真实远端上的主流程：进程停在命令中途，重启后连回同一个实例并附着，副作用只有一次，结束后实例被删除。
func TestOpenSandboxStopDuringCommandAttaches(t *testing.T) {
	p := openSandboxFixture(t)
	dir := t.TempDir()
	first := &scriptedModel{steps: []llm.ToolCall{dockerCall("c1", "run_command", map[string]any{"command": slowCommand()})}}
	rig := startRig(t, rigConfig{dir: dir, namespace: p.run, provider: p, run: preset.BuilderRunner(first, 8)})
	rig.spawn("child", "执行")
	id := p.waitInstances(t, 1)[0]
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if strings.Contains(p.shell(t, id, "cat /tmp/effect 2>/dev/null || true"), "once") {
			break
		}
	}
	rig.stop()
	require.Equal(t, "once", p.shell(t, id, "cat /tmp/effect"))

	second := secondModel()
	rig = startRig(t, rigConfig{dir: dir, namespace: p.run, provider: p, run: preset.BuilderRunner(second, 8)})
	p.shell(t, id, "touch /tmp/release")
	require.Equal(t, "完成", rig.wait("child", background.StatusCompleted).Result)
	require.Contains(t, second.seen(), "命令结果")
	p.waitInstances(t, 0)
}

// 创建响应丢失、查询和删除各失败两次：实例不泄漏，也不留问题。
func TestOpenSandboxLostCreateResponseDoesNotLeak(t *testing.T) {
	p := openSandboxFixture(t)
	faults := &sandboxtest.Faults{Provider: p}
	faults.LoseCreateResponse.Store(1)
	faults.FailFind.Store(2)
	faults.FailDestroy.Store(2)
	rig := startRig(t, rigConfig{dir: t.TempDir(), namespace: p.run, provider: faults,
		run: func(context.Context, *preset.Assembly, string) (string, error) { return "不应运行", nil }})
	rig.spawn("child", "任务")
	rig.wait("child", background.StatusFailed)
	p.waitInstances(t, 0)
	waitNoProblems(t, rig.manager)
}
