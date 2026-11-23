package memoryeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kart-io/goagent/core/budget"
	"github.com/kart-io/goagent/core/durable"
	"github.com/kart-io/goagent/core/usage"
	ae "github.com/kart-io/goagent/errors"
	"github.com/kart-io/goagent/llm"
	"io"
	"strings"
)

// Cost counts client submissions, reconciled requests and provider-reported usage.
type Cost struct {
	Physical     int `json:"physical_requests"`
	Ledger       int `json:"ledger_requests"`
	Tokens       int `json:"known_tokens"`
	Unknown      int `json:"unknown_usage_calls"`
	UnknownPrice int `json:"unknown_cost_calls"`
}
type meter struct {
	llm.Client
	model                   string
	ctl                     *budget.Controller
	limit, output, physical int
	answer                  string
	answered                bool
}

func newMeter(path string, client llm.Client, model string, c Config) (*meter, error) {
	d, err := durable.NewFileStore(path)
	if err != nil {
		return nil, err
	}
	ctl, err := budget.NewDurable(budget.Config{RootKey: "memory-eval", Version: "1", TokenPolicyVersion: "utf8-bytes-plus-output-v1", MaxModelRequests: c.Requests, MaxReservedTokens: c.Tokens}, d)
	if err != nil {
		return nil, err
	}
	return &meter{Client: client, model: model, ctl: ctl, limit: c.ContextTokens, output: c.OutputTokens}, nil
}

// 每个实例只归一个阶段和任务使用。控制器只锁准入事务，不锁模型流。
func (m *meter) CompleteStream(ctx context.Context, r *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	req := *r
	req.Model = m.model
	req.Temperature = 0
	req.TemperatureSet = true
	req.MaxTokens = m.output
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	estimate := len(raw) + 16*(len(req.Messages)+1) + req.MaxTokens
	if estimate > m.limit {
		return nil, fmt.Errorf("上下文估算超出上限：%w", ae.ErrModelBudgetExceeded)
	}
	key := fmt.Sprintf("request-%d", m.physical+1)
	if err = m.ctl.ReserveEstimated(ctx, key, estimate); err != nil {
		return nil, err
	}
	m.physical++
	stream, err := m.Client.CompleteStream(ctx, &req)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, fmt.Errorf("模型返回空流")
	}
	var deltas strings.Builder
	for {
		if ctx.Err() != nil {
			return nil, m.cancelUsage(ctx, key, stream)
		}
		select {
		case <-ctx.Done():
			return nil, m.cancelUsage(ctx, key, stream)
		case chunk, ok := <-stream:
			if !ok {
				return nil, io.ErrUnexpectedEOF
			}
			if chunk == nil {
				continue
			}
			if chunk.Usage != nil {
				e := usage.Evidence{Tokens: chunk.Usage.Tokens(), KnownFields: chunk.Usage.Fields()}
				if _, err = m.ctl.RecordEvidence(context.WithoutCancel(ctx), key, nil, e); err != nil {
					return nil, err
				}
			}
			if ctx.Err() != nil {
				return nil, m.cancelUsage(ctx, key, stream)
			}
			if chunk.Error != nil {
				return nil, chunk.Error
			}
			deltas.WriteString(chunk.Delta)
			if !chunk.Done {
				continue
			}
			terminal := *chunk
			if terminal.Content == "" {
				terminal.Content = deltas.String()
			}
			m.answer = strings.TrimSpace(terminal.Content)
			m.answered = true
			out := make(chan *llm.StreamChunk, 1)
			out <- &terminal
			close(out)
			return out, nil
		}
	}
}

// 取消时只结算当时已缓冲的块，不等生产者继续发送或关闭；取消仍作为错误返回。
func (m *meter) cancelUsage(ctx context.Context, key string, stream <-chan *llm.StreamChunk) error {
	err := ctx.Err()
	for range len(stream) {
		select {
		case chunk := <-stream:
			if chunk != nil && chunk.Usage != nil {
				e := usage.Evidence{Tokens: chunk.Usage.Tokens(), KnownFields: chunk.Usage.Fields()}
				_, recordErr := m.ctl.RecordEvidence(context.WithoutCancel(ctx), key, nil, e)
				err = errors.Join(err, recordErr)
			}
		default:
			return err
		}
	}
	return err
}
func (m *meter) cost(ctx context.Context) (Cost, error) {
	s, err := m.ctl.Attribution(context.WithoutCancel(ctx))
	c := Cost{Physical: m.physical, Ledger: s.Root.ReservedRequests, Tokens: s.Root.KnownTokens.TotalTokens, Unknown: s.Unattributed.UnknownTotalEntries, UnknownPrice: m.physical}
	if err == nil && c.Physical != c.Ledger {
		err = fmt.Errorf("物理调用与账本计数不一致")
	}
	return c, err
}
