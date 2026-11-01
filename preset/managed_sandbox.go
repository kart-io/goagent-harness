package preset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/harness/env"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/sandbox"
)

func prepareSandbox(ctx context.Context, a *Assembler, c *WorkspaceConfig) (*sandbox.Session, error) {
	if a.host.Sandboxes == nil {
		return nil, errors.New("executor=sandbox requires Host.Sandboxes")
	}
	parts := append(slices.Clone(a.opts.scope), a.ComponentID())
	scope, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	session, err := a.host.Sandboxes.Open(ctx, sandbox.Request{Provider: c.Provider, Scope: string(scope), Image: c.Image, Network: c.Network})
	if session != nil {
		a.OnClose(func() error {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return session.Close(cleanup)
		})
	}
	if err != nil {
		return nil, err
	}
	info := session.Info()
	a.SetInfo("sandbox_id", info.ID)
	a.SetInfo("sandbox_root", info.Root)
	a.SetInfo("provider", info.Provider)
	a.SetInfo("provider_version", info.Version)
	a.SetInfo("isolated", strconv.FormatBool(info.Isolated))
	a.SetInfo("remote", strconv.FormatBool(info.Capabilities.Remote))
	a.SetInfo("container", strconv.FormatBool(info.Capabilities.Container))
	a.SetInfo("network", strconv.FormatBool(info.Capabilities.Network))
	a.SetInfo("read_only_root", strconv.FormatBool(info.Capabilities.ReadOnlyRoot))
	return session, nil
}

// Managed backends retain the host authorization domain while operating on a
// separately bound filesystem. Host path and environment mappings are unsupported.
func buildManagedWorkspace(ctx context.Context, a *Assembler, c *WorkspaceConfig, p, ceiling sandbox.Policy) error {
	if len(c.AdditionalWritableRoots) > 0 || len(c.EnvAllowlist) > 0 {
		return fmt.Errorf("%w: managed backends do not map host writable roots or environment allowlists", sandbox.ErrUnsupported)
	}
	session, err := prepareSandbox(ctx, a, c)
	if err != nil {
		return err
	}
	commands := c.MaxSandbox != ReadOnly
	if commands {
		if err = session.CheckPolicy(ctx, p); err != nil {
			return err
		}
		if err = session.CheckPolicy(ctx, ceiling); err != nil {
			return err
		}
	}
	base := session.FileSystem()
	if guard, ok := base.(sandbox.MutationProtector); ok {
		base = guard.WithProtectedNames(".git")
	}
	fsys := sandbox.FileSystem{FileSystem: base, Policy: p}
	executor := managedPolicyExecutor{session: session, policy: p, ceiling: ceiling}
	opts := []coding.Option{coding.WithExecutor(executor)}
	if c.CommandTimeout > 0 {
		opts = append(opts, coding.WithCommandTimeout(c.CommandTimeout))
	}
	if c.MaxOutputBytes > 0 {
		opts = append(opts, coding.WithMaxOutputBytes(c.MaxOutputBytes))
	}
	if len(c.Linters) > 0 {
		if !commands {
			return fmt.Errorf("%w: read-only managed workspace cannot run linters", sandbox.ErrUnsupported)
		}
		opts = append(opts, coding.WithLinter(func(path string) []string {
			for _, l := range c.Linters {
				if filepath.Ext(path) == l.Ext {
					return append(slices.Clone(l.Command), path)
				}
			}
			return nil
		}))
	}
	w, err := coding.New(fsys, nil, opts...)
	if err != nil {
		return err
	}
	info := session.Info()
	return a.SetWorkspace(w, WorkspaceInfo{Provider: c.Provider, SandboxID: info.ID, Root: info.Root, Sandbox: c.Sandbox, MaxSandbox: c.MaxSandbox, Executor: c.Executor, Image: c.Image, Network: c.Network, CommandsEnabled: commands, Policy: p, Ceiling: ceiling, Backend: "sandbox:" + c.Provider, Enforcement: "policy-contract", Contract: "harness-managed-policy-v1;" + info.Version})
}

type managedPolicyExecutor struct {
	session         *sandbox.Session
	policy, ceiling sandbox.Policy
}

func (e managedPolicyExecutor) Capabilities() env.Capabilities { return e.session.Info().Capabilities }
func (e managedPolicyExecutor) Run(ctx context.Context, c env.Command) (env.Result, error) {
	p := e.policy
	if effective, ok := sandbox.FromContext(ctx); ok {
		p = effective
	}
	if err := sandbox.Within(e.ceiling, p); err != nil {
		return env.Result{}, err
	}
	// Invocation policies may change mode only; resource mappings remain immutable.
	if p.WorkspaceRoot != e.policy.WorkspaceRoot || p.Network != e.policy.Network || len(p.AdditionalWritableRoots) != 0 {
		return env.Result{}, sandbox.ErrDenied
	}
	if len(c.Argv) == 0 || filepath.IsAbs(c.Dir) || strings.HasPrefix(filepath.Clean(c.Dir), "..") {
		return env.Result{}, sandbox.ErrDenied
	}
	return e.session.RunWithPolicy(ctx, c, p)
}
