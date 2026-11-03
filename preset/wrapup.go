package preset

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kart-io/goagent/builder"
)

// ComponentWrapUp 是收尾提示组件名。
const ComponentWrapUp = "wrap-up"

// WrapUpConfig 是 wrap-up 组件的配置。引擎在本轮最后一次请求时撤掉工具，运行接近截止时间时直接中止，
// 两种情况模型原本都不知情。组件在这两种时刻附上运行上下文，要求模型收尾并说明未完成的部分。只作用于原生工具流。
type WrapUpConfig struct {
	// WarnBefore 是距运行截止多久开始提示，默认 1m。运行没有截止时间时不提示。
	WarnBefore time.Duration `yaml:"warnBefore"`
}

const (
	finalStepNotice = "这是本轮最后一次模型请求，工具已不可用。直接给出最终答复：说明已完成的改动、尚未完成的部分和建议的下一步，不要声称做了没有做的事。"
	deadlineNotice  = "距运行截止还剩约 %s。不要再开始新的工作：保存已有改动，必要时运行一次检查，然后给出答复并说明尚未完成的部分。"
)

func init() {
	Register(Spec[WrapUpConfig]{
		Name: ComponentWrapUp, Version: "1", Singleton: true,
		Validate: func(c *WrapUpConfig) error {
			if c.WarnBefore < 0 {
				return errors.New("warnBefore 不能为负")
			}
			if c.WarnBefore == 0 {
				c.WarnBefore = time.Minute
			}
			return nil
		},
		Build: func(_ context.Context, a *Assembler, c *WrapUpConfig) error {
			warn := c.WarnBefore
			a.AddPromptContext(builder.PromptContext{Name: ComponentWrapUp, Order: 100, Text: func(s builder.PromptScope) string {
				return wrapUpNotice(s, warn, time.Now())
			}})
			return nil
		},
	})
}

func wrapUpNotice(s builder.PromptScope, warn time.Duration, now time.Time) string {
	if s.Final {
		return finalStepNotice
	}
	if s.Deadline.IsZero() {
		return ""
	}
	if left := s.Deadline.Sub(now); left <= warn {
		return fmt.Sprintf(deadlineNotice, max(left, 0).Round(time.Second))
	}
	return ""
}
