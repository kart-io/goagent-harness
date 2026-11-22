package integration_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	p "github.com/kart-io/goagent-harness/planning"
	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/store"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func coreFixture(t *testing.T, rel string) []byte {
	t.Helper()
	// 从当前 go.work、replace 或模块缓存中定位真正参与本次构建的核心仓。
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/kart-io/goagent").Output()
	require.NoError(t, err)
	dir := strings.TrimSpace(string(out))
	require.NotEmpty(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, rel))
	require.NoError(t, err)
	return data
}

type hiddenRecall struct {
	durable.Store
	id string
}

func (s hiddenRecall) Load(ctx context.Context, id string) (*durable.Snapshot, error) {
	if s.id == id {
		return nil, os.ErrNotExist
	}
	return s.Store.Load(ctx, id)
}

func TestMemoryV4PreservesLegacyPlainPlanningReplay(t *testing.T) {
	ctx := memory.WithNamespace(t.Context(), "t1", "alice")
	for _, version := range []string{"2", "3"} {
		t.Run("v"+version, func(t *testing.T) {
			fs, err := durable.NewFileStore(t.TempDir())
			require.NoError(t, err)
			scope := fs.Scope("memory:" + store.NamespaceKey([]string{"t1", "alice"}) + "#0")
			for _, f := range []struct{ file, id, prefix string }{{"head", "memory/head", "memory-head/"}, {"base", "memory/base/0", "memory-state-blob/"}} {
				data := coreFixture(t, "memory/testdata/state-v"+version+"/"+f.file+".json")
				unlock, err := scope.Lock(ctx, f.id)
				require.NoError(t, err)
				err = scope.Save(ctx, f.id, &durable.Snapshot{RunID: f.id, Plan: "memory", Version: f.prefix + version, Data: data})
				require.NoError(t, unlock())
				require.NoError(t, err)
			}
			m, err := memory.New(memory.Config{Durable: fs})
			require.NoError(t, err)
			before, err := m.Get(ctx, "r00")
			require.NoError(t, err)
			require.NotEmpty(t, before.Content)
			_, err = m.Remember(ctx, memory.Record{Content: "用户喜欢喝美式咖啡", Importance: 0.9})
			require.NoError(t, err)
			head, err := scope.Load(ctx, "memory/head")
			require.NoError(t, err)
			require.Equal(t, "memory-head/4", head.Version)
			m, err = memory.New(memory.Config{Durable: fs})
			require.NoError(t, err)
			after, err := m.Get(ctx, "r00")
			require.NoError(t, err)
			require.Equal(t, before.Content, after.Content)
			run := func(log session.Store, journal durable.Store, client llm.Client, resends int) error {
				a, e := builder.NewSimpleBuilder(client).WithSessionID("s").WithSessionLog(log).WithDurableScope("plain").WithInterruptedModelResends(resends).WithLongTermMemory(m).Build()
				require.NoError(t, e)
				_, e = a.AsRunnable().Invoke(durable.WithJournal(durable.WithExecutionKey(ctx, "one"), journal), "推荐一杯咖啡")
				return e
			}
			op := func(s string) string {
				return durable.OperationStorageID(durable.ChildKey(durable.ChildKey("one", "agent/plain"), s))
			}
			for _, which := range []string{"completed", "interrupted"} {
				t.Run(which, func(t *testing.T) {
					log := session.NewMemoryStore()
					var events []session.Event
					require.NoError(t, json.Unmarshal(coreFixture(t, "builder/testdata/legacy_plain_"+which+".json"), &events))
					_, err := log.Append(ctx, "s", 0, events...)
					require.NoError(t, err)
					journal := session.DurableStore(log, "s")
					client := mocks.NewScriptedClient(mocks.TextReply("恢复", "好"))
					require.NoError(t, run(log, journal, client, 1))
					if which == "completed" {
						require.Empty(t, client.Requests())
					} else {
						require.Len(t, client.Requests(), 1)
						require.Equal(t, []llm.Message{llm.UserMessage("推荐一杯咖啡")}, client.Requests()[0].Messages)
					}
					for _, id := range []string{"memory/recall", "memory/use/0"} {
						_, err = journal.Load(ctx, op(id))
						require.ErrorIs(t, err, os.ErrNotExist)
					}
				})
			}
			log := session.NewMemoryStore()
			journal := session.DurableStore(log, "s")
			client := mocks.NewScriptedClient(mocks.TextReply("新轮次", "好"))
			require.NoError(t, run(log, journal, client, 0))
			require.Len(t, client.Requests(), 1)
			req, err := json.Marshal(client.Requests()[0].Messages)
			require.NoError(t, err)
			require.Contains(t, string(req), "美式咖啡")
			for _, id := range []string{"memory/use/0", "model/answer/0"} {
				_, err := journal.Load(ctx, op(id))
				require.NoError(t, err)
			}
			err = run(log, hiddenRecall{journal, op("memory/recall")}, client, 0)
			require.True(t, agentErrors.IsCode(err, agentErrors.CodeStateValidation), "%v", err)
			require.Len(t, client.Requests(), 1)
		})
	}
}

func jointWindow(t *testing.T, fs durable.Store, root string, max int) *memory.BackgroundConfig {
	t.Helper()
	ctrl, err := budget.NewDurable(budget.Config{Version: "1", RootKey: root, MaxModelRequests: max, MaxReservedTokens: 65536, TokenPolicyVersion: "1"}, fs)
	require.NoError(t, err)
	return &memory.BackgroundConfig{Budget: ctrl, MaxInputRunes: 10000, MaxOutputRunes: 1000, MaxOutputTokens: 32, MaxDerivationsPerPass: 1, EstimatorVersion: "1", EstimateChat: func(*llm.CompletionRequest) (int, error) { return 64, nil }, EstimateEmbedding: func(string) (int, error) { return 8, nil }, SingleRequestClients: true}
}

type jointEmbeddings struct {
	online, background           *budget.Controller
	onlineCalls, backgroundCalls atomic.Int32
}

func (e *jointEmbeddings) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, errors.New("嵌入输入为空")
	}
	if c, ok := budget.FromContext(ctx); ok && c == e.background {
		e.backgroundCalls.Add(1)
	} else {
		if err := e.online.ReserveAttributed(ctx, "embed/"+uuid.NewString(), 8, nil); err != nil {
			return nil, err
		}
		e.onlineCalls.Add(1)
	}
	return []float32{1, 0}, nil
}

func TestPlanningAndMaintenanceBudgetsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	h := openJointHost(t, dir)
	fs, e := durable.NewFileStore(dir + "/memory")
	require.NoError(t, e)
	window := jointWindow(t, h.journal, "maintenance", 10)
	online := jointWindow(t, h.journal, "online", 3).Budget
	embed := &jointEmbeddings{online: online, background: window.Budget}
	chat := mocks.NewScriptedClient(mocks.TextReply("经历反思", `{"insights":[{"content":"烘焙影响咖啡口味"}]}`))
	mem, e := memory.New(memory.Config{Durable: fs, Index: memory.NewMemoryIndex(), EmbedModel: "local", Embedder: embed, Model: chat, Sessions: h.log, Background: window, ReflectionThreshold: .5})
	require.NoError(t, e)
	h.mem = mem
	_, e = mem.Remember(h.ctx, memory.Record{ID: "episode", Kind: memory.KindEpisode, Content: "编写咖啡报告的经历", Importance: 1})
	require.NoError(t, e)
	writes := embed.onlineCalls.Load()
	require.Positive(t, writes)
	_, e = mem.ProcessPending(h.ctx)
	require.NoError(t, e)
	require.Len(t, chat.Requests(), 1)
	require.Positive(t, embed.backgroundCalls.Load())
	require.Equal(t, writes, embed.onlineCalls.Load())
	s, _, task := jointFixture(t)
	pc := jointPlanClient(t, task)
	role, e := p.NewAgentPlanner(h.role("planner", pc))
	require.NoError(t, e)
	_, e = role.Plan(durable.WithExecutionKey(h.ctx, "plan-node"), p.PlanningInput{Goal: s.Goal})
	require.NoError(t, e)
	require.Len(t, pc.Requests(), 1)
	require.Contains(t, jointJSON(t, pc.Requests()[0].Messages), "编写咖啡报告的经历")
	require.Greater(t, embed.onlineCalls.Load(), writes)
	// 将在线窗口耗尽后，后台仍有额度，额外调用必须在客户端前被拒绝。
	query := memory.Query{Text: "咖啡报告"}
	for i := int(embed.onlineCalls.Load()); i < online.Configuration().MaxModelRequests; i++ {
		results, err := mem.Recall(h.ctx, query)
		require.NoError(t, err)
		require.NotEmpty(t, results)
	}
	prior := embed.onlineCalls.Load()
	beforeChat, beforeEmbed := chat.Calls(), embed.backgroundCalls.Load()
	backgroundBefore, e := window.Budget.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Less(t, backgroundBefore.ReservedRequests, window.Budget.Configuration().MaxModelRequests)
	onlineBefore, e := online.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, online.Configuration().MaxModelRequests, onlineBefore.ReservedRequests)
	results, e := mem.Recall(h.ctx, query)
	require.ErrorIs(t, e, agentErrors.ErrModelBudgetExceeded)
	require.Empty(t, results)
	require.Equal(t, prior, embed.onlineCalls.Load())
	require.Equal(t, beforeChat, chat.Calls())
	require.Equal(t, beforeEmbed, embed.backgroundCalls.Load())
	onlineAfter, e := online.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, onlineBefore, onlineAfter)
	backgroundAfter, e := window.Budget.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, backgroundBefore, backgroundAfter)
	for _, pair := range []struct {
		c *budget.Controller
		n int
	}{{h.controller, pc.Calls()}, {online, int(embed.onlineCalls.Load())}, {window.Budget, chat.Calls() + int(embed.backgroundCalls.Load())}} {
		reopened, e := budget.NewDurable(pair.c.Configuration(), h.journal)
		require.NoError(t, e)
		snap, e := reopened.Snapshot(h.ctx)
		require.NoError(t, e)
		require.Equal(t, pair.n, snap.ReservedRequests)
		if pair.c == online {
			require.Equal(t, pair.n, snap.UnknownUsageEntries)
			require.Equal(t, 8*pair.n, snap.ReservedTokens)
		}
	}
}

// Read the reflection entry from the committed /4 head, falling back to its
// referenced base if compaction has moved the entry out of the overlay.
func persistedReflection(t *testing.T, ctx context.Context, fs durable.Scoped, scopeID string) memory.DerivationWork {
	t.Helper()
	ns := store.NamespaceKey(memory.NamespaceFrom(ctx))
	scope := fs.Scope("memory:" + ns + "#" + scopeID)
	snap, err := scope.Load(ctx, "memory/head")
	require.NoError(t, err)
	require.Equal(t, "memory-head/4", snap.Version)
	var head struct {
		Overlay string `json:"overlay"`
		Base    struct {
			Slot     int    `json:"slot"`
			CommitID string `json:"commit_id"`
		} `json:"base"`
	}
	require.NoError(t, json.Unmarshal(snap.Data, &head))
	decoder, err := zstd.NewReader(nil)
	require.NoError(t, err)
	defer decoder.Close()
	decode := func(encoded string, dst any) {
		t.Helper()
		packed, err := base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		plain, err := decoder.DecodeAll(packed, nil)
		require.NoError(t, err)
		require.NoError(t, gob.NewDecoder(bytes.NewReader(plain)).Decode(dst))
	}
	if head.Overlay != "" {
		var overlay struct {
			Derivations map[string]struct {
				Deleted bool
				Value   memory.DerivationWork
			}
		}
		decode(head.Overlay, &overlay)
		if change, ok := overlay.Derivations["reflection"]; ok {
			require.False(t, change.Deleted)
			return change.Value
		}
	}
	require.NotEmpty(t, head.Base.CommitID, "reflection missing from persisted head")
	base, err := scope.Load(ctx, fmt.Sprintf("memory/base/%d", head.Base.Slot))
	require.NoError(t, err)
	require.Equal(t, "memory-state-blob/4", base.Version)
	var parts []json.RawMessage
	require.NoError(t, json.Unmarshal(base.Data, &parts))
	require.Greater(t, len(parts), 1)
	var header struct {
		CommitID string `json:"commit_id"`
	}
	require.NoError(t, json.Unmarshal(parts[0], &header))
	require.Equal(t, head.Base.CommitID, header.CommitID)
	for _, part := range parts[1:] {
		var encoded string
		require.NoError(t, json.Unmarshal(part, &encoded))
		var chunk struct {
			Derivations map[string]memory.DerivationWork
		}
		decode(encoded, &chunk)
		if work, ok := chunk.Derivations["reflection"]; ok {
			return work
		}
	}
	t.Fatal("reflection missing from committed /4 base")
	return memory.DerivationWork{}
}

func TestMaintenanceWindowChangeDoesNotResetFailureAttempts(t *testing.T) {
	dir := t.TempDir()
	h := openJointHost(t, dir)
	fs, e := durable.NewFileStore(dir + "/maintenance-memory")
	require.NoError(t, e)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fail := mocks.ScriptedRule{Name: "模型技术故障", Respond: func(context.Context, *llm.CompletionRequest) (mocks.ScriptedReply, error) {
		return mocks.ScriptedReply{}, errors.New("维护客户端失败")
	}}
	client := mocks.NewScriptedClient(fail, fail, fail, fail, fail, fail)
	open := func(root string) (*memory.Memory, *budget.Controller) {
		w := jointWindow(t, h.journal, root, 20)
		m, e := memory.New(memory.Config{Durable: fs, Model: client, Sessions: h.log, Background: w, ReflectionThreshold: .5, Now: func() time.Time { return now }})
		require.NoError(t, e)
		return m, w.Budget
	}
	m, first := open("first")
	_, e = m.Remember(h.ctx, memory.Record{ID: "e", Kind: memory.KindEpisode, Content: "一次咖啡经历", Importance: 1})
	require.NoError(t, e)
	effectCtx := durable.WithExecutionKey(h.ctx, "write-attempt")
	writes := 0
	write := func() error {
		_, e := durable.Do(effectCtx, "publish", []byte("{}"), durable.OperationPolicy{MaxAttempts: 1}, func(context.Context) (json.RawMessage, error) { writes++; return nil, agentErrors.ErrEffectUnknown })
		return e
	}
	require.Error(t, write())
	require.Equal(t, 1, writes)
	// 先真实消费前台请求，再比较两个维护窗口均未修改其账本。
	s, _, task := jointFixture(t)
	pc := jointPlanClient(t, task)
	role, e := p.NewAgentPlanner(h.role("planner", pc))
	require.NoError(t, e)
	_, e = role.Plan(durable.WithExecutionKey(h.ctx, "foreground"), p.PlanningInput{Goal: s.Goal})
	require.NoError(t, e)
	require.Len(t, pc.Requests(), 1)
	fg, e := h.controller.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, 1, fg.ReservedRequests)
	for i := 1; i <= 5; i++ {
		_, e = m.ProcessPending(h.ctx)
		require.Error(t, e, "第%d次", i)
		require.Equal(t, i, client.Calls())
		_, e = m.ProcessPending(h.ctx)
		require.NoError(t, e)
		require.Equal(t, i, client.Calls())
		now = now.Add(time.Second << uint(i-1))
	}
	turn, e := m.Turn(h.ctx)
	require.NoError(t, e)
	before := persistedReflection(t, h.ctx, fs, turn.Scope)
	require.Greater(t, before.DirtySeq, before.DoneSeq)
	require.Equal(t, 5, before.Attempts)
	require.True(t, before.Paused)
	m, second := open("second")
	now = now.Add(30 * 24 * time.Hour)
	_, e = m.ProcessPending(h.ctx)
	require.NoError(t, e)
	require.Equal(t, 5, client.Calls())
	afterSwitch := persistedReflection(t, h.ctx, fs, turn.Scope)
	require.Greater(t, afterSwitch.DirtySeq, afterSwitch.DoneSeq)
	require.Equal(t, 5, afterSwitch.Attempts)
	require.True(t, afterSwitch.Paused)
	require.Equal(t, before, afterSwitch)
	for _, v := range []struct {
		c     *budget.Controller
		count int
	}{{first, 5}, {second, 0}} {
		snap, e := v.c.Snapshot(h.ctx)
		require.NoError(t, e)
		require.Equal(t, v.count, snap.ReservedRequests, v.c.Configuration().RootKey)
	}
	after, e := h.controller.Snapshot(h.ctx)
	require.NoError(t, e)
	require.Equal(t, fg, after)
	require.ErrorIs(t, write(), agentErrors.ErrOperationAttention)
	require.Equal(t, 1, writes)
}
