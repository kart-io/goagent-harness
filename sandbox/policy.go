// Package sandbox separates trusted invocation policies from enforcement backends.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"github.com/kart-io/goagent/harness/env"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Mode string

const (
	ReadOnly         Mode = "read-only"
	WorkspaceWrite   Mode = "workspace-write"
	DangerFullAccess Mode = "danger-full-access"
)

func (m Mode) Rank() int {
	switch m {
	case ReadOnly:
		return 0
	case WorkspaceWrite:
		return 1
	case DangerFullAccess:
		return 2
	}
	return -1
}

type Network string

const (
	NetworkDeny    Network = "deny"
	NetworkInherit Network = "inherit"
)

var ErrUnavailable = errors.New("sandbox unavailable")
var ErrDenied = errors.New("sandbox policy denied")

type Policy struct {
	Mode                    Mode
	WorkspaceRoot           string
	AdditionalWritableRoots []string
	Network                 Network
}

func clone(p Policy) Policy {
	p.AdditionalWritableRoots = append([]string(nil), p.AdditionalWritableRoots...)
	return p
}
func Normalize(p Policy) (Policy, error) {
	p = clone(p)
	if p.Mode == "" {
		p.Mode = ReadOnly
	}
	if p.Network == "" {
		p.Network = NetworkDeny
	}
	if p.Mode.Rank() < 0 || (p.Network != NetworkDeny && p.Network != NetworkInherit) {
		return Policy{}, fmt.Errorf("%w: invalid mode or network", ErrDenied)
	}
	if p.WorkspaceRoot == "" {
		return Policy{}, fmt.Errorf("%w: empty workspace", ErrDenied)
	}
	real, err := directory(p.WorkspaceRoot, false)
	if err != nil {
		return Policy{}, err
	}
	p.WorkspaceRoot = real
	for i, r := range p.AdditionalWritableRoots {
		real, err := directory(r, true)
		if err != nil {
			return Policy{}, err
		}
		p.AdditionalWritableRoots[i] = real
	}
	return p, nil
}
func directory(p string, rejectLinks bool) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(a)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: symlink root", ErrDenied)
	}
	r, err := filepath.EvalSymlinks(a)
	if err != nil {
		return "", err
	}
	if rejectLinks && filepath.Clean(a) != r {
		return "", fmt.Errorf("%w: symlink writable root", ErrDenied)
	}
	info, err = os.Stat(r)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%w: root is not directory", ErrDenied)
	}
	return r, nil
}
func contains(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func Within(ceiling, requested Policy) error {
	c, e := Normalize(ceiling)
	if e != nil {
		return e
	}
	r, e := Normalize(requested)
	if e != nil {
		return e
	}
	if r.Mode.Rank() > c.Mode.Rank() || (c.Network == NetworkDeny && r.Network != NetworkDeny) {
		return ErrDenied
	}
	roots := append([]string{c.WorkspaceRoot}, c.AdditionalWritableRoots...)
	workspaceOK := false
	for _, root := range roots {
		workspaceOK = workspaceOK || contains(root, r.WorkspaceRoot)
	}
	if !workspaceOK {
		return ErrDenied
	}
	if c.Mode == DangerFullAccess {
		return nil
	}
	for _, v := range r.AdditionalWritableRoots {
		ok := false
		for _, root := range roots {
			ok = ok || contains(root, v)
		}
		if !ok {
			return ErrDenied
		}
	}
	return nil
}

type policyKey struct{}

func WithPolicy(ctx context.Context, p Policy) context.Context {
	return context.WithValue(ctx, policyKey{}, clone(p))
}
func FromContext(ctx context.Context) (Policy, bool) {
	p, ok := ctx.Value(policyKey{}).(Policy)
	return clone(p), ok
}
func effective(ctx context.Context, p Policy) (Policy, error) {
	if err := ctx.Err(); err != nil {
		return Policy{}, err
	}
	base, err := Normalize(p)
	if err != nil {
		return Policy{}, err
	}
	if v, ok := FromContext(ctx); ok {
		v, err = Normalize(v)
		if err != nil {
			return Policy{}, err
		}
		if base.WorkspaceRoot != v.WorkspaceRoot || base.Network != v.Network || len(base.AdditionalWritableRoots) != len(v.AdditionalWritableRoots) {
			return Policy{}, ErrDenied
		}
		for i, r := range base.AdditionalWritableRoots {
			if v.AdditionalWritableRoots[i] != r {
				return Policy{}, ErrDenied
			}
		}
		return v, nil
	}
	return base, nil
}

type FileSystem struct {
	env.FileSystem
	Policy Policy
}

func (f FileSystem) check(ctx context.Context) error {
	p, e := effective(ctx, f.Policy)
	if e != nil {
		return e
	}
	if p.Mode == ReadOnly {
		return errors.Join(ErrDenied, env.ErrReadOnly)
	}
	return nil
}
func (f FileSystem) WriteFile(ctx context.Context, p string, b []byte, i env.WriteIntent) (env.Version, error) {
	if e := f.check(ctx); e != nil {
		return "", e
	}
	base, rel, _, err := f.route(p)
	if err != nil {
		return "", err
	}
	return base.WriteFile(ctx, rel, b, i)
}
func (f FileSystem) Remove(ctx context.Context, p string) error {
	if e := f.check(ctx); e != nil {
		return e
	}
	base, rel, _, err := f.route(p)
	if err != nil {
		return err
	}
	return base.Remove(ctx, rel)
}

// Named extra roots use @root1/path, @root2/path in trusted configuration order.
func (f FileSystem) route(p string) (env.FileSystem, string, string, error) {
	if !strings.HasPrefix(p, "@root") {
		return f.FileSystem, p, "", nil
	}
	parts := strings.SplitN(filepath.ToSlash(p), "/", 2)
	index, err := strconv.Atoi(strings.TrimPrefix(parts[0], "@root"))
	if err != nil || index < 1 || index > len(f.Policy.AdditionalWritableRoots) {
		return nil, "", "", ErrDenied
	}
	rel := "."
	if len(parts) > 1 {
		rel = parts[1]
	}
	if filepath.IsAbs(rel) {
		return nil, "", "", ErrDenied
	}
	policy, err := Normalize(f.Policy)
	if err != nil {
		return nil, "", "", err
	}
	base, err := env.NewLocalFS(policy.AdditionalWritableRoots[index-1])
	if err != nil {
		return nil, "", "", err
	}
	return base, rel, parts[0] + "/", nil
}
func (f FileSystem) Resolve(p string) (string, error) {
	return f.ResolveContext(context.Background(), p)
}
func (f FileSystem) ResolveContext(ctx context.Context, p string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	base, rel, prefix, err := f.route(p)
	if err != nil {
		return "", err
	}
	if resolver, ok := base.(ContextResolver); ok {
		rel, err = resolver.ResolveContext(ctx, rel)
	} else {
		rel, err = base.Resolve(rel)
	}
	if err != nil {
		return "", err
	}
	return prefix + rel, nil
}
func (f FileSystem) ReadFile(ctx context.Context, p string) ([]byte, env.Version, error) {
	if _, err := effective(ctx, f.Policy); err != nil {
		return nil, "", err
	}
	base, rel, _, err := f.route(p)
	if err != nil {
		return nil, "", err
	}
	return base.ReadFile(ctx, rel)
}
func (f FileSystem) Stat(ctx context.Context, p string) (env.Info, error) {
	if _, err := effective(ctx, f.Policy); err != nil {
		return env.Info{}, err
	}
	base, rel, prefix, err := f.route(p)
	if err != nil {
		return env.Info{}, err
	}
	info, err := base.Stat(ctx, rel)
	info.Path = prefix + info.Path
	return info, err
}
func (f FileSystem) Walk(ctx context.Context, p string, fn func(env.Info) error) error {
	if _, err := effective(ctx, f.Policy); err != nil {
		return err
	}
	base, rel, prefix, err := f.route(p)
	if err != nil {
		return err
	}
	return base.Walk(ctx, rel, func(i env.Info) error { i.Path = prefix + i.Path; return fn(i) })
}
