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

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func p2ChatRegister(t *testing.T, c *llm.Catalog, client *http.Client, limit int) {
	t.Helper()
	p2OK(t, c.RegisterOpenAIChat(client, limit))
}
func p2ChatConfig() llm.ModelConfig { c := p2Config(); c.Protocol = "openai-chat"; return c }
func p2ChatContext(t *testing.T, calls *atomic.Int32) context.Context {
	return llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { calls.Add(1); return nil }))
}
func p2ChatInvoke(ctx context.Context, m llm.Model, stream bool, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	if !stream {
		return m.Generate(ctx, in, opts...)
	}
	r, err := m.Stream(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var chunks []*schema.AgenticMessage
	for {
		v, e := r.Recv()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		chunks = append(chunks, v)
	}
	return schema.ConcatAgenticMessages(chunks)
}
func p2ChatBody(stream bool, finish, content string, refusal bool, tools bool, usage bool) string {
	payload := map[string]any{"role": "assistant", "content": content}
	if refusal {
		payload["refusal"] = "Request cannot be fulfilled."
	}
	if tools {
		payload["tool_calls"] = []any{map[string]any{"index": 0, "id": "call-a", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"中文😀"}`}}}
	}
	marshal := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	u := map[string]any{"prompt_tokens": 1000, "completion_tokens": 12, "prompt_tokens_details": map[string]any{"cached_tokens": 600}, "completion_tokens_details": map[string]any{"reasoning_tokens": 2}}
	if !stream {
		v := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": payload, "finish_reason": finish}}}
		if usage {
			v["usage"] = u
		}
		return marshal(v)
	}
	frame := func(delta any, reason any) string {
		return "data: " + marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}}) + "\n\n"
	}
	out := frame(payload, nil)
	if refusal {
		out += frame(map[string]any{"refusal": " Safety policy."}, nil)
	}
	if finish != "" {
		out += frame(map[string]any{}, finish)
	}
	if usage {
		out += "data: " + marshal(map[string]any{"id": "fixture", "choices": []any{}, "usage": u}) + "\n\n"
	}
	return out + "data: [DONE]\n\n"
}
func p2ChatResponse(r *http.Request, stream bool, body string) *http.Response {
	kind := "application/json"
	if stream {
		kind = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestP2OpenAIChatFinishAndRefusal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, finish, want    string
			refusal, tools, usage bool
		}{
			{name: "stop", finish: "stop", want: "stop", usage: true},
			{name: "tools", finish: "tool_calls", want: "tool_calls", tools: true, usage: true},
			{name: "length_tools", finish: "length", want: "length", tools: true},
			{name: "refusal_stop", finish: "stop", want: "refusal", refusal: true},
			{name: "refusal_tools", finish: "tool_calls", want: "refusal", refusal: true, tools: true},
			{name: "content_filter", finish: "content_filter", want: "refusal"},
			{name: "missing"}, {name: "unknown", finish: "synthetic-unknown"},
		} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, tc.name), func(t *testing.T) {
				var physical, observations atomic.Int32
				c := llm.NewCatalog(nil)
				cfg := p2ChatConfig()
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					return p2ChatResponse(r, stream, p2ChatBody(stream, tc.finish, "中文😀", tc.refusal, tc.tools, tc.usage)), nil
				})}, 1<<20)
				p2OK(t, c.Register(cfg))
				m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, e)
				observer := &usageObserver{}
				ctx := llm.WithUsageObservation(p2ChatContext(t, &observations), observer)
				msg, e := p2ChatInvoke(ctx, m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
				if tc.want == "" {
					p2Code(t, e, product.CodeInvalidArgument)
				} else {
					p2OK(t, e)
					if msg == nil || msg.Extra["seasprak.finish"] != tc.want {
						t.Fatal("wrong normalized finish")
					}
					wantRefusal := ""
					if tc.refusal {
						wantRefusal = "Request cannot be fulfilled."
						if stream {
							wantRefusal += " Safety policy."
						}
					}
					if p2RefusalReason(msg) != wantRefusal {
						t.Error("displayable refusal reason lost")
					}
					if msg.Extra["seasprak.original_finish"] != tc.finish {
						t.Error("original finish reason lost")
					}
					if tc.tools {
						var found bool
						for _, b := range msg.ContentBlocks {
							if b.FunctionToolCall != nil {
								found = b.FunctionToolCall.CallID == "call-a" && b.FunctionToolCall.Arguments == `{"q":"中文😀"}`
							}
						}
						if !found {
							t.Fatal("tool content lost")
						}
					}
					if tc.usage && (msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil || msg.ResponseMeta.TokenUsage.PromptTokenDetails.CachedTokens != 600 || msg.ResponseMeta.TokenUsage.CompletionTokensDetails.ReasoningTokens != 2) {
						t.Fatal("SDK usage detail lost during finish normalization")
					}
					if !tc.usage && msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil {
						t.Fatal("absent usage became SDK zero usage")
					}
				}
				if physical.Load() != 1 || observations.Load() != 1 {
					t.Fatal("request accounting mismatch")
				}
				observer.mu.Lock()
				defer observer.mu.Unlock()
				if len(observer.snapshots) != 1 {
					t.Fatal("usage observation missing or duplicated")
				}
				u := observer.snapshots[0].Usage
				if tc.usage {
					if !u.InputTotal.Known || u.InputTotal.Value != 1000 || u.CacheRead.Value != 600 || u.OutputTotal.Value != 12 || u.Reasoning.Value != 2 || u.CacheWrite.Known {
						t.Fatal("raw usage mismatch")
					}
				} else if u.InputTotal.Known || u.OutputTotal.Known {
					t.Fatal("missing usage fabricated")
				}
			})
		}
	}
}

func TestP2OpenAIChatCollectionFailClosed(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"overflow", "incomplete", "unknown_format", "unknown_choice", "bad_refusal"} {
			t.Run(fmt.Sprintf("%t/%s", stream, kind), func(t *testing.T) {
				cfg := p2ChatConfig()
				c := llm.NewCatalog(nil)
				var physical, observations atomic.Int32
				limit := 1 << 20
				if kind == "overflow" {
					limit = 64
				}
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					body := p2ChatBody(stream, "stop", "text", false, false, false)
					if kind == "incomplete" {
						if stream {
							body = strings.TrimSuffix(body, "data: [DONE]\n\n")
						} else {
							body = strings.TrimSuffix(body, "}")
						}
					}
					if kind == "unknown_choice" {
						body = strings.ReplaceAll(body, `"index":0`, `"index":1`)
					}
					if kind == "bad_refusal" {
						body = strings.ReplaceAll(body, `"content":"text"`, `"content":"text","refusal":{"unexpected":true}`)
					}
					res := p2ChatResponse(r, stream, body)
					if kind == "unknown_format" {
						res.Header.Set("Content-Type", "application/octet-stream")
					}
					return res, nil
				})}, limit)
				p2OK(t, c.Register(cfg))
				m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, e)
				_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
				if e == nil {
					t.Fatal("unsafe collection was accepted")
				}
				wantCode := product.CodeResourceUnavailable
				if kind == "overflow" || kind == "unknown_format" || ((kind == "incomplete" || kind == "unknown_choice") && stream) {
					wantCode = product.CodeInvalidArgument
				}
				p2Code(t, e, wantCode)
				if physical.Load() != 1 || observations.Load() != 1 {
					t.Fatal("request count mismatch")
				}
			})
		}
	}
}

func TestP2OpenAIChatConcurrentIsolation(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var physical, observations atomic.Int32
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		raw, e := io.ReadAll(r.Body)
		if e != nil {
			return nil, e
		}
		refused := strings.Contains(string(raw), "refuse-this")
		return p2ChatResponse(r, true, p2ChatBody(true, "stop", "text", refused, false, true)), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			input, want := "normal", "stop"
			if i%2 == 0 {
				input, want = "refuse-this", "refusal"
			}
			msg, e := p2ChatInvoke(p2ChatContext(t, &observations), m, true, []*schema.AgenticMessage{schema.UserAgenticMessage(input)})
			if e != nil {
				t.Error(e)
				return
			}
			if msg.Extra["seasprak.finish"] != want {
				t.Error("concurrent refusal evidence leaked between requests")
			}
		})
	}
	wg.Wait()
	if physical.Load() != 20 || observations.Load() != 20 {
		t.Fatal("concurrent count mismatch")
	}
}
