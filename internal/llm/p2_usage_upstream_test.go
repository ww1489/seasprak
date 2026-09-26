package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

// Pin the actual upstream contract rather than carrying forward the older
// assumption that Claude's adapter discards all cache-write counts. This is a
// dependency probe, not certification of the future product Claude factory.
func TestP2UsageClaudeUpstreamCacheWriteAndPresence(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, write := range []int{-1, 0, 100} {
			t.Run(fmt.Sprintf("stream_%t/write_%d", stream, write), func(t *testing.T) {
				usage := map[string]any{"input_tokens": 300, "cache_read_input_tokens": 600, "output_tokens": 50}
				if write >= 0 {
					usage["cache_creation_input_tokens"] = write
				}
				message := map[string]any{"id": "upstream-fixture", "type": "message", "role": "assistant", "model": "fixture", "content": []any{map[string]any{"type": "text", "text": "done"}}, "stop_reason": "end_turn", "usage": usage}
				raw, err := json.Marshal(message)
				p2OK(t, err)
				body, kind := string(raw), "application/json"
				if stream {
					message["content"] = []any{}
					message["stop_reason"] = nil
					usage["output_tokens"] = 0
					start, err := json.Marshal(map[string]any{"type": "message_start", "message": message})
					p2OK(t, err)
					kind = "text/event-stream"
					body = "event: message_start\ndata: " + string(start) + "\n\n" +
						"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
						"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"done\"}}\n\n" +
						"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
						"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":50}}\n\n" +
						"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				// Both modes receive exactly the same bytes. Only the second one
				// supplements presence; the SDK's numeric values must not change.
				for _, collect := range []bool{false, true} {
					observer := &usageObserver{}
					var calls atomic.Int32
					var settings []llm.UsageCollection
					if collect {
						settings = []llm.UsageCollection{{Protocol: "anthropic-messages", MaxBytes: 4096}}
					}
					ctx := llm.WithUsageObservation(t.Context(), observer)
					ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "logical", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
					client := &http.Client{Transport: llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
					}), settings...)}
					m, err := agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "fixture", MaxTokens: 128, HTTPClient: client})
					p2OK(t, err)
					input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
					var msg *schema.AgenticMessage
					if stream {
						r, err := m.Stream(ctx, input)
						p2OK(t, err)
						closeReader := sync.OnceFunc(r.Close)
						defer closeReader()
						var chunks []*schema.AgenticMessage
						for {
							chunk, err := r.Recv()
							if err == io.EOF {
								break
							}
							p2OK(t, err)
							chunks = append(chunks, chunk)
						}
						closeReader()
						msg, err = schema.ConcatAgenticMessages(chunks)
						p2OK(t, err)
					} else {
						msg, err = m.Generate(ctx, input)
						p2OK(t, err)
					}
					if calls.Load() != 1 || msg == nil || msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
						t.Fatal("upstream probe missing one response and usage")
					}
					u := msg.ResponseMeta.TokenUsage
					if u.PromptTokens != 900+max(0, write) || u.CompletionTokens != 50 || u.PromptTokenDetails.CachedTokens != 600 || u.PromptTokenDetails.CacheWriteTokens != max(0, write) {
						t.Fatalf("upstream cache counts changed with collect=%t: %+v", collect, u)
					}
					value, present := agenticclaude.GetCacheCreationInputTokens(msg)
					if value != max(0, write) || present != (write > 0) {
						t.Fatalf("unexpected upstream getter presence: %d %t", value, present)
					}
					if collect {
						if len(observer.snapshots) != 1 {
							t.Fatal("expected one final presence observation")
						}
						got := observer.snapshots[0].Usage
						if got.CacheWrite.Value != int64(max(0, write)) || got.CacheWrite.Known != (write >= 0) || got.InputTotal.Known != (write >= 0) {
							t.Fatalf("presence supplement conflated zero and absence: %+v", got)
						}
					} else if len(observer.snapshots) != 0 {
						t.Fatal("raw collection enabled without opt-in")
					}
				}
			})
		}
	}
}
