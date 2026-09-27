package llm_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

// Exercise the actual catalog factories, not an SDK-only converter.
func TestFactoryUsagePresence(t *testing.T) {
	for _, provider := range []string{"anthropic", "gemini", "deepseek"} {
		for _, stream := range []bool{false, true} {
			for _, mode := range []string{"missing", "zero", "cumulative", "overflow"} {
				t.Run(fmt.Sprintf("%s/stream_%t/%s", provider, stream, mode), func(t *testing.T) {
					secret := "synthetic-presence-fixture"
					cfg := p2DeepSeekConfig()
					catalog := llm.NewCatalog(nil)
					if provider == "anthropic" {
						cfg = anthropicConfig()
						catalog = llm.NewCatalog(anthropicCredentialResolver(&secret))
					}
					if provider == "gemini" {
						cfg = geminiConfig()
						catalog = llm.NewCatalog(geminiCredentialResolver(&secret))
					}
					var physical, observed atomic.Int32
					body := factoryUsageBody(provider, stream, mode)
					client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
						physical.Add(1)
						return anthropicResponse(r, stream, body), nil
					})}
					limit := 1 << 20
					if mode == "overflow" {
						limit = 64
					}
					switch provider {
					case "anthropic":
						p2OK(t, catalog.RegisterAnthropicMessages(client, limit))
					case "gemini":
						p2OK(t, catalog.RegisterGeminiGenerateContent(client, limit))
					default:
						p2OK(t, catalog.RegisterDeepSeekChat(client, limit))
					}
					p2OK(t, catalog.Register(cfg))
					bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
					p2OK(t, err)
					observer := &usageObserver{}
					ctx := llm.WithUsageObservation(p2ChatContext(t, &observed), observer)
					var msg *schema.AgenticMessage
					if !stream {
						msg, err = anthropicInvoke(ctx, bound, false)
					} else {
						reader, streamErr := bound.Stream(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
						p2OK(t, streamErr)
						defer reader.Close()
						var chunks []*schema.AgenticMessage
						for {
							chunk, recvErr := reader.Recv()
							if recvErr == io.EOF {
								break
							}
							p2OK(t, recvErr)
							if chunk.Extra["seasprak.finish"] == nil && chunk.ResponseMeta != nil && chunk.ResponseMeta.TokenUsage != nil {
								t.Fatal("intermediate cumulative usage escaped before final projection")
							}
							chunks = append(chunks, chunk)
						}
						msg, err = schema.ConcatAgenticMessages(chunks)
					}
					p2OK(t, err)
					if msg.Extra["seasprak.finish"] != "stop" {
						t.Fatal("normal terminal lost")
					}
					if physical.Load() != 1 || observed.Load() != 1 {
						t.Fatalf("physical=%d observed=%d", physical.Load(), observed.Load())
					}
					observer.mu.Lock()
					defer observer.mu.Unlock()
					if len(observer.snapshots) != 1 {
						t.Fatalf("usage reports=%d", len(observer.snapshots))
					}
					u := observer.snapshots[0].Usage
					if mode == "missing" || mode == "overflow" {
						if u != (llm.UsageRecord{}) {
							t.Fatalf("unexpected known usage: %+v", u)
						}
						if msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
							t.Fatalf("SDK synthesized usage escaped: %+v", msg.ResponseMeta.TokenUsage)
						}
						if mode == "overflow" && observer.snapshots[0].Diagnostic != "usage_buffer_limit" {
							t.Fatal("overflow diagnostic lost")
						}
						return
					}
					wantIn, wantOut := int64(10), int64(3)
					if mode == "zero" {
						wantIn, wantOut = 0, 0
					}
					if !u.InputTotal.Known || u.InputTotal.Value != wantIn || !u.OutputTotal.Known || u.OutputTotal.Value != wantOut {
						t.Fatalf("raw presence/cumulative usage=%+v", u)
					}
					if msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
						t.Fatal("known usage was removed")
					}
					sdk := msg.ResponseMeta.TokenUsage
					if int64(sdk.PromptTokens) != wantIn || int64(sdk.CompletionTokens) != wantOut {
						t.Fatalf("aggregated usage=%+v", sdk)
					}
				})
			}
		}
	}
}

func factoryUsageBody(provider string, stream bool, mode string) string {
	in, out := 10, 3
	if mode == "zero" {
		in, out = 0, 0
	}
	encode := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	switch provider {
	case "anthropic":
		usage := map[string]any{"input_tokens": in, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": out}
		if !stream {
			var m map[string]any
			_ = json.Unmarshal([]byte(anthropicBody("end_turn", "ok", false, false)), &m)
			if mode != "missing" {
				m["usage"] = usage
			}
			return encode(m)
		}
		body := anthropicStreamBody("end_turn", "ok", false, true)
		// Replace both usage objects independently, retaining the real stop event.
		var frames []string
		for _, frame := range strings.Split(body, "\n\n") {
			if frame == "" {
				continue
			}
			lines := strings.Split(frame, "\n")
			var m map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &m)
			if m["type"] == "message_start" {
				start := m["message"].(map[string]any)
				delete(start, "usage")
				if mode != "missing" {
					initial := map[string]any{"input_tokens": in, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0, "output_tokens": 0}
					start["usage"] = initial
				}
			}
			if m["type"] == "message_delta" {
				delete(m, "usage")
				if mode != "missing" {
					m["usage"] = map[string]any{"output_tokens": out}
				}
			}
			frames = append(frames, lines[0]+"\ndata: "+encode(m)+"\n\n")
		}
		return strings.Join(frames, "")
	case "gemini":
		makeBody := func(finish string, n int) string {
			var m map[string]any
			_ = json.Unmarshal([]byte(geminiBody(finish, "ok", false, false)), &m)
			if mode != "missing" {
				m["usageMetadata"] = map[string]any{"promptTokenCount": in, "candidatesTokenCount": n, "thoughtsTokenCount": 0, "cachedContentTokenCount": 0}
			}
			return encode(m)
		}
		if !stream {
			return makeBody("STOP", out)
		}
		return "data: " + makeBody("", 0) + "\n\ndata: " + makeBody("STOP", out) + "\n\n"
	default:
		makeBody := func(finish string, n int) string {
			key := "message"
			if stream {
				key = "delta"
			}
			m := map[string]any{"id": "fixture", "model": "fixture", "choices": []any{map[string]any{"index": 0, key: map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": finish}}}
			if mode != "missing" {
				m["usage"] = map[string]any{"prompt_tokens": in, "completion_tokens": n, "prompt_cache_hit_tokens": 0, "prompt_cache_miss_tokens": in}
			}
			return encode(m)
		}
		if !stream {
			return makeBody("stop", out)
		}
		return "data: " + makeBody("", 0) + "\n\ndata: " + makeBody("stop", out) + "\n\ndata: [DONE]\n\n"
	}
}
