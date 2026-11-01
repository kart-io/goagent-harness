package preset

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kart-io/goagent-harness/permissions"
	"github.com/kart-io/goagent-harness/sandbox"
	"github.com/kart-io/goagent-harness/tools/coding"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/harness/env"
	"github.com/kart-io/goagent/tools"
)

// buildWorkspace keeps legacy local-readonly behavior (no command tools), while
// real confined execution is supplied by Docker or an explicitly selected native backend.
func buildWorkspace(ctx context.Context, a *Assembler, c *WorkspaceConfig) error {
	if a.Root() == "" {
		return fmt.Errorf("Host.Root 未设置")
	}
	local, err := env.NewLocalFS(a.Root())
	if err != nil {
		return err
	}
	network := sandbox.NetworkDeny
	if c.Network {
		network = sandbox.NetworkInherit
	}
	p, err := sandbox.Normalize(sandbox.Policy{Mode: sandbox.Mode(c.Sandbox), WorkspaceRoot: local.Root(), AdditionalWritableRoots: slices.Clone(c.AdditionalWritableRoots), Network: network})
	if err != nil {
		return err
	}
	ceiling := p
	ceiling.Mode = sandbox.Mode(c.MaxSandbox)
	if a.host.SandboxCeiling != nil {
		if err := sandbox.Within(*a.host.SandboxCeiling, ceiling); err != nil {
			return fmt.Errorf("sandbox exceeds hard host ceiling: %w", err)
		}
	}
	if parent := a.opts.parent; parent != nil {
		if parent.policy.WorkspaceRoot != "" {
			if err := sandbox.Within(parent.ceiling, ceiling); err != nil {
				return fmt.Errorf("子 Agent 的沙箱权限宽于父 Agent: %w", err)
			}
		}
		for _, name := range c.EnvAllowlist {
			if !slices.Contains(parent.envAllowlist, name) {
				return fmt.Errorf("子 Agent 不能增加父 Agent 未授权的环境变量 %s", name)
			}
		}
	}
	executor := &sandbox.Executor{Policy: p, Backend: c.Executor, Image: c.Image, EnvAllowlist: slices.Clone(c.EnvAllowlist)}
	commands := true
	report := sandbox.Report{Backend: c.Executor, Enforcement: "unverified", Network: network}
	switch c.Executor {
	case "local":
		if c.Sandbox == ReadOnly && c.MaxSandbox == ReadOnly {
			commands = false
		} else if c.Sandbox != DangerFullAccess || c.MaxSandbox != DangerFullAccess {
			return fmt.Errorf("沙箱模式 %s 要求在容器中执行命令或使用 native 后端，local 无法限制文件写入", c.Sandbox)
		}
		if commands && !c.Network {
			return fmt.Errorf("local 执行器的网络能力与 network: false 不符")
		}
		report.Enforcement = "none"
	case "native":
		report, err = executor.Probe(ctx)
		if err != nil {
			return err
		}
	case "docker":
		// Preserve assembly without a running daemon/image. Run probes before executing.
		// A static contract is not evidence that this machine has a working Docker backend.
	}
	fsys := sandbox.FileSystem{FileSystem: local, Policy: p}
	opts := []coding.Option{coding.WithExecutor(executor)}
	if c.CommandTimeout > 0 {
		opts = append(opts, coding.WithCommandTimeout(c.CommandTimeout))
	}
	if c.MaxOutputBytes > 0 {
		opts = append(opts, coding.WithMaxOutputBytes(c.MaxOutputBytes))
	}
	if len(c.Linters) > 0 {
		linters := c.Linters
		opts = append(opts, coding.WithLinter(func(path string) []string {
			for _, l := range linters {
				if filepath.Ext(path) == l.Ext {
					if strings.HasPrefix(path, "@root") {
						parts := strings.SplitN(path, "/", 2)
						index, err := strconv.Atoi(strings.TrimPrefix(parts[0], "@root"))
						if err == nil && index > 0 && index <= len(p.AdditionalWritableRoots) && len(parts) == 2 {
							path = filepath.Join(p.AdditionalWritableRoots[index-1], filepath.FromSlash(parts[1]))
						}
					}
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
	// Change this contract when backend rules change, including the pinned
	// seccomp profile. Recovery must not silently reuse a different execution boundary.
	contract := "harness-sandbox-v1;moby-seccomp-sha256=6416b47770785a41ac59073cdc77d9fe98517df2799dc83ef207e622de3053f6"
	if c.Executor == "native" {
		contract += ";" + runtime.GOOS + "/" + runtime.GOARCH
	}
	return a.SetWorkspace(w, WorkspaceInfo{
		Root: local.Root(), Sandbox: c.Sandbox, MaxSandbox: c.MaxSandbox,
		Executor: c.Executor, Image: c.Image, Network: c.Network,
		CommandsEnabled: commands, Policy: p, Ceiling: ceiling,
		Backend: report.Backend, Enforcement: report.Enforcement,
		EnvAllowlist: slices.Clone(c.EnvAllowlist), Contract: contract,
	})
}

func (a *Assembler) attachSandbox(pipeline *tools.Pipeline) error {
	if a.workspace == nil {
		return nil
	}
	identity := a.host.AgentID
	if identity == "" {
		identity = a.preset.ID
	}
	config := permissions.Config{AgentID: identity, Default: a.workspaceInfo.Policy, Ceiling: a.workspaceInfo.Ceiling, Approver: pipeline.Approver, Store: a.host.SessionStore}
	if parent := a.opts.parent; parent != nil && parent.controller != nil {
		config.CeilingSnapshot = parent.controller.Snapshot
	}
	controller, err := permissions.New(config)
	if err != nil {
		return err
	}
	a.controller = controller
	pipeline.Around = append([]tools.Around{controller.Around()}, pipeline.Around...)
	return nil
}

func (a *Assembler) sandboxPromptContext() builder.PromptContext {
	return builder.PromptContext{Name: "sandbox-policy", Text: func(scope builder.PromptScope) string {
		// PromptContext has no context/error channel in GoAgent v0.23. Bound store
		// access independently; actual calls always revalidate and fail closed.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		text, err := a.controller.RuntimeContext(ctx, scope.SessionID)
		if err != nil {
			return "Current sandbox policy is unavailable. Tool execution must pass host policy validation; do not infer permission from previous context."
		}
		return text
	}}
}
