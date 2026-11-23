package memoryeval

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kart-io/goagent/llm"
	"github.com/kart-io/goagent/testing/mocks"
	"github.com/stretchr/testify/require"
)

type cancelBufferedClient struct {
	*mocks.ScriptedClient
	cancel      context.CancelFunc
	closeStream bool
}

func (c cancelBufferedClient) CompleteStream(context.Context, *llm.CompletionRequest) (<-chan *llm.StreamChunk, error) {
	stream := make(chan *llm.StreamChunk, 1)
	stream <- &llm.StreamChunk{Done: true, Content: "answer", Usage: &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}
	if c.closeStream {
		close(stream)
	}
	c.cancel()
	return stream, nil
}

func TestCancellationRetainsBufferedUsageEvidence(t *testing.T) {
	for _, closed := range []bool{true, false} {
		name := "生产者未关闭"
		if closed {
			name = "生产者已关闭"
		}
		t.Run(name, func(t *testing.T) {
			for range 30 {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client := cancelBufferedClient{mocks.NewScriptedClient(), cancel, closed}
				m, err := newMeter(filepath.Join(t.TempDir(), "cost"), client, "fixture", DefaultConfig())
				require.NoError(t, err)
				done := make(chan error, 1)
				go func() {
					_, err := m.CompleteStream(ctx, &llm.CompletionRequest{Messages: []llm.Message{llm.UserMessage("q")}})
					done <- err
				}()
				select {
				case err := <-done:
					require.ErrorIs(t, err, context.Canceled)
				case <-time.After(2 * time.Second):
					t.Fatal("取消不应等待生产者关闭")
				}
				cost, err := m.cost(ctx)
				require.NoError(t, err)
				require.Equal(t, 5, cost.Tokens)
				require.Zero(t, cost.Unknown)
				require.Equal(t, 1, cost.Physical)
				require.Equal(t, 1, cost.Ledger)
				require.False(t, m.answered, "取消不能变为成功答案")
			}
		})
	}
}
