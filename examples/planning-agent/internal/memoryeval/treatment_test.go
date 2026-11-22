package memoryeval

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/stretchr/testify/require"
)

func storeFiles(t *testing.T, path string) map[string]string {
	t.Helper()
	files := map[string]string{}
	tree := os.DirFS(path)
	require.NoError(t, fs.WalkDir(tree, ".", func(name string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(tree, name)
		files[name] = string(data)
		return err
	}))
	return files
}
func TestFormedCoreChangesPhysicalPrompt(t *testing.T) {
	for _, tt := range []struct {
		label     string
		core      int
		different bool
	}{{"excluded", 0, true}, {"fits", 512, false}} {
		t.Run(tt.label, func(t *testing.T) {
			cases, c := fixture()
			c.CoreRunes = tt.core
			extract := mocks.NewScriptedClient(mocks.TextReply("抽取", `{"memories":[{"kind":"fact","content":"coffee formed core","importance":0.8}]}`))
			fg := mocks.NewScriptedClient(mocks.TextReply("作答", "a"), mocks.TextReply("作答", "b"), mocks.TextReply("作答", "c"))
			judge := mocks.NewScriptedClient(mocks.TextReply("判分", "no"), mocks.TextReply("判分", "no"), mocks.TextReply("判分", "no"))
			dir := filepath.Join(t.TempDir(), "out")
			summary, err := Run(t.Context(), dir, cases, c, &Clients{extract, fg, judge})
			require.NoError(t, err)
			require.Equal(t, 3, summary.Completed)
			require.Equal(t, 1, extract.Calls())
			reqs := fg.Requests()
			require.Len(t, reqs, 3)
			require.True(t, mocks.RequestContains("coffee formed core")(&reqs[1]))
			require.Equal(t, !tt.different, mocks.RequestContains("coffee formed core")(&reqs[2]))
			source := storeFiles(t, filepath.Join(dir, "r1-c0", "formed"))
			require.NotEmpty(t, source)
			for _, variant := range []string{"legacy", "layout"} {
				require.Equal(t, source, storeFiles(t, filepath.Join(dir, "r1-c0", variant)))
			}
			data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			require.NoError(t, err)
			var manifest map[string]any
			require.NoError(t, json.Unmarshal(data, &manifest))
			require.NotEmpty(t, manifest["pin_policy"])
			var obs []struct {
				Pinned    int    `json:"pinned_records"`
				Compared  int    `json:"compared_questions"`
				Changed   int    `json:"changed_contexts"`
				Treatment string `json:"treatment"`
			}
			data, err = json.Marshal(manifest["formation"])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &obs))
			require.Len(t, obs, 1)
			require.Equal(t, 1, obs[0].Pinned)
			require.Equal(t, 1, obs[0].Compared)
			if tt.different {
				require.Equal(t, 1, obs[0].Changed)
				require.Equal(t, "changed_context", obs[0].Treatment)
			} else {
				require.Zero(t, obs[0].Changed)
				require.Equal(t, "no_layout_effect", obs[0].Treatment)
				// 前一请求已选中核心记录；在同一新布局存储执行真实遗忘后重新作答。
				current, err := openMemory(filepath.Join(dir, "r1-c0", "layout"), memory.Config{RecallRunes: c.Runes, ContextLayout: &memory.ContextLayout{CoreRunes: c.CoreRunes}})
				require.NoError(t, err)
				ns := memory.WithNamespace(t.Context(), cases[0].ID)
				pins, err := current.Pinned(ns)
				require.NoError(t, err)
				require.Len(t, pins, 1)
				require.NoError(t, current.Forget(ns, pins[0].ID))
				after := mocks.NewScriptedClient(mocks.TextReply("遗忘后作答", "unknown"))
				grade := mocks.NewScriptedClient(mocks.TextReply("判分", "no"))
				row := ask(t.Context(), filepath.Join(dir, "after-forget"), Key{}, layoutSystem{"layout", cases[0].ID, current}, cases[0], cases[0].Questions[0], c, Clients{Foreground: after, Judge: grade})
				require.Equal(t, "completed", row.Status)
				afterRequests := after.Requests()
				require.Len(t, afterRequests, 1)
				require.False(t, mocks.RequestContains("coffee formed core")(&afterRequests[0]))

			}
		})
	}
}
func TestInvalidLayoutRejectedBeforePlanOrFormation(t *testing.T) {
	cases, c := fixture()
	c.CoreRunes = c.Runes
	cl := mocks.NewScriptedClient()
	for _, clients := range []*Clients{nil, {cl, cl, cl}} {
		dir := filepath.Join(t.TempDir(), "out")
		_, err := Run(t.Context(), dir, cases, c, clients)
		require.Error(t, err)
		require.NoDirExists(t, dir)
	}
	require.Zero(t, cl.Calls())
}
