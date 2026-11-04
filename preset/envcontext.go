package preset

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kart-io/goagent-harness/tools/coding"
)

// ComponentEnvContext 是环境上下文组件名。
const ComponentEnvContext = "env-context"

// EnvContextConfig 是 env-context 组件的配置。装配时在工作区执行器内探测一次系统、git 状态与工具链版本，
// 连同根目录列表与命令的沙箱限制拼进系统提示词，模型不必先花几步探索环境，也不会去排查受沙箱限制必然失败的命令。
// 快照固定在系统提示词中，保持前缀可缓存。
type EnvContextConfig struct {
	// Tools 是要探测版本的命令名，缺省为 coding.DefaultEnvironmentTools，写 [] 表示不探测工具链。
	Tools []string `yaml:"tools"`
	// MaxEntries 是列出的根目录条目上限，默认 60。
	MaxEntries int `yaml:"maxEntries"`
	// Probe 默认 true：在执行器内运行一条探测命令，装配因此多花一次命令的时间（容器执行器可能数十秒）。
	// false 时只列根目录。
	Probe *bool `yaml:"probe"`
}

func init() {
	Register(Spec[EnvContextConfig]{
		Name: ComponentEnvContext, Version: "2", Singleton: true, Requires: []string{ComponentWorkspace},
		Validate: func(c *EnvContextConfig) error {
			if c.MaxEntries < 0 {
				return errors.New("maxEntries 不能为负")
			}
			return coding.ValidateEnvironmentTools(c.Tools)
		},
		Build: func(ctx context.Context, a *Assembler, c *EnvContextConfig) error {
			w, info, err := a.Workspace()
			if err != nil {
				return err
			}
			snap, err := w.Environment(ctx, coding.EnvironmentOptions{Tools: c.Tools, MaxEntries: c.MaxEntries, SkipProbe: c.Probe != nil && !*c.Probe})
			if err != nil {
				return err
			}
			a.AddPrompt(renderEnvironment(snap, info))
			return nil
		},
	})
}

func renderEnvironment(snap coding.EnvironmentSnapshot, info WorkspaceInfo) string {
	var sb strings.Builder
	sb.WriteString("工作区环境（会话开始时的快照，之后可能变化）：\n")
	if snap.Probe != "" {
		sb.WriteString(snap.Probe + "\n")
	}
	sb.WriteString("根目录：")
	if len(snap.Entries) == 0 {
		sb.WriteString("（空）")
	}
	for _, e := range snap.Entries {
		sb.WriteString("\n" + e)
	}
	if snap.OmittedEntries > 0 {
		fmt.Fprintf(&sb, "\n……另有 %d 项未列出", snap.OmittedEntries)
	}
	if limits := sandboxLimits(info); len(limits) > 0 {
		sb.WriteString("\n命令在沙箱中运行：" + strings.Join(limits, "；") + "。受这些限制影响的命令或测试在这里必然失败，与代码改动无关：不必排查，在答复中说明即可。")
	}
	return sb.String()
}

// sandboxLimits 列出命令受到的、模型容易误判为代码问题的限制。
func sandboxLimits(info WorkspaceInfo) []string {
	if !info.CommandsEnabled {
		return nil
	}
	var limits []string
	switch info.Sandbox {
	case ReadOnly:
		limits = append(limits, "不能写文件")
	case WorkspaceWrite:
		limits = append(limits, "只能写入工作区与额外授权的目录")
	}
	if !info.Network {
		limits = append(limits, "不能联网")
	}
	// native 后端在这些情况下用 seccomp 或 Seatbelt 禁止 Unix 域套接字，见 sandbox.Executor。
	if info.Backend == "native" && (info.Sandbox != DangerFullAccess || !info.Network) {
		limits = append(limits, "不能创建或连接 Unix 域套接字（监听 unix socket 的测试会失败）")
	}
	return limits
}
