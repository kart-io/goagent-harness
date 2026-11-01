package sandbox

import (
	"context"
	"errors"
	"github.com/kart-io/goagent/harness/env"
	"os"
	"path/filepath"
	"testing"
)

func TestPolicy(t *testing.T) {
	root := t.TempDir()
	p, err := Normalize(Policy{WorkspaceRoot: root})
	if err != nil || p.Mode != ReadOnly || p.Network != NetworkDeny {
		t.Fatalf("defaults: %+v %v", p, err)
	}
	p.Mode = WorkspaceWrite
	child := p
	child.Network = NetworkInherit
	if !errors.Is(Within(p, child), ErrDenied) {
		t.Fatal("network widened")
	}
	child = p
	child.WorkspaceRoot = t.TempDir()
	if Within(p, child) == nil {
		t.Fatal("root widened")
	}
	extra := t.TempDir()
	p.AdditionalWritableRoots = []string{extra}
	ctx := WithPolicy(context.Background(), p)
	p.AdditionalWritableRoots[0] = "changed"
	got, _ := FromContext(ctx)
	got.AdditionalWritableRoots[0] = "changed"
	again, _ := FromContext(ctx)
	if again.AdditionalWritableRoots[0] != extra {
		t.Fatal("aliased")
	}
	link := filepath.Join(root, "link")
	os.Symlink(extra, link)
	if _, err := Normalize(Policy{WorkspaceRoot: root, AdditionalWritableRoots: []string{link}}); err == nil {
		t.Fatal("accepted symlink")
	}
}
func TestFileSystem(t *testing.T) {
	root := t.TempDir()
	base, _ := env.NewLocalFS(root)
	f := FileSystem{FileSystem: base, Policy: Policy{Mode: ReadOnly, WorkspaceRoot: root}}
	ctx := context.Background()
	if _, err := f.WriteFile(ctx, "x", nil, env.WriteIntent{}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	ctx = WithPolicy(ctx, Policy{Mode: WorkspaceWrite, WorkspaceRoot: root})
	if _, err := f.WriteFile(ctx, "x", nil, env.WriteIntent{}); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove(context.Background(), "x"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}
func TestNamedScopes(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	extra, _ := filepath.EvalSymlinks(t.TempDir())
	base, _ := env.NewLocalFS(root)
	f := FileSystem{FileSystem: base, Policy: Policy{Mode: WorkspaceWrite, WorkspaceRoot: root, AdditionalWritableRoots: []string{extra}}}
	ctx := context.Background()
	if _, err := f.WriteFile(ctx, "@root1/x", []byte("yes"), env.WriteIntent{}); err != nil {
		t.Fatal(err)
	}
	b, _, err := f.ReadFile(ctx, "@root1/x")
	if err != nil || string(b) != "yes" {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := f.Resolve("@root1/../escape"); err == nil {
		t.Fatal("scope escape")
	}
	if _, err := f.Resolve("@root2/x"); err == nil {
		t.Fatal("unknown scope")
	}
	ro := f.Policy
	ro.Mode = ReadOnly
	if err := f.Remove(WithPolicy(ctx, ro), "@root1/x"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}
func TestRootRebindDenied(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	p, err := Normalize(Policy{WorkspaceRoot: root, Mode: WorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(root, root+"-original"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root + "-original")
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := Normalize(p); !errors.Is(err, ErrDenied) {
		t.Fatalf("rebound root accepted: %v", err)
	}
}
