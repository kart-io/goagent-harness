package preset

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kart-io/goagent/builder"
	"github.com/stretchr/testify/require"
)

// 最后一次请求撤掉了工具，模型收到收尾提示；之前的请求没有。
func TestWrapUpNoticesFinalStep(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, codingPreset(`, {id: wrap, use: wrap-up}`)), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	model := &scriptModel{turns: []scriptTurn{call("c1", "glob", `{"pattern":"*"}`), say("已完成部分，测试未运行")}}
	agent, err := Configure(builder.NewSimpleBuilder(model), asm).WithSessionID("s").WithMaxIterations(2).Build()
	require.NoError(t, err)
	_, err = agent.AsRunnable().Invoke(context.Background(), "改代码")
	require.NoError(t, err)
	require.NotContains(t, joined(model.request(0)), finalStepNotice)
	last := model.request(1)[len(model.request(1))-1]
	require.Equal(t, "user", last.Role)
	require.Contains(t, last.Content, finalStepNotice)
}

func TestWrapUpNoticeTiming(t *testing.T) {
	now := time.Unix(1000, 0)
	require.Empty(t, wrapUpNotice(builder.PromptScope{}, time.Minute, now), "没有截止时间不提示")
	require.Empty(t, wrapUpNotice(builder.PromptScope{Deadline: now.Add(2 * time.Minute)}, time.Minute, now))
	notice := wrapUpNotice(builder.PromptScope{Deadline: now.Add(45*time.Second + 300*time.Millisecond)}, time.Minute, now)
	require.True(t, strings.HasPrefix(notice, "距运行截止还剩约 45s。"), notice)
	require.Contains(t, wrapUpNotice(builder.PromptScope{Deadline: now.Add(-time.Second)}, time.Minute, now), "还剩约 0s")
	require.Equal(t, finalStepNotice, wrapUpNotice(builder.PromptScope{Final: true, Deadline: now.Add(time.Second)}, time.Minute, now), "最后一步优先")
}

func TestWrapUpRejectsNegativeWarnBefore(t *testing.T) {
	_, err := Load(codingPreset(`, {id: wrap, use: wrap-up, config: {warnBefore: -1s}}`))
	require.ErrorContains(t, err, "warnBefore")
}

// 组件贡献的 Request 钩子经 Configure 生效，每步请求前执行。
func TestConfigureWiresRequestHooks(t *testing.T) {
	asm, err := Build(context.Background(), mustGet(t, codingPreset("")), Host{Root: t.TempDir(), ArtifactDir: t.TempDir()})
	require.NoError(t, err)
	defer func() { require.NoError(t, asm.Close()) }()

	var steps []int
	var finals []bool
	asm.Loop.Request = append(asm.Loop.Request, func(_ context.Context, r *builder.ModelRequest) error {
		steps = append(steps, r.Step)
		finals = append(finals, r.Final)
		return nil
	})
	model := &scriptModel{turns: []scriptTurn{call("c1", "glob", `{"pattern":"*"}`), say("完成")}}
	agent, err := Configure(builder.NewSimpleBuilder(model), asm).WithSessionID("s").WithMaxIterations(2).Build()
	require.NoError(t, err)
	_, err = agent.AsRunnable().Invoke(context.Background(), "做点事")
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, steps)
	require.Equal(t, []bool{false, true}, finals)
}
