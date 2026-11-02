package sshsandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
	"golang.org/x/crypto/ssh"
)

func validConfig() Config {
	return Config{
		Addr: "h:22", User: "u", Auth: []ssh.AuthMethod{ssh.Password("x")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // 只校验配置，不连接
		BaseDir:         "/srv/sandboxes", RunnerPath: "/usr/local/bin/sandbox-runner", RunnerSHA256: strings.Repeat("A", 64),
	}
}

func TestNewValidatesConfig(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"没有主机校验":       func(c *Config) { c.HostKeyCallback = nil },
		"没有认证":         func(c *Config) { c.Auth = nil },
		"相对 BaseDir":   func(c *Config) { c.BaseDir = "srv" },
		"根目录":          func(c *Config) { c.BaseDir = "/" },
		"BaseDir 含 ..": func(c *Config) { c.BaseDir = "/srv/../etc" },
		"相对 Runner":    func(c *Config) { c.RunnerPath = "sandbox-runner" },
		"摘要长度不对":       func(c *Config) { c.RunnerSHA256 = "abc" },
	} {
		c := validConfig()
		change(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: 应当拒绝", name)
		}
	}
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p.Version(), strings.Repeat("a", 64)) {
		t.Fatal(p.Version())
	}
}

func TestInstanceIDs(t *testing.T) {
	if idOf("a") != idOf("a") || idOf("a") == idOf("b") || !instanceID.MatchString(idOf("a")) {
		t.Fatal(idOf("a"))
	}
	for _, id := range []string{"", "..", "../etc", "sbx-../../etc", "sbx-" + strings.Repeat("g", 24), "sbx-" + strings.Repeat("a", 25) + "/x"} {
		if instanceID.MatchString(id) {
			t.Errorf("%q 不应通过", id)
		}
	}
}

func TestRunnerError(t *testing.T) {
	if err := runnerError(1, []byte(runnerPrefix+"x")); err != nil {
		t.Fatal(err)
	}
	if err := runnerError(runnerFailedRC, []byte("命令自己退出 125")); err != nil {
		t.Fatal(err)
	}
	if err := runnerError(runnerFailedRC, []byte(runnerPrefix+"x")); !errors.Is(err, sandbox.ErrUnavailable) {
		t.Fatal(err)
	}
	if err := runnerError(runnerFailedRC, []byte(runnerDenied+"x")); !errors.Is(err, sandbox.ErrDenied) {
		t.Fatal(err)
	}
}

func runner(t *testing.T, c runnerConfig, argv ...string) (string, string, int) {
	t.Helper()
	config, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := RunnerMain(append([]string{string(config), "--"}, argv...), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func TestRunnerMain(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := runnerConfig{Workspace: root, Mode: sandbox.WorkspaceWrite, Network: sandbox.NetworkDeny}
	config, _ := json.Marshal(c)
	var probe, probeErr bytes.Buffer
	if code := RunnerMain([]string{"probe", string(config)}, &probe, &probeErr); code != 0 {
		t.Skipf("本机没有可用的隔离后端: %s", probeErr.String())
	}
	var report sandbox.Report
	if err := json.Unmarshal(probe.Bytes(), &report); err != nil || report.Runner == "" {
		t.Fatalf("%s %v", probe.String(), err)
	}

	stdout, stderr, code := runner(t, c, "sh", "-c", "echo out; echo err >&2; echo x > ok; exit 7")
	if code != 7 || !strings.Contains(stdout, "out") || !strings.Contains(stdout, "err") || stderr != "" {
		t.Fatalf("%q %q %d", stdout, stderr, code)
	}
	if _, err := os.Stat(filepath.Join(root, "ok")); err != nil {
		t.Fatal(err)
	}
	if _, _, code = runner(t, c, "sh", "-c", ": > "+shellQuote(filepath.Join(outside, "no"))); code == 0 {
		t.Fatal("越界写入成功")
	}
	bad := c
	bad.Dir = "../"
	if _, stderr, code = runner(t, bad, "true"); code != runnerFailedRC || !errors.Is(runnerError(code, []byte(stderr)), sandbox.ErrDenied) {
		t.Fatalf("%q %d", stderr, code)
	}
	limited := c
	limited.Timeout = 300 * time.Millisecond
	stdout, _, code = runner(t, limited, "sh", "-c", "echo partial; sleep 30")
	if code != runnerTimedOutRC || !strings.HasPrefix(stdout, "partial\n") || !strings.HasSuffix(stdout, runnerTimedOut) {
		t.Fatalf("%q %d", stdout, code)
	}
	var usage bytes.Buffer
	if code := RunnerMain([]string{"nonsense"}, &bytes.Buffer{}, &usage); code != runnerFailedRC || !strings.Contains(usage.String(), runnerPrefix) {
		t.Fatalf("%q %d", usage.String(), code)
	}
}
