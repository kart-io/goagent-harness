package preset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kart-io/goagent/builder"
)

// ComponentVerifyBeforeStop 是结束前验证组件名。
const ComponentVerifyBeforeStop = "verify-before-stop"

// verifyFeedbackPrefix 开头的续跑消息来自本组件，压缩时不当作用户原话。
const verifyFeedbackPrefix = "结束前的验证"

// VerifyBeforeStopConfig 是 verify-before-stop 组件的配置。模型给出最终答复时，在工作区运行 Command；
// 退出码非零或超时则把输出作为 user 消息交还模型续跑，最多 MaxAttempts 次。工作区尚无写入类工具的改动时不运行，
// 纯问答不必付出检查的代价；run_command 对文件的改动不计入。
type VerifyBeforeStopConfig struct {
	// Command 是检查命令，如 go test ./...，用 sh -c 在工作区根目录执行；与 run_command 一样只继承 workspace 的 envAllowlist 放行的环境变量。
	// 经写入类工具改动过的文件作为位置参数传入（路径与文件工具一致），命令可以用 "$@" 只检查相关部分。
	Command string `yaml:"command"`
	// MaxAttempts 是因验证失败续跑的上限，默认 3；用尽后按模型的答复结束。
	MaxAttempts int `yaml:"maxAttempts"`
}

func init() {
	Register(Spec[VerifyBeforeStopConfig]{
		Name: ComponentVerifyBeforeStop, Version: "1", Singleton: true, Requires: []string{ComponentWorkspace},
		Validate: func(c *VerifyBeforeStopConfig) error {
			if strings.TrimSpace(c.Command) == "" {
				return errors.New("command 不能为空")
			}
			if c.MaxAttempts < 0 {
				return errors.New("maxAttempts 不能为负")
			}
			if c.MaxAttempts == 0 {
				c.MaxAttempts = 3
			}
			return nil
		},
		Build: func(_ context.Context, a *Assembler, c *VerifyBeforeStopConfig) error {
			w, _, err := a.Workspace()
			if err != nil {
				return err
			}
			cfg := *c
			a.AddTurnStopping(func(ctx context.Context, s *builder.Stopping) (string, error) {
				if s.Blocked >= cfg.MaxAttempts || len(w.Changes()) == 0 {
					return "", nil
				}
				var changed []string
				for _, c := range w.Changes() {
					changed = append(changed, c.Path)
				}
				slices.Sort(changed)
				r, err := w.RunCheck(ctx, cfg.Command, changed...)
				if err != nil {
					if ctx.Err() != nil {
						return "", err
					}
					return fmt.Sprintf(verifyFeedbackPrefix+"命令 `%s` 无法运行：%v。请检查环境；无法解决时在答复中说明未经验证。", cfg.Command, err), nil
				}
				if r.ExitCode == 0 && !r.TimedOut {
					return "", nil
				}
				status := fmt.Sprintf("退出码 %d", r.ExitCode)
				if r.TimedOut {
					status = "超时"
				}
				return fmt.Sprintf(verifyFeedbackPrefix+"未通过（第 %d/%d 次）：`%s` %s。\n输出：\n%s\n请修复后再给出最终答复。失败与本次改动无关时（如改动前就失败的测试）不要修改无关代码，在答复中说明即可；确实无法修复时说明原因。",
					s.Blocked+1, cfg.MaxAttempts, cfg.Command, status, r.Output), nil
			})
			return nil
		},
	})
}
