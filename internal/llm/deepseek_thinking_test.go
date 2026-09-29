package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cloudwego/eino/components/model"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

func deepSeekThinkingConfig(endpoint string) llm.ModelConfig {
	cfg := p2DeepSeekConfig()
	cfg.Endpoint = endpoint
	v := llm.Capability{Status: llm.Verified, AdapterVersion: "agenticdeepseek-v0.1.1", Endpoint: endpoint, Model: cfg.Model, ModelVersion: "fixture-v1", ConfigVersion: cfg.Version, Evidence: []string{"offline thinking toggle fixture"}}
	cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: v, NativeValue: "enabled"}, "off": {Capability: v, NativeValue: "disabled"}}
	return cfg
}

func deepSeekThinkingReply(stream bool) p2ProbeReply {
	body := `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":2,"prompt_cache_miss_tokens":8}}`
	if stream {
		body = "data: " + `{"id":"fixture","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_cache_hit_tokens":2,"prompt_cache_miss_tokens":8}}` + "\n\ndata: [DONE]\n\n"
	}
	return p2ProbeReply{stream, body}
}

func TestP2DeepSeekThinkingToolReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			message := map[string]any{"role": "assistant", "content": "", "reasoning_content": "推理😀", "tool_calls": []any{map[string]any{"index": 0, "id": "call-fixture", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}
			key := "message"
			if stream {
				key = "delta"
			}
			encoded, err := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, key: message, "finish_reason": "tool_calls"}}})
			p2OK(t, err)
			body := string(encoded)
			if stream {
				body = "data: " + body + "\n\ndata: [DONE]\n\n"
			}
			endpoint, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body}, deepSeekThinkingReply(stream))
			cfg := deepSeekThinkingConfig(endpoint)
			c := llm.NewCatalog(nil)
			p2DeepSeekRegister(t, c, client, 1<<20)
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{Thinking: "low"})
			p2OK(t, err)
			var observed atomic.Int32
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("lookup")}
			tool := model.WithTools([]*schema.ToolInfo{{Name: "lookup"}})
			msg, err := p2ChatInvoke(p2ChatContext(t, &observed), m, stream, input, tool)
			p2OK(t, err)
			reasoning := ""
			calls := 0
			for _, block := range msg.ContentBlocks {
				if block.Reasoning != nil {
					reasoning += block.Reasoning.Text
				}
				if block.FunctionToolCall != nil {
					calls++
					if block.FunctionToolCall.CallID != "call-fixture" {
						t.Fatal("call ID lost")
					}
				}
			}
			if reasoning != "推理😀" || calls != 1 {
				t.Fatal("reasoning or tool response lost")
			}
			input = append(input, msg, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "call-fixture", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "result"}}}})}})
			final, err := p2ChatInvoke(p2ChatContext(t, &observed), m, stream, input, tool)
			p2OK(t, err)
			if final.ResponseMeta == nil || final.ResponseMeta.TokenUsage == nil || final.ResponseMeta.TokenUsage.PromptTokens != 10 || final.ResponseMeta.TokenUsage.CompletionTokens != 3 || final.ResponseMeta.TokenUsage.PromptTokenDetails.CachedTokens != 2 {
				t.Fatal("usage lost")
			}
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			messages := request["messages"].([]any)
			assistant := messages[1].(map[string]any)
			result := messages[2].(map[string]any)
			if assistant["reasoning_content"] != "推理😀" || result["tool_call_id"] != "call-fixture" || result["content"] != "result" {
				t.Fatal("tool replay lost reasoning or result identity")
			}
			if len(request["tools"].([]any)) != 1 || observed.Load() != 2 {
				t.Fatal("tools or physical count mismatch")
			}
		})
	}
}

func TestP2DeepSeekThinkingCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			entered := make(chan struct{})
			stopped := make(chan struct{})
			var physical, observed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				physical.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"partial\"}}]}\n\n")
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(stopped)
			}))
			defer server.Close()
			cfg := deepSeekThinkingConfig(server.URL)
			c := llm.NewCatalog(nil)
			p2DeepSeekRegister(t, c, server.Client(), 1<<20)
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{Thinking: "low"})
			p2OK(t, err)
			ctx, cancel := context.WithCancel(p2ChatContext(t, &observed))
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := p2ChatInvoke(ctx, m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation identity lost")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("model did not cancel")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP did not cancel")
			}
			if physical.Load() != 1 || observed.Load() != 1 {
				t.Fatal("cancel retried request")
			}
		})
	}
}
func TestP2DeepSeekThinkingOptions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, name := range []string{"on", "off", "unspecified", "default", "unverified", "bad_off", "bad_on", "budget"} {
			t.Run(fmt.Sprintf("%t/%s", stream, name), func(t *testing.T) {
				reject := name == "unverified" || name == "bad_off" || name == "bad_on" || name == "budget"
				var replies []p2ProbeReply
				if !reject {
					replies = append(replies, deepSeekThinkingReply(stream))
				}
				endpoint, client, requests := p2ProbeServer(t, replies...)
				cfg := deepSeekThinkingConfig(endpoint)
				requested := llm.RequestedOptions{Thinking: "low"}
				want := "enabled"
				switch name {
				case "off":
					requested.Thinking = "off"
					want = "disabled"
				case "unspecified":
					requested.Thinking = ""
					want = ""
				case "default":
					requested.Thinking = ""
					cfg.Parameters.DefaultThinking = "low"
				case "unverified":
					cfg.Capabilities.Thinking = nil
				case "bad_off":
					requested.Thinking = "off"
					cfg.Capabilities.Thinking["off"] = cfg.Capabilities.Thinking["low"]
				case "bad_on":
					cfg.Capabilities.Thinking["low"] = cfg.Capabilities.Thinking["off"]
				case "budget":
					v := cfg.Capabilities.Thinking["low"]
					v.BudgetTokens = 10
					cfg.Capabilities.Thinking["low"] = v
				}
				c := llm.NewCatalog(nil)
				p2DeepSeekRegister(t, c, client, 1<<20)
				p2OK(t, c.Register(cfg))
				m, err := c.Bind(cfg.Key(), requested)
				var observed atomic.Int32
				if err == nil {
					_, err = p2ChatInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				}
				if reject {
					p2Code(t, err, "unsupported_capability")
					if observed.Load() != 0 {
						t.Fatal("rejected control observed HTTP")
					}
					return
				}
				p2OK(t, err)
				payload := p2ProbeRequest(t, requests)
				if observed.Load() != 1 {
					t.Fatal("physical observation count mismatch")
				}
				thinking, present := payload["thinking"]
				if want == "" {
					if present {
						t.Fatal("unspecified thinking sent toggle")
					}
				} else {
					mode, _ := thinking.(map[string]any)
					if mode["type"] != want {
						t.Fatal("native thinking toggle mismatch")
					}
				}
				if _, ok := payload["reasoning_effort"]; ok {
					t.Fatal("effort substituted for toggle")
				}
			})
		}
	}
}
