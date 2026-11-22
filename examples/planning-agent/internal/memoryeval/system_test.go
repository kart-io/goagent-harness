package memoryeval

import (
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/memory/memeval"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

func TestLayoutCopiesAndForget(t *testing.T) {
	ctx := t.Context()
	c := memeval.Case{ID: "case", Sessions: []memeval.Session{{ID: "s", Date: time.Now(), Turns: []llm.Message{llm.UserMessage("original history")}}}}
	cfg := memory.Config{RecallRunes: 400}
	dir := t.TempDir()
	source, err := openMemory(dir+"/formed", cfg)
	require.NoError(t, err)
	_, err = source.Remember(memory.WithNamespace(ctx, c.ID), memory.Record{ID: "pin", Content: "coffee secret", Pinned: true})
	require.NoError(t, err)
	pair, err := copyLayouts(dir, c, cfg, memory.ContextLayout{})
	require.NoError(t, err)
	q := memeval.Question{Text: "coffee"}
	old, err := pair[0].Context(ctx, q)
	require.NoError(t, err)
	require.Contains(t, old.Text, "coffee secret")
	fresh, err := pair[1].Context(ctx, q)
	require.NoError(t, err)
	require.NotContains(t, fresh.Text, "coffee secret")
	fg := mocks.NewScriptedClient(mocks.TextReply("前", "a"), mocks.TextReply("后", "b"))
	judge := mocks.NewScriptedClient(mocks.TextReply("判分", "no"), mocks.TextReply("判分", "no"))
	clients := Clients{Foreground: fg, Judge: judge}
	row := ask(ctx, filepath.Join(dir, "before"), Key{}, pair[0], c, q, DefaultConfig(), clients)
	require.Equal(t, "completed", row.Status)
	require.NoError(t, pair[0].mem.Forget(memory.WithNamespace(ctx, c.ID), "pin"))
	got, err := pair[0].Context(ctx, q)
	require.NoError(t, err)
	require.NotContains(t, got.Text, "coffee secret")
	row = ask(ctx, filepath.Join(dir, "after"), Key{}, pair[0], c, q, DefaultConfig(), clients)
	require.Equal(t, "completed", row.Status)
	requests := fg.Requests()
	require.Len(t, requests, 2)
	require.True(t, mocks.RequestContains("coffee secret")(&requests[0]))
	require.False(t, mocks.RequestContains("coffee secret")(&requests[1]))
	original, err := source.Retrieve(memory.WithNamespace(ctx, c.ID), "coffee")
	require.NoError(t, err)
	require.Contains(t, original.Text, "coffee secret")
}
