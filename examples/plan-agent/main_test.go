package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/kart-io/goagent/llm"

	shared "github.com/kart-io/goagent/constants"
	"github.com/stretchr/testify/require"
)

type summaryClient func(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error)

func (s summaryClient) CompleteStream(ctx context.Context, req *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	return s(ctx, req)
}
func (summaryClient) Provider() shared.Provider { return shared.ProviderCustom }

func TestSummarizerObservesSuccessfulTerminalOnly(t *testing.T) {
	sentinel := errors.New("summary failed")
	for _, tc := range []struct {
		name          string
		chunks        []*llm.StreamChunk
		requestErr    error
		wantErr       error
		wantSummaries int
	}{
		{name: "success", chunks: []*llm.StreamChunk{{Delta: "summary"}, {Done: true, FinishReason: "stop", ResponseModel: "m", Usage: &llm.Usage{TotalTokens: 3}}}, wantSummaries: 1},
		{name: "request error", requestErr: sentinel, wantErr: sentinel},
		{name: "stream error", chunks: []*llm.StreamChunk{{Delta: "partial"}, {Done: true, Error: sentinel}}, wantErr: sentinel},
		{name: "unterminated", chunks: []*llm.StreamChunk{{Delta: "partial"}}, wantErr: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var upstream context.Context
			s := summarizer{r: &record{}, Client: summaryClient(func(ctx context.Context, _ *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
				upstream = ctx
				ch := make(chan *llm.StreamChunk, len(tc.chunks))
				for _, chunk := range tc.chunks {
					ch <- chunk
				}
				close(ch)
				return ch, tc.requestErr
			})}
			resp, err := llm.Chat(context.Background(), s, []llm.Message{llm.SystemMessage("summarize"), llm.UserMessage("history")})
			require.ErrorIs(t, err, tc.wantErr)
			if err == nil {
				require.Equal(t, "summary", resp.Content)
				require.Equal(t, "m", resp.ResponseModel)
				require.Equal(t, 3, resp.TokensUsed)
			}
			s.r.mu.Lock()
			require.Equal(t, tc.wantSummaries, s.r.summaries)
			s.r.mu.Unlock()
			require.Eventually(t, func() bool { return upstream.Err() != nil }, 5*time.Second, time.Millisecond)
		})
	}
}

func TestSummarizerCancellationClosesForwarder(t *testing.T) {
	var upstream context.Context
	s := summarizer{r: &record{}, Client: summaryClient(func(ctx context.Context, _ *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
		upstream = ctx
		return make(chan *llm.StreamChunk), nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := s.CompleteStream(ctx, &llm.CompletionRequest{})
	require.NoError(t, err)
	cancel()
	select {
	case _, open := <-stream:
		require.False(t, open)
	case <-time.After(5 * time.Second):
		t.Fatal("summary forwarder did not stop after cancellation")
	}
	require.ErrorIs(t, upstream.Err(), context.Canceled)
	require.Zero(t, s.r.summaries)
}
