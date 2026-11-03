package coding

import (
	"context"
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/kart-io/goagent/harness/env"
)

// DefaultEnvironmentTools 是默认探测版本的工具链。
var DefaultEnvironmentTools = []string{"go", "node", "python3", "cargo", "java", "make", "git"}

const (
	defaultMaxEntries   = 60
	maxProbeLineRunes   = 200
	maxEnvironmentTools = 32
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// EnvironmentOptions 配置 Workspace.Environment。
type EnvironmentOptions struct {
	// Tools 是要探测版本的命令名，nil 时使用 DefaultEnvironmentTools，空切片表示不探测工具链。
	Tools []string
	// MaxEntries 是列出的根目录条目上限，0 时默认 60。
	MaxEntries int
	// SkipProbe 只列目录、不执行探测命令。容器执行器每条命令可能要数秒到数十秒，宿主可据此取舍。
	SkipProbe bool
}

// EnvironmentSnapshot 是工作区与执行环境的快照，供宿主在第一次请求模型前注入提示词，省去模型的探索步骤。
type EnvironmentSnapshot struct {
	// Entries 是按词法排序的根目录条目，目录以 / 结尾，不含 .git。
	Entries        []string
	OmittedEntries int
	// Probe 是在执行环境内探测到的系统、git 状态与工具链版本，每行一项；探测失败时为空。
	Probe string
}

// ValidateEnvironmentTools 检查工具名只含字母、数字与 ._+-，它们会拼进探测脚本。
func ValidateEnvironmentTools(names []string) error {
	if len(names) > maxEnvironmentTools {
		return fmt.Errorf("最多探测 %d 个工具", maxEnvironmentTools)
	}
	for _, name := range names {
		if !toolNamePattern.MatchString(name) {
			return fmt.Errorf("工具名 %q 无效", name)
		}
	}
	return nil
}

// Environment 列出根目录，并用工作区执行器探测环境：容器或沙箱中看到的是命令实际运行的环境，而非宿主机。
// 探测沿用工作区命令超时（容器启动可能就要十几秒）；探测失败（执行器不可用、超时）只让 Probe 为空，ctx 结束时返回错误。
func (w *Workspace) Environment(ctx context.Context, opts EnvironmentOptions) (EnvironmentSnapshot, error) {
	toolNames := opts.Tools
	if toolNames == nil {
		toolNames = DefaultEnvironmentTools
	}
	if err := ValidateEnvironmentTools(toolNames); err != nil {
		return EnvironmentSnapshot{}, err
	}
	limit := opts.MaxEntries
	if limit <= 0 {
		limit = defaultMaxEntries
	}
	var snap EnvironmentSnapshot
	err := w.fs.Walk(ctx, ".", func(info env.Info) error {
		if info.Path == "." {
			return nil
		}
		name := info.Path
		if info.IsDir() {
			if name == ".git" {
				return fs.SkipDir
			}
			name += "/"
		}
		if len(snap.Entries) < limit {
			snap.Entries = append(snap.Entries, name)
		} else {
			snap.OmittedEntries++
		}
		if info.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return EnvironmentSnapshot{}, err
	}
	if opts.SkipProbe {
		return snap, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, w.commandTimeout)
	defer cancel()
	res, err := w.exec.Run(probeCtx, env.Command{Argv: []string{"sh", "-c", probeScript(toolNames)}, Dir: "."})
	if err := ctx.Err(); err != nil {
		return EnvironmentSnapshot{}, err
	}
	if err == nil && res.ExitCode == 0 {
		snap.Probe = cleanProbe(string(res.Output))
	}
	return snap, nil
}

// probeScript 只拼接已校验的工具名；版本命令失败或没有输出的工具不列出。GOTOOLCHAIN=local 避免 go version 按 go.mod 联网下载工具链。
func probeScript(toolNames []string) string {
	var sb strings.Builder
	sb.WriteString(`echo "os: $(uname -srm 2>/dev/null)"
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "git: 分支 $(git branch --show-current 2>/dev/null)，$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ') 个未提交改动"
fi
`)
	if len(toolNames) > 0 {
		fmt.Fprintf(&sb, `for t in %s; do
  command -v "$t" >/dev/null 2>&1 || continue
  case "$t" in
    go) v=$(GOTOOLCHAIN=local go version 2>&1) ;;
    java) v=$(java -version 2>&1) ;;
    *) v=$("$t" --version 2>&1) ;;
  esac || continue
  [ -n "$v" ] || continue
  echo "$t: $(printf '%%s\n' "$v" | head -n 1)"
done
`, strings.Join(toolNames, " "))
	}
	return sb.String()
}

func cleanProbe(out string) string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxProbeLineRunes {
			line = string(r[:maxProbeLineRunes]) + "…"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
