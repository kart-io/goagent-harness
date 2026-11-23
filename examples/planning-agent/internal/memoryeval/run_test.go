package memoryeval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	ae "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory/memeval"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/stretchr/testify/require"
)

func fixture() ([]memeval.Case, Config) {
	cfg := DefaultConfig()
	cfg.Repeats = 1
	return []memeval.Case{{ID: "customer", Sessions: []memeval.Session{{ID: "s", Date: time.Unix(100, 0), Turns: []llm.Message{llm.UserMessage("coffee fact")}}}, Questions: []memeval.Question{{ID: "q", Text: "coffee?", Answer: "judge-only-rubric", Evidence: []string{"s"}}}}}, cfg
}
func readRows(t *testing.T, dir string) []Row {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "results.jsonl"))
	require.NoError(t, err)
	defer f.Close()
	d := json.NewDecoder(f)
	var rows []Row
	for d.More() {
		var r Row
		require.NoError(t, d.Decode(&r))
		rows = append(rows, r)
	}
	return rows
}
func failure(err error) mocks.ScriptedRule {
	return mocks.ScriptedRule{Name: "失败", Respond: func(context.Context, *llm.CompletionRequest) (mocks.ScriptedReply, error) {
		return mocks.ScriptedReply{}, err
	}}
}
func TestSharedFormationRetryAndGradingFailure(t *testing.T) {
	cases, c := fixture()
	dir := filepath.Join(t.TempDir(), "out")
	extract := mocks.NewScriptedClient(mocks.TextReply("抽取", `{"memories":[{"kind":"fact","content":"coffee fact","importance":0.8}]}`))
	fg := mocks.NewScriptedClient(failure(ae.New(ae.CodeLLMRateLimit, "限流")), mocks.TextReply("作答", "first answer"), mocks.TextReply("作答", "second answer"), mocks.TextReply("作答", "third answer"))
	judge := mocks.NewScriptedClient(failure(errors.New("判分失败")), mocks.TextReply("评分", "yes"), mocks.TextReply("评分", "no"))
	s, err := Run(t.Context(), dir, cases, c, &Clients{extract, fg, judge})
	require.NoError(t, err)
	require.Equal(t, 3, s.Planned)
	require.Equal(t, 2, s.Completed)
	require.Equal(t, 1, s.Failed)
	require.Equal(t, 1, s.Correct)
	require.Len(t, s.Formation, 1)
	require.Equal(t, 1, extract.Calls())
	require.Equal(t, 1, s.Formation[0].Cost.Ledger)
	rows := readRows(t, dir)
	require.Len(t, rows, 3)
	require.Equal(t, "grading_failed", rows[0].Status)
	require.Equal(t, "first answer", rows[0].Answer)
	require.Equal(t, 2, rows[0].Foreground.Ledger)
	require.Equal(t, 1, rows[0].Judge.Ledger)
	for _, r := range rows {
		require.Equal(t, r.Foreground.Physical, r.Foreground.Ledger)
		require.Equal(t, r.Judge.Physical, r.Judge.Ledger)
	}
	for _, req := range append(fg.Requests(), extract.Requests()...) {
		require.False(t, mocks.RequestContains("judge-only-rubric")(&req))
	}
	for _, req := range judge.Requests() {
		require.True(t, mocks.RequestContains("judge-only-rubric")(&req))
	}
	require.Equal(t, 4, fg.Calls())
}
func TestIngestFailureAndCancellationKeepDenominator(t *testing.T) {
	cases, c := fixture()
	dir := filepath.Join(t.TempDir(), "out")
	cl := Clients{mocks.NewScriptedClient(failure(errors.New("灌入失败"))), mocks.NewScriptedClient(mocks.TextReply("作答", "a")), mocks.NewScriptedClient(mocks.TextReply("评分", "no"))}
	s, err := Run(t.Context(), dir, cases, c, &cl)
	require.NoError(t, err)
	require.Equal(t, 3, s.Planned)
	require.Equal(t, 2, s.Failed)
	require.Equal(t, "ingest_failed", readRows(t, dir)[1].Status)
	ctx, stop := context.WithCancel(t.Context())
	stop()
	dir = filepath.Join(t.TempDir(), "cancel")
	s, err = Run(ctx, dir, cases, c, &cl)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 3, s.NotRun)
	require.Len(t, readRows(t, dir), 3)
}

func TestDuplicateSessionIDsRejectedBeforeFormation(t *testing.T) {
	for _, id := range []string{"", "s"} {
		t.Run("拒绝会话键_"+id, func(t *testing.T) {
			cases, c := fixture()
			cases[0].Sessions = append(cases[0].Sessions, memeval.Session{ID: id, Date: time.Unix(110, 0), Turns: []llm.Message{llm.UserMessage("second session tea")}})
			forms, fore, judge := mocks.NewScriptedClient(), mocks.NewScriptedClient(), mocks.NewScriptedClient()
			for _, clients := range []*Clients{nil, {forms, fore, judge}} {
				out := filepath.Join(t.TempDir(), "out")
				s, err := Run(t.Context(), out, cases, c, clients)
				require.Error(t, err)
				require.Zero(t, s.Planned)
				_, err = os.Stat(out)
				require.ErrorIs(t, err, os.ErrNotExist)
				require.Zero(t, forms.Calls())
				require.Zero(t, fore.Calls())
				require.Zero(t, judge.Calls())
			}
		})
	}
	t.Run("不同会话正常形成", func(t *testing.T) {
		cases, c := fixture()
		cases[0].Sessions = append(cases[0].Sessions, memeval.Session{ID: "second", Date: time.Unix(110, 0), Turns: []llm.Message{llm.UserMessage("second session tea")}})
		forms := mocks.NewScriptedClient(mocks.TextReply("形成一", `{"memories":[{"kind":"fact","content":"coffee fact","importance":0.8}]}`), mocks.TextReply("形成二", `{"memories":[]}`))
		fore := mocks.NewScriptedClient(mocks.TextReply("作答一", "a"), mocks.TextReply("作答二", "a"), mocks.TextReply("作答三", "a"))
		judge := mocks.NewScriptedClient(mocks.TextReply("判分一", "no"), mocks.TextReply("判分二", "no"), mocks.TextReply("判分三", "no"))
		s, err := Run(t.Context(), filepath.Join(t.TempDir(), "out"), cases, c, &Clients{forms, fore, judge})
		require.NoError(t, err)
		require.Equal(t, 3, s.Completed)
		require.Equal(t, 2, s.Formation[0].Cost.Ledger)
		require.Equal(t, 2, forms.Calls())
	})
}
