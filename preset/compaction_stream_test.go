package preset

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kart-io/goagent/builder"
	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/llm/constants"
	"github.com/stretchr/testify/require"
)

// This client implements only the streaming contract used by real providers.
type summaryStream func(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error)

func (s summaryStream) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return s(ctx, req)
}
func (summaryStream) Provider() constants.Provider { return constants.ProviderCustom }

func TestCompactionCollectsSummaryStream(t *testing.T) {
	var streamCtx context.Context
	model := summaryStream(func(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
		streamCtx = ctx
		require.Len(t, req.Messages, 2)
		require.Contains(t, req.Messages[1].Content, "old history")
		ch := make(chan *llm.StreamChunk, 3)
		ch <- &llm.StreamChunk{Delta: "sum"}
		ch <- &llm.StreamChunk{Delta: "mary"}
		ch <- &llm.StreamChunk{Done: true, FinishReason: "stop"}
		close(ch)
		return ch, nil
	})
	cp := &compactor{model: model}
	out, changed, err := cp.compact(context.Background(), []llm.Message{llm.UserMessage("old history"), llm.UserMessage("new input")}, 1)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []llm.Message{llm.UserMessage(summaryPrefix + "summary"), llm.UserMessage("new input")}, out)
	require.ErrorIs(t, streamCtx.Err(), context.Canceled, "the summary stream is released after completion")
}

func TestCompactionStreamFailuresPreserveHistory(t *testing.T) {
	sentinel := errors.New("summary failed")
	for _, tc := range []struct {
		name       string
		chunks     []*llm.StreamChunk
		requestErr error
		want       error
	}{
		{name: "request error", requestErr: sentinel, want: sentinel},
		{name: "stream error", chunks: []*llm.StreamChunk{{Delta: "partial"}, {Error: sentinel}}, want: sentinel},
		{name: "missing terminal", chunks: []*llm.StreamChunk{{Delta: "partial"}}, want: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := &compactor{model: summaryStream(func(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
				ch := make(chan *llm.StreamChunk, len(tc.chunks))
				for _, chunk := range tc.chunks {
					ch <- chunk
				}
				close(ch)
				return ch, tc.requestErr
			})}
			messages := []llm.Message{llm.UserMessage("old history"), llm.UserMessage("new input")}
			step := &builder.Step{Messages: messages, Inputs: 1}
			_, err := cp.preStep(context.Background(), step)
			require.ErrorIs(t, err, tc.want)
			require.Equal(t, messages, step.Messages)
		})
	}
}

func TestCompactionCancelsOpenSummaryStream(t *testing.T) {
	entered := make(chan context.Context, 1)
	cp := &compactor{model: summaryStream(func(ctx context.Context, _ *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
		entered <- ctx
		return make(chan *llm.StreamChunk), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := cp.compact(ctx, []llm.Message{llm.UserMessage("old history"), llm.UserMessage("new input")}, 1)
		done <- err
	}()
	streamCtx := <-entered
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.ErrorIs(t, streamCtx.Err(), context.Canceled)
}
