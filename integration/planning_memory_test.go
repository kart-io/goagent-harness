package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	agentErrors "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/store"
	"github.com/kart-io/goagent/testing/mocks"
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
