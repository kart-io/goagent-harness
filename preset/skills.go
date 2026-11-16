package preset

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/skills"
	"github.com/kart-io/goagent/tools/policy"
	skilltool "github.com/kart-io/goagent/tools/skill"
)

// ComponentSkills 是技能组件名。
const ComponentSkills = "skills"

// ToolLoadSkill 是加载技能说明的工具名。
const ToolLoadSkill = constants.ToolLoadSkill

// SkillsConfig 是 skills 组件的配置。技能是目录下的 <名称>/SKILL.md，开头为 YAML frontmatter（name、description）。
// 系统提示词只列出名称与描述，模型需要时用 load_skill 读取完整说明与目录中的其他文件。
type SkillsConfig struct {
	// Dirs 是技能目录，默认 [.agents/skills, .claude/skills]；相对路径相对工作区根目录，
	// 绝对路径或 ~ 开头的目录在工作区之外，来自不可信层时需要授权。同名技能以先出现的目录为准。
	Dirs []string `yaml:"dirs"`
}

func init() {
	Register(Spec[SkillsConfig]{
		Name: ComponentSkills, Version: "3", Singleton: true,
		Validate: func(c *SkillsConfig) error {
			if len(c.Dirs) == 0 {
				c.Dirs = []string{".agents/skills", ".claude/skills"}
			}
			for _, d := range c.Dirs {
				if strings.TrimSpace(d) == "" {
					return errors.New("技能目录不能为空")
				}
			}
			return nil
		},
		Needs: func(c *SkillsConfig) Needs {
			var n Needs
			for _, d := range c.Dirs {
				if outsideWorkspace(d) {
					n.Grants = append(n.Grants, "读取工作区外的技能目录 "+d)
				}
			}
			return n
		},
		Build: buildSkills,
	})
}

func outsideWorkspace(dir string) bool { return filepath.IsAbs(dir) || strings.HasPrefix(dir, "~") }

func buildSkills(ctx context.Context, a *Assembler, c *SkillsConfig) (err error) {
	var roots []*os.Root
	defer func() {
		for i := len(roots) - 1; i >= 0; i-- {
			err = errors.Join(err, roots[i].Close())
		}
	}()
	for _, d := range c.Dirs {
		dir, err := openSkillDirectory(a.Root(), d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		roots = append(roots, dir)
	}
	catalog, err := skills.Discover(ctx, roots...)
	if err != nil {
		return err
	}
	// Discover borrows the input roots and owns the retained skill handles.
	// Register cleanup before assembly, including for an empty catalog.
	a.OnClose(catalog.Close)
	metadata := catalog.List()
	if len(metadata) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("可用技能（需要时用 " + ToolLoadSkill + " 加载完整说明后再按说明操作）：\n")
	var names []string
	for _, s := range metadata {
		names = append(names, s.Name+"@"+s.Digest[:12])
		if !s.DisableModelInvocation {
			fmt.Fprintf(&sb, "- %s：%s\n", s.Name, s.Description)
		}
	}
	a.AddPrompt(strings.TrimSuffix(sb.String(), "\n"))
	a.AddTools(skilltool.New(catalog))
	a.AddRules([]policy.Rule{{Tool: ToolLoadSkill, Decision: policy.Allow}}, nil, false)
	a.SetInfo("skills", strings.Join(names, ","))
	return nil
}

// Open relative directories through the workspace handle so even concurrent
// symlink replacement cannot redirect discovery outside the configured scope.
func openSkillDirectory(root, d string) (*os.Root, error) {
	switch {
	case strings.HasPrefix(d, "~"):
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return os.OpenRoot(filepath.Join(home, strings.TrimPrefix(d, "~")) + string(os.PathSeparator) + ".")
	case filepath.IsAbs(d):
		return os.OpenRoot(d + string(os.PathSeparator) + ".")
	case root == "":
		return nil, fmt.Errorf("相对技能目录 %s 需要 Host.Root", d)
	}
	workspace, err := os.OpenRoot(root + string(os.PathSeparator) + ".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = workspace.Close() }()
	// A final dot forces d to be acquired as a directory before opening the
	// final component, so a FIFO replacing the source cannot block Build.
	return workspace.OpenRoot(d + string(os.PathSeparator) + ".")
}
