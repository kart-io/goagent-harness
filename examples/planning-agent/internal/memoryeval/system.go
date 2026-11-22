// Package memoryeval compares memory layouts on independent copies of a formed store.
package memoryeval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/session"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/memory"
	"github.com/kart-io/goagent/memory/memeval"
)

type layoutSystem struct {
	label, namespace string
	mem              *memory.Memory
}

func (s layoutSystem) Name() string { return s.label }
func (s layoutSystem) Ingest(_ context.Context, c memeval.Case) (memeval.Retriever, error) {
	if c.ID != s.namespace {
		return nil, fmt.Errorf("用例与快照命名空间不一致")
	}
	return s, nil
}
func (s layoutSystem) Context(ctx context.Context, q memeval.Question) (memeval.Retrieved, error) {
	got, err := s.mem.Retrieve(memory.WithNamespace(ctx, s.namespace), q.Text)
	return memeval.Retrieved{Text: got.Text, Sessions: got.Sessions()}, err
}

func openMemory(path string, cfg memory.Config) (*memory.Memory, error) {
	d, err := durable.NewFileStore(path)
	if err != nil {
		return nil, err
	}
	cfg.Durable = d
	return memory.New(cfg)
}

func copyLayouts(dir string, c memeval.Case, cfg memory.Config, layout memory.ContextLayout) ([]layoutSystem, error) {
	var systems []layoutSystem
	for _, label := range []string{"legacy", "layout"} {
		dst := filepath.Join(dir, label)
		if err := os.CopyFS(dst, os.DirFS(filepath.Join(dir, "formed"))); err != nil {
			return nil, err
		}
		if label == "layout" {
			cfg.ContextLayout = &layout
		}
		mem, err := openMemory(dst, cfg)
		if err != nil {
			return nil, err
		}
		systems = append(systems, layoutSystem{label, c.ID, mem})
	}
	return systems, nil
}

const pinLimit = 8
const pinPolicy = "empty-query-recall-top-8-before-copy-v1"

func form(ctx context.Context, dir string, c memeval.Case, model llm.Client, runes int, layout memory.ContextLayout, stats *Formation) ([]layoutSystem, error) {
	var last time.Time
	for _, s := range c.Sessions {
		if s.Date.After(last) {
			last = s.Date
		}
	}
	d, err := durable.NewFileStore(filepath.Join(dir, "formed"))
	if err != nil {
		return nil, err
	}
	cfg := memory.Config{Durable: d, Now: func() time.Time { return last }, RecallRunes: runes, JobAttempts: 1}
	sys := memeval.Memory{Configure: func(s session.Store) memory.Config { copy := cfg; copy.Model = model; copy.Sessions = s; return copy }}
	// 抽取器只读历史，不接收问题及答案。
	history := c
	history.Questions = nil
	if _, err = sys.Ingest(ctx, history); err != nil {
		return nil, err
	}
	formed, err := memory.New(cfg)
	if err != nil {
		return nil, err
	}
	ns := memory.WithNamespace(ctx, c.ID)
	records, err := formed.Recall(ns, memory.Query{K: pinLimit})
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("抽取后没有有效记忆")
	}
	// 使用空查询的公开排序，在复制前钉住同一批有效记录。保留原有来源与正文。
	pinned := true
	for _, record := range records {
		if _, err = formed.Update(ns, record.ID, record.Revision, memory.Patch{Pinned: &pinned}); err != nil {
			return nil, err
		}
		stats.PinnedRecords++
	}
	return copyLayouts(dir, c, cfg, layout)
}

// observedSystem only passes history into ingestion and unlabelled questions into retrieval.
type observedSystem struct {
	memeval.System
	retrieved memeval.Retrieved
}

func (s *observedSystem) Ingest(ctx context.Context, c memeval.Case) (memeval.Retriever, error) {
	c.Questions = nil
	r, err := s.System.Ingest(ctx, c)
	if err != nil {
		return nil, err
	}
	return observedRetriever{r, s}, nil
}

type observedRetriever struct {
	source memeval.Retriever
	system *observedSystem
}

func (r observedRetriever) Context(ctx context.Context, q memeval.Question) (memeval.Retrieved, error) {
	got, err := r.source.Context(ctx, memeval.Question{ID: q.ID, Text: q.Text, Date: q.Date})
	if err == nil {
		r.system.retrieved = got
	}
	return got, err
}
