package planning

import (
	"context"
	"errors"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/codec/json"
	"github.com/kart-io/goagent/constants"
	"github.com/kart-io/goagent/core"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/kart-io/goagent/tools"
	"github.com/kart-io/goagent/workflow"
	"github.com/stretchr/testify/require"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAgentAdaptersRequireFactory(t *testing.T) {
	if _, err := NewAgentPlanner(AgentConfig{Version: "1"}); err == nil {
		t.Fatal("缺少工厂应失败")
	}
	if _, err := NewAgentGoalResolver(AgentConfig{Version: "1"}); err == nil {
		t.Fatal("缺少工厂应失败")
	}
	if _, err := NewAgentReviewer(AgentConfig{Version: "1"}); err == nil {
		t.Fatal("缺少工厂应失败")
	}
}

type adapterFixture struct {
	ctx            context.Context
	store          durable.Store
	budget         *budget.Controller
	log            session.Store
	opened, closed atomic.Int32
}

func newAdapterFixture(t *testing.T) *adapterFixture {
	t.Helper()
	store, err := durable.NewFileStore(t.TempDir())
	require.NoError(t, err)
	log, err := session.NewFileStore(t.TempDir())
	require.NoError(t, err)
	controller, err := budget.NewDurable(budget.Config{RootKey: "run", Version: "1", MaxModelRequests: 9}, store)
	require.NoError(t, err)
	ctx := budget.WithController(durable.WithJournal(durable.WithExecutionKey(t.Context(), "node"), store), controller)
	return &adapterFixture{ctx: ctx, store: store, budget: controller, log: log}
}
func (f *adapterFixture) config(model *mocks.ScriptedClient, mem *memory.Memory) AgentConfig {
	return AgentConfig{Version: "1", Factory: func(ctx context.Context) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
		var owned tools.OwnedRunnable[any, *builder.AgentOutput]
		f.opened.Add(1)
		b := builder.NewSimpleBuilder(model).WithAgentID("role").WithDurableScope("role").WithSessionID("session").WithSessionLog(f.log)
		if mem != nil {
			b = b.WithLongTermMemory(mem)
		}
		a, err := b.Build()
		if err != nil {
			return owned, err
		}
		owned.Runnable = a.AsRunnable()
		owned.Close = func(context.Context) error { f.closed.Add(1); return nil }
		return owned, nil
	}}
}
func TestResolverReplayDoesNotCallModel(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		reject         bool
	}{
		{"有效", `{"candidate":{"objective":"测试"},"questions":[]}`, false}, {"格式错误", `{"questions":[],"unexpected":1}`, true}, {"伪造来源", `{"candidate":{"inputs":[{"id":"fake","content":42}]}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdapterFixture(t)
			model := mocks.NewScriptedClient(mocks.TextReply("解析", tc.response))
			for range 2 {
				a, err := NewAgentGoalResolver(f.config(model, nil))
				require.NoError(t, err)
				out, err := a.Resolve(f.ctx, GoalResolveInput{Round: 1})
				if tc.reject {
					var candidate *CandidateError
					require.ErrorAs(t, err, &candidate)
				} else {
					require.NoError(t, err)
					require.Equal(t, "测试", out.Candidate.Objective)
				}
			}
			require.Equal(t, 1, model.Calls())
			require.EqualValues(t, 1, f.opened.Load())
			require.Equal(t, f.opened.Load(), f.closed.Load())
		})
	}
}
func TestReviewerSubjectDigestMustMatchFrozenAttempt(t *testing.T) {
	f := newAdapterFixture(t)
	a, err := NewAgentReviewer(f.config(mocks.NewScriptedClient(mocks.TextReply("复核", `{"subject_digest":"other","verdict":"accept"}`)), nil))
	require.NoError(t, err)
	_, err = a.Review(f.ctx, ReviewInput{SubjectDigest: "frozen", Assignment: &Assignment{}})
	var candidate *CandidateError
	require.ErrorAs(t, err, &candidate)
}
func TestPlannerPlainMemoryIsActuallyInjectedThenRevoked(t *testing.T) {
	f := newAdapterFixture(t)
	ctx := memory.WithNamespace(f.ctx, "org", "user")
	m, err := memory.New(memory.Config{Durable: f.store})
	require.NoError(t, err)
	const private = "咖啡偏好是美式无糖"
	record, err := m.Remember(ctx, memory.Record{Content: private, Importance: 0.9})
	require.NoError(t, err)
	model := mocks.NewScriptedClient(mocks.TextReply("计划一", "{}"), mocks.TextReply("计划二", "{}"))
	a, err := NewAgentPlanner(f.config(model, m))
	require.NoError(t, err)
	input := PlanningInput{Goal: GoalSpec{Objective: "推荐咖啡"}}
	for i, key := range []string{"one", "two"} {
		_, err = a.Plan(durable.WithExecutionKey(ctx, key), input)
		require.NoError(t, err)
		var recall string
		for _, msg := range model.Requests()[i].Messages {
			if strings.HasPrefix(msg.Content, memory.RecallPrefix) {
				recall = msg.Content
			} else {
				require.NotContains(t, msg.Content, private)
			}
		}
		if i == 0 {
			require.Contains(t, recall, private)
			require.NoError(t, m.Forget(ctx, record.ID))
		} else {
			require.Empty(t, recall)
		}
	}
	require.EqualValues(t, 2, f.closed.Load())
}

type modelCrashStore struct {
	durable.Store
	fail func() bool
}

var errAdapterCrash = errors.New("模拟进程中断")

func (s modelCrashStore) Save(ctx context.Context, id string, snap *durable.Snapshot) error {
	if s.fail() {
		return errAdapterCrash
	}
	return s.Store.Save(ctx, id, snap)
}
func TestModelInterruptDefaultNeedsAttention(t *testing.T) {
	f := newAdapterFixture(t)
	model := mocks.NewScriptedClient(mocks.TextReply("中断", "{}"))
	a, err := NewAgentPlanner(f.config(model, nil))
	require.NoError(t, err)
	crash := true
	ctx := durable.WithJournal(f.ctx, modelCrashStore{Store: f.store, fail: func() bool { return crash && model.Calls() > 0 }})
	_, err = a.Plan(ctx, PlanningInput{})
	require.ErrorIs(t, err, errAdapterCrash)
	crash = false
	_, err = a.Plan(ctx, PlanningInput{})
	require.ErrorIs(t, err, agentErrors.ErrOperationAttention)
	require.Equal(t, 1, model.Calls())
}
func TestPlannerReplayIgnoresLiveBudgetChanges(t *testing.T) {
	f := newAdapterFixture(t)
	model := mocks.NewScriptedClient(mocks.TextReply("计划", `{"rationale":"已完成"}`))
	initial := PlanningInput{Goal: GoalSpec{Objective: "冻结的目标"}}
	build := func(crash bool) *workflow.DurableGraph[PlanningInput] {
		a, err := NewAgentPlanner(f.config(model, nil))
		require.NoError(t, err)
		g := workflow.NewGraph[PlanningInput](workflow.Config{})
		require.NoError(t, g.AddNode("plan", func(ctx context.Context, in PlanningInput) (PlanningInput, error) {
			draft, err := a.Plan(ctx, in)
			if err != nil {
				return in, err
			}
			if crash {
				return in, errAdapterCrash
			}
			in.RejectionReason = draft.Rationale
			return in, nil
		}))
		require.NoError(t, g.AddEdge(constants.WorkflowStart, "plan"))
		require.NoError(t, g.AddEdge("plan", constants.WorkflowEnd))
		dg, err := g.CompileDurable(f.ctx, workflow.DurableConfig{Store: f.store, Version: "graph-1", StateVersion: "state-1"})
		require.NoError(t, err)
		return dg
	}
	_, err := build(true).Run(f.ctx, "run", initial, "")
	require.ErrorIs(t, err, errAdapterCrash)
	require.Equal(t, 1, model.Calls())
	require.NoError(t, f.budget.Reserve(f.ctx, "另一个模型请求"))
	done, err := build(false).Run(f.ctx, "run", initial, "")
	require.NoError(t, err)
	require.True(t, done.Done)
	require.Equal(t, "已完成", done.State.RejectionReason)
	require.Equal(t, 1, model.Calls())
	require.EqualValues(t, 1, f.opened.Load())
}

type malformedAgentStream struct {
	core.Runnable[any, *builder.AgentOutput]
	chunks  []core.StreamChunk[*builder.AgentOutput]
	drained *atomic.Bool
}

func (s *malformedAgentStream) Stream(ctx context.Context, _ any) (<-chan core.StreamChunk[*builder.AgentOutput], error) {
	ch := make(chan core.StreamChunk[*builder.AgentOutput])
	go func() {
		defer close(ch)
		for _, c := range s.chunks {
			ch <- c
		}
		s.drained.Store(true)
	}()
	return ch, nil
}
func TestAgentSessionRejectsBrokenStreamsAndCloses(t *testing.T) {
	done := core.StreamChunk[*builder.AgentOutput]{Done: true, Data: &builder.AgentOutput{Result: "{}"}}
	for _, tc := range []struct {
		name   string
		stream *malformedAgentStream
	}{
		{"类型化空值", nil}, {"缺少结束", &malformedAgentStream{}}, {"结束后还有数据", &malformedAgentStream{chunks: []core.StreamChunk[*builder.AgentOutput]{done, done, done}}}, {"空输出", &malformedAgentStream{chunks: []core.StreamChunk[*builder.AgentOutput]{{Done: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdapterFixture(t)
			if tc.stream != nil {
				tc.stream.drained = new(atomic.Bool)
			}
			var closed atomic.Bool
			cfg := AgentConfig{Version: "1", Factory: func(ctx context.Context) (tools.OwnedRunnable[any, *builder.AgentOutput], error) {
				return tools.OwnedRunnable[any, *builder.AgentOutput]{Runnable: tc.stream, Close: func(context.Context) error {
					if tc.stream != nil && !tc.stream.drained.Load() {
						return errors.New("尚未排空")
					}
					closed.Store(true)
					return nil
				}}, nil
			}}
			a, err := NewAgentPlanner(cfg)
			require.NoError(t, err)
			_, err = a.Plan(f.ctx, PlanningInput{})
			require.Error(t, err)
			require.True(t, closed.Load())
			var candidate *CandidateError
			require.NotErrorAs(t, err, &candidate)
		})
	}
}
func TestResolverAcceptsOnlyUnmodifiedTrustedAnswerInput(t *testing.T) {
	f := newAdapterFixture(t)
	body := json.RawMessage(`{"value":9007199254740993}`)
	digest, err := SourceDigest(body)
	require.NoError(t, err)
	trusted := SourceInput{ID: "report", Origin: "user", Digest: digest, Content: body}
	input := GoalResolveInput{Answers: []GoalAnswer{{Inputs: []SourceInput{trusted}}}}
	for _, tc := range []struct {
		name   string
		edit   func(*SourceInput)
		accept bool
	}{
		{"原样引用", func(*SourceInput) {}, true}, {"篡改正文", func(s *SourceInput) { s.Content = json.RawMessage("42") }, false}, {"篡改来源地址", func(s *SourceInput) { s.Origin = "model" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := trusted
			tc.edit(&selected)
			raw, err := json.Marshal(GoalResolution{Candidate: GoalSpec{Inputs: []SourceInput{selected}}})
			require.NoError(t, err)
			model := mocks.NewScriptedClient(mocks.TextReply("选择", string(raw)))
			a, err := NewAgentGoalResolver(f.config(model, nil))
			require.NoError(t, err)
			out, err := a.Resolve(durable.WithExecutionKey(f.ctx, tc.name), input)
			if tc.accept {
				require.NoError(t, err)
				require.Equal(t, trusted, out.Candidate.Inputs[0])
			} else {
				var candidate *CandidateError
				require.ErrorAs(t, err, &candidate)
			}
		})
	}
}
