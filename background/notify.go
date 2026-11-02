package background

import (
	"context"
	"errors"
	"fmt"

	"github.com/kart-io/goagent-harness/effects"
	"github.com/kart-io/goagent/builder"
)

// NotifyHandle 返回把通知投递给所有者 Handle 的 Config.Notify。lookup 返回所有者当前的 Handle，没有时返回 nil。
// 轮次全部结束后 Handle 不再接收输入，也不会自行开启新轮次：这时调用 idle，由宿主决定如何继续（如用结果开启新一轮）；
// idle 为 nil 或没有 Handle 时稍后重试，结果始终可用 Manager.Get 查询。
func NotifyHandle(lookup func(owner string) *builder.Handle, idle func(context.Context, Notice) error) func(context.Context, Notice) error {
	return func(ctx context.Context, notice Notice) error {
		if h := lookup(notice.Owner); h != nil {
			err := h.Steer(noticeText(notice))
			if err == nil || !errors.Is(err, builder.ErrHandleDone) {
				return err
			}
		}
		if idle != nil {
			return idle(ctx, notice)
		}
		return effects.ErrLater
	}
}

func noticeText(notice Notice) string {
	text := fmt.Sprintf("后台子工作 %s 已结束，状态 %s", notice.ChildID, notice.Status)
	if notice.FailureKind != "" {
		text += "，失败分类 " + notice.FailureKind
	}
	if notice.Result != "" {
		text += "。结果：\n" + notice.Result
	}
	if notice.ResultTruncated {
		text += "\n（结果已截断）"
	}
	return text
}
