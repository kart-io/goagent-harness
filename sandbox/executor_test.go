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
	"testing"
	"time"
)

func TestExecutorLocal(t *testing.T) {
	e := Executor{Backend: "local", Policy: Policy{WorkspaceRoot: t.TempDir(), Mode: ReadOnly}}
	if _, err := e.Run(context.Background(), env.Command{Argv: []string{"true"}}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	e.Policy.Mode = DangerFullAccess
	e.Policy.Network = NetworkInherit
	t.Setenv("SANDBOX_SECRET", "hidden")
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "echo ${SANDBOX_SECRET-unset}; exit 7"}})
	if err != nil || r.ExitCode != 7 || !strings.Contains(string(r.Output), "unset") {
		t.Fatalf("%+v %v", r, err)
	}
	for _, d := range []string{"/tmp", "../"} {
		if _, err := e.Run(context.Background(), env.Command{Argv: []string{"true"}, Dir: d}); err == nil {
			t.Fatal("cwd accepted", d)
		}
	}
}
func TestNativeReal(t *testing.T) {
	e := Executor{Backend: "native", Policy: Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}}
	report, err := e.Probe(context.Background())
	if errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if report.Enforcement != "full" && (report.Runner != RunnerLandlock || report.Enforcement != "partial") {
		t.Fatal(report)
	}
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "echo yes > allowed"}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("%s %v", r.Output, err)
	}
}

func TestNativeBoundaries(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	extra, _ := filepath.EvalSymlinks(t.TempDir())
	e := Executor{Backend: "native", Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny, AdditionalWritableRoots: []string{extra}}}
	if _, err := e.Probe(context.Background()); errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		script string
		ok     bool
	}{{"sh -c " + shellQuote(": > "+shellQuote(filepath.Join(outside, "no"))), false}, {": > " + shellQuote(filepath.Join(extra, "yes")), true}, {": > " + shellQuote(filepath.Join(root, "yes")), true}} {
		r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", test.script}})
		if err != nil || (r.ExitCode == 0) != test.ok {
			t.Fatalf("%s: %+v %v", test.script, r, err)
		}
	}
	os.Symlink(outside, filepath.Join(root, "escape"))
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", ": > escape/no"}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("symlink: %+v %v", r, err)
	}
	e.Policy.Mode = ReadOnly
	r, err = e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", ": > yes"}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("readonly: %+v %v", r, err)
	}
}

// fakeBwrap 让前 failures 次启动失败并输出 message，之后跳过隔离直接运行命令。
func fakeBwrap(t *testing.T, failures int, message string) (count func() int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("只有 Linux 用 bwrap")
	}
	dir := t.TempDir()
	counter := filepath.Join(dir, "count")
	script := fmt.Sprintf(`#!/bin/sh
echo x >> %[1]s
if [ "$(/usr/bin/wc -l < %[1]s)" -le %[2]d ]; then echo %[3]s >&2; exit 1; fi
while [ "$1" != "--" ]; do shift; done
shift
exec "$@"
`, shellQuote(counter), failures, shellQuote(message))
	if err := os.WriteFile(filepath.Join(dir, "bwrap"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		data, _ := os.ReadFile(counter)
		return strings.Count(string(data), "\n")
	}
}

const mountRaceOutput = `bwrap: Can't bind mount /oldroot/ on /newroot/: Unable to apply mount flags: remount "/newroot/tmp/containerd-mount1": Invalid argument`

func TestNativeRetriesMountRace(t *testing.T) {
	count := fakeBwrap(t, 2, mountRaceOutput)
	e := Executor{Backend: "native"}
	p := Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}
	r, err := e.raw(context.Background(), p, env.Command{Argv: []string{"sh", "-c", "echo ran"}})
	if err != nil || r.ExitCode != 0 || !strings.Contains(string(r.Output), "ran") {
		t.Fatalf("%s %+v %v", r.Output, r, err)
	}
	if got := count(); got != 3 {
		t.Fatalf("启动 %d 次", got)
	}
}

func TestNativeMountRaceRetriesAreBounded(t *testing.T) {
	count := fakeBwrap(t, 1000, mountRaceOutput)
	e := Executor{Backend: "native"}
	p := Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}
	if _, err := e.raw(context.Background(), p, env.Command{Argv: []string{"true"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if got := count(); got != mountRaceAttempts {
		t.Fatalf("启动 %d 次", got)
	}
}

func TestNativeOtherStartupFailureIsNotRetried(t *testing.T) {
	count := fakeBwrap(t, 1000, "bwrap: Creating new namespace failed: Operation not permitted")
	e := Executor{Backend: "native"}
	p := Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}
	if _, err := e.raw(context.Background(), p, env.Command{Argv: []string{"true"}}); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if got := count(); got != 1 {
		t.Fatalf("启动 %d 次", got)
	}
}

// 命令自己的输出碰巧相同时，它已经执行过了，绝不能重跑。
func TestNativeCommandOutputIsNotMistakenForMountRace(t *testing.T) {
	count := fakeBwrap(t, 0, "")
	e := Executor{Backend: "native"}
	p := Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}
	r, err := e.raw(context.Background(), p, env.Command{Argv: []string{"sh", "-c", "echo " + shellQuote(mountRaceOutput) + "; exit 1"}})
	if err != nil || r.ExitCode != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if got := count(); got != 1 {
		t.Fatalf("启动 %d 次", got)
	}
}

func TestCancelledAndMismatchedPolicy(t *testing.T) {
	root := t.TempDir()
	e := Executor{Backend: "local", Policy: Policy{WorkspaceRoot: root, Mode: DangerFullAccess, Network: NetworkInherit}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Run(ctx, env.Command{Argv: []string{"sh", "-c", ": > effect"}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx = WithPolicy(context.Background(), Policy{WorkspaceRoot: t.TempDir(), Mode: DangerFullAccess, Network: NetworkInherit})
	if _, err := e.Run(ctx, env.Command{Argv: []string{"true"}}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestNativePrivateTempAndNull(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	e := Executor{Backend: "native", Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "p=$(mktemp \"$TMPDIR/item.XXXXXX\"); echo temp > \"$p\"; printf '%s' \"$p\""}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("temp: %s %v", r.Output, err)
	}
	if !filepath.IsAbs(string(r.Output)) || contains(root, string(r.Output)) {
		t.Fatalf("temp not private: %s", r.Output)
	}
	if _, err := os.Stat(string(r.Output)); !os.IsNotExist(err) {
		t.Fatalf("temp remains: %s %v", r.Output, err)
	}
	e.Policy.Mode = ReadOnly
	r, err = e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "echo sink >/dev/null"}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("null: %s %v", r.Output, err)
	}
}
func TestDockerKeepsImagePath(t *testing.T) {
	bin := t.TempDir()
	capture := filepath.Join(bin, "capture")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellQuote(capture) + "\nif [ \"$1\" != run ]; then exit 0; fi\nwhile [ \"$1\" != test ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	e := Executor{Backend: "docker", Image: "test", Policy: Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.raw(context.Background(), e.Policy, env.Command{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(capture)
	if strings.Contains(string(b), "PATH=") {
		t.Fatalf("host PATH forwarded: %s", b)
	}
}
func TestShellQuote(t *testing.T) {
	want := "a'b $HOME; end"
	out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(want)).Output()
	if err != nil || string(out) != want {
		t.Fatalf("%q %v", out, err)
	}
}
func TestLinuxProcRootCannotEscape(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires Linux Bubblewrap PID namespace")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	e := Executor{Backend: "native", Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: NetworkDeny}}
	if _, err := e.Probe(context.Background()); errors.Is(err, ErrUnavailable) {
		t.Skip(err)
	} else if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("/proc/%d/root%s/escaped", os.Getpid(), outside)
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", ": > " + shellQuote(target)}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("parent proc root escape: %s %v", r.Output, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatal("outside file created")
	}
}
func TestExecutorRootRebindDenied(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	p, err := Normalize(Policy{WorkspaceRoot: root, Mode: DangerFullAccess, Network: NetworkInherit})
	if err != nil {
		t.Fatal(err)
	}
	e := Executor{Backend: "local", Policy: p}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root + "-original")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", ": > escaped"}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("rebind accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatal("outside side effect")
	}
}
func TestDockerReal(t *testing.T) {
	image := os.Getenv("HARNESS_SANDBOX_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set HARNESS_SANDBOX_DOCKER_IMAGE to an already-local image")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Fatal("image must already exist; no pull authorized")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	shared, err := os.MkdirTemp(home, ".harness-docker-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shared)

	root, _ := os.MkdirTemp(shared, "workspace-")
	extra, _ := os.MkdirTemp(shared, "extra-")
	root, _ = filepath.EvalSymlinks(root)
	extra, _ = filepath.EvalSymlinks(extra)
	t.Setenv("SANDBOX_SECRET", "do-not-inherit")
	e := Executor{Backend: "docker", Image: image, Policy: Policy{WorkspaceRoot: root, AdditionalWritableRoots: []string{extra}, Mode: WorkspaceWrite, Network: NetworkDeny}}
	report, err := e.Probe(context.Background())
	if err != nil || report.Enforcement != "full" {
		t.Fatalf("probe %+v %v", report, err)
	}
	r, err := e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "set -e; echo yes > yes; echo extra > " + shellQuote(filepath.Join(extra, "extra")) + "; test -z \"$SANDBOX_SECRET\"; test \"$(ls /sys/class/net)\" = lo"}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("write %s %v", r.Output, err)
	}
	for _, path := range []string{filepath.Join(root, "yes"), filepath.Join(extra, "extra")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	base, _ := env.NewLocalFS(root)
	scoped := FileSystem{FileSystem: base, Policy: e.Policy}
	data, _, scopeErr := scoped.ReadFile(context.Background(), "@root1/extra")
	if scopeErr != nil || string(data) != "extra\n" {
		t.Fatalf("alias %s %v", data, scopeErr)
	}
	r, err = e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "exit 7"}})
	if err != nil || r.ExitCode != 7 {
		t.Fatalf("nonzero %+v %v", r, err)
	}
	e.Policy.Mode = ReadOnly
	r, err = e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "echo no > yes"}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("readonly workspace %+v %v", r, err)
	}
	r, err = e.Run(context.Background(), env.Command{Argv: []string{"sh", "-c", "echo no > " + shellQuote(filepath.Join(extra, "extra"))}})
	if err != nil || r.ExitCode == 0 {
		t.Fatalf("readonly extra %+v %v", r, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = e.Run(ctx, env.Command{Argv: []string{"sleep", "30"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "name=harness-sandbox-", "--filter", "volume="+root).Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("container leftovers %s %v", out, err)
	}
}

func TestDockerUnixSocketReal(t *testing.T) {
	image := os.Getenv("HARNESS_SANDBOX_DOCKER_IMAGE")
	if image == "" {
		t.Skip("requires existing Docker image")
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, ".harness-unix-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	source := `package main
import("net";"os";"time";"syscall")
func main(){if os.Args[1]=="dgram"{pair,e:=syscall.Socketpair(syscall.AF_UNIX,syscall.SOCK_DGRAM,0);if e!=nil{os.Exit(1)};if e=syscall.Sendto(pair[0],[]byte("probe"),0,&syscall.SockaddrUnix{Name:os.Args[2]+".dgram"});e!=nil{os.Exit(1)};return};if os.Args[1]=="client"{c,e:=net.DialTimeout("unix",os.Args[2],time.Second);if e!=nil{os.Exit(1)};c.Close();return};d,e:=net.ListenPacket("unixgram",os.Args[2]+".dgram");if e!=nil{panic(e)};os.Chmod(os.Args[2]+".dgram",0777);go func(){b:=make([]byte,100);for{if _,_,e:=d.ReadFrom(b);e!=nil{return}}}();l,e:=net.Listen("unix",os.Args[2]);if e!=nil{panic(e)};os.Chmod(os.Args[2],0777);for{c,e:=l.Accept();if e!=nil{return};c.Close()}}
`
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	arch, err := exec.Command("docker", "image", "inspect", "--format", "{{.Architecture}}", image).Output()
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", filepath.Join(root, "helper"), filepath.Join(root, "main.go"))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GOROOT=") || strings.HasPrefix(entry, "GOARCH=") || strings.HasPrefix(entry, "GOOS=") || strings.HasPrefix(entry, "CGO_ENABLED=") || strings.HasPrefix(entry, "GOWORK=") {
			continue
		}
		build.Env = append(build.Env, entry)
	}
	build.Env = append(build.Env, "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0", "GOWORK=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build %s %v", out, err)
	}
	name := fmt.Sprintf("harness-unix-fixture-%d", time.Now().UnixNano())
	defer exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "--rm", "-d", "--name", name, "--network", "none", "--mount", "type=bind,src="+root+",dst=/workspace", image, "/workspace/helper", "server", "/workspace/fixture.sock").CombinedOutput(); err != nil {
		t.Fatalf("server %s %v", out, err)
	}
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(filepath.Join(root, "fixture.sock")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, clientMode := range []string{"client", "dgram"} {
		client := []string{"/workspace/helper", clientMode, "/workspace/fixture.sock"}
		positive := append([]string{"run", "--rm", "--network", "none", "--mount", "type=bind,src=" + root + ",dst=/workspace", image}, client...)
		if out, err := exec.Command("docker", positive...).CombinedOutput(); err != nil {
			t.Fatalf("unix positive %s %v", out, err)
		}
		for _, network := range []Network{NetworkDeny, NetworkInherit} {
			e := Executor{Backend: "docker", Image: image, Policy: Policy{WorkspaceRoot: root, Mode: WorkspaceWrite, Network: network}}
			r, err := e.Run(context.Background(), env.Command{Argv: client})
			if err != nil || r.ExitCode == 0 {
				t.Fatalf("unix allowed %s %s %v", network, r.Output, err)
			}
		}
	}
}
func TestNativeUnixSocketInherit(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires Seatbelt or Bubblewrap")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 test helper absent")
	}
	root, err := os.MkdirTemp("/tmp", "harness-unix-real-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	stream := filepath.Join(root, "s.sock")
	dgram := filepath.Join(root, "d.sock")
	l, err := net.Listen("unix", stream)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	d, err := net.ListenPacket("unixgram", dgram)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	e := Executor{Backend: "native", Policy: Policy{WorkspaceRoot: t.TempDir(), Mode: WorkspaceWrite, Network: NetworkInherit}}
	for _, code := range []string{"import socket; s=socket.socket(socket.AF_UNIX); s.connect(" + strconv.Quote(stream) + ")", "import socket; a=socket.socket(socket.AF_UNIX,socket.SOCK_DGRAM); a.sendto(b'probe'," + strconv.Quote(dgram) + ")"} {
		if out, err := exec.Command(python, "-c", code).CombinedOutput(); err != nil {
			t.Fatalf("positive %s %v", out, err)
		}
		r, err := e.Run(context.Background(), env.Command{Argv: []string{python, "-c", code}})
		if err != nil || r.ExitCode == 0 {
			t.Fatalf("Unix pathname allowed: %s %v", r.Output, err)
		}
	}
	r, err := e.Run(context.Background(), env.Command{Argv: []string{python, "-c", "import socket; a,b=socket.socketpair(); a.send(b'x'); assert b.recv(1)==b'x'"}})
	if err != nil || (r.ExitCode == 0) != (runtime.GOOS == "darwin") {
		t.Fatalf("anonymous pair policy mismatch on %s: %s %v", runtime.GOOS, r.Output, err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	go func() {
		conn, err := tcp.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	code := fmt.Sprintf("import socket; socket.create_connection(('127.0.0.1',%d),1).close()", tcp.Addr().(*net.TCPAddr).Port)
	r, err = e.Run(context.Background(), env.Command{Argv: []string{python, "-c", code}})
	if err != nil || r.ExitCode != 0 {
		t.Fatalf("inherit TCP blocked %s %v", r.Output, err)
	}
}
