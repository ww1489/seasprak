package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func protocolConfigs() []llm.ModelConfig {
	return []llm.ModelConfig{p2ChatConfig(), p2ResponsesConfig(), anthropicConfig(), geminiConfig(), p2DeepSeekConfig()}
}

func protocolBind(t *testing.T, cfg llm.ModelConfig, client *http.Client) llm.Model {
	t.Helper()
	return p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error {
		switch cfg.Protocol {
		case "openai-chat":
			return c.RegisterOpenAIChat(client, 1<<20)
		case "openai-responses":
			return c.RegisterOpenAIResponses(client, 1<<20)
		case "anthropic-messages":
			return c.RegisterAnthropicMessages(client, 1<<20)
		case "gemini-generate-content":
			return c.RegisterGeminiGenerateContent(client, 1<<20)
		case "deepseek-chat":
			return c.RegisterDeepSeekChat(client, 1<<20)
		default:
			t.Fatal("unexpected protocol")
			return nil
		}
	})
}

func protocolJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// Each fixture represents the same semantic response. Provider wire formats stay
// here; the conversation driver below never branches on a provider.
func protocolBody(protocol string, stream bool, calls int, usage bool) string {
	const args = `{"a":2,"b":3}`
	const answer = "5"
	event := func(v any) string { return "data: " + protocolJSON(v) + "\n\n" }
	switch protocol {
	case "openai-chat", "deepseek-chat":
		body := p2ChatBody(stream, "stop", answer, false, false, usage)
		if protocol == "deepseek-chat" {
			body = strings.ReplaceAll(body, `"prompt_tokens":1000`, `"prompt_tokens":1000,"prompt_cache_hit_tokens":600,"prompt_cache_miss_tokens":400`)
		}
		if protocol == "deepseek-chat" && stream {
			body = strings.ReplaceAll(body, `"choices":[]`, `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]`)
		}
		if calls > 0 {
			var tools []any
			for i := 0; i < calls; i++ {
				tools = append(tools, map[string]any{"index": i, "id": fmt.Sprintf("call-%d", i), "type": "function", "function": map[string]any{"name": "calculate", "arguments": args}})
			}
			payload := map[string]any{"role": "assistant", "tool_calls": tools}
			if stream {
				v := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": payload, "finish_reason": "tool_calls"}}}
				if usage {
					v["usage"] = map[string]any{"prompt_tokens": 1000, "completion_tokens": 12, "prompt_cache_hit_tokens": 600, "prompt_cache_miss_tokens": 400, "prompt_tokens_details": map[string]any{"cached_tokens": 600}}
				}
				body = event(v)
				return body + "data: [DONE]\n\n"
			}
			v := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": payload, "finish_reason": "tool_calls"}}}
			if usage {
				v["usage"] = map[string]any{"prompt_tokens": 1000, "completion_tokens": 12, "prompt_cache_hit_tokens": 600, "prompt_cache_miss_tokens": 400, "prompt_tokens_details": map[string]any{"cached_tokens": 600}}
			}
			body = protocolJSON(v)
		}
		return body
	case "openai-responses":
		output := p2ResponsesTextOutput(answer)
		if calls > 0 {
			output = nil
			for i := 0; i < calls; i++ {
				output = append(output, map[string]any{"id": fmt.Sprintf("fc_%d", i), "type": "function_call", "status": "completed", "call_id": fmt.Sprintf("call-%d", i), "name": "calculate", "arguments": args})
			}
		}
		response := p2ResponsesResponse("completed", "", output, false)
		var v map[string]any
		_ = json.Unmarshal([]byte(response), &v)
		if usage {
			v["usage"] = map[string]any{"input_tokens": 1000, "output_tokens": 12, "input_tokens_details": map[string]any{"cached_tokens": 600}}
		}
		if !stream {
			return protocolJSON(v)
		}
		body := ""
		if calls == 0 {
			body += event(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": answer})
		}
		for i, item := range output {
			body += event(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
			body += event(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		return body + event(map[string]any{"type": "response.completed", "response": v})
	case "anthropic-messages":
		blocks := []any{map[string]any{"type": "text", "text": answer}}
		finish := "end_turn"
		if calls > 0 {
			finish = "tool_use"
			blocks = nil
			for i := 0; i < calls; i++ {
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": fmt.Sprintf("call-%d", i), "name": "calculate", "input": map[string]any{"a": 2, "b": 3}})
			}
		}
		v := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture", "content": blocks, "stop_reason": finish}
		u := map[string]any{"input_tokens": 300, "cache_read_input_tokens": 600, "cache_creation_input_tokens": 100, "output_tokens": 12}
		if usage {
			v["usage"] = u
		}
		if !stream {
			return protocolJSON(v)
		}
		start := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "fixture", "content": []any{}}
		if usage {
			start["usage"] = map[string]any{"input_tokens": 300, "cache_read_input_tokens": 600, "cache_creation_input_tokens": 100, "output_tokens": 0}
		}
		frame := func(kind string, payload map[string]any) string {
			payload["type"] = kind
			return "event: " + kind + "\n" + event(payload)
		}
		body := frame("message_start", map[string]any{"message": start})
		for i, b := range blocks {
			if calls > 0 {
				block := b.(map[string]any)
				block["input"] = map[string]any{}
				body += frame("content_block_start", map[string]any{"index": i, "content_block": block})
				body += frame("content_block_delta", map[string]any{"index": i, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}})
			} else {
				body += frame("content_block_start", map[string]any{"index": i, "content_block": b})
			}
			body += frame("content_block_stop", map[string]any{"index": i})
		}
		delta := map[string]any{"delta": map[string]any{"stop_reason": finish}}
		if usage {
			delta["usage"] = map[string]any{"output_tokens": 12}
		}
		return body + frame("message_delta", delta) + frame("message_stop", map[string]any{})
	case "gemini-generate-content":
		parts := []any{map[string]any{"text": answer}}
		if calls > 0 {
			parts = nil
			for i := 0; i < calls; i++ {
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": fmt.Sprintf("call-%d", i), "name": "calculate", "args": map[string]any{"a": 2, "b": 3}}})
			}
		}
		v := map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}, "finishReason": "STOP"}}}
		if usage {
			v["usageMetadata"] = map[string]any{"promptTokenCount": 1000, "candidatesTokenCount": 12, "cachedContentTokenCount": 600, "thoughtsTokenCount": 0}
		}
		if stream {
			return event(v)
		}
		return protocolJSON(v)
	default:
		panic("unexpected protocol")
	}
}

func TestP2ProtocolContractCalculation(t *testing.T) {
	for _, cfg := range protocolConfigs() {
		for _, stream := range []bool{false, true} {
			for _, n := range []int{1, 2} {
				for _, known := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream_%t/tools_%d/usage_%t", cfg.Protocol, stream, n, known), func(t *testing.T) {
						var physical, observed atomic.Int32
						var requests []map[string]any
						client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
							var request map[string]any
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								return nil, err
							}
							requests = append(requests, request)
							turn := physical.Add(1)
							if turn > 2 {
								t.Error("unexpected extra request")
							}
							calls := 0
							if turn == 1 {
								calls = n
							}
							return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, calls, known)), nil
						})}
						cfg.Capabilities.Items[llm.CapMultipleTools] = cfg.Capabilities.Items[llm.CapTools]
						m := protocolBind(t, cfg, client)
						usage := &usageObserver{}
						ctx := llm.WithUsageObservation(p2ChatContext(t, &observed), usage)
						tools := model.WithTools([]*schema.ToolInfo{{Name: "calculate", Desc: "Add two integers", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"a": {Type: schema.Integer, Required: true}, "b": {Type: schema.Integer, Required: true}})}})
						input := []*schema.AgenticMessage{schema.UserAgenticMessage("Calculate 2+3 using calculate, then answer 5.")}
						msg, err := p2FactoryReplayInvoke(ctx, m, stream, input, tools)
						p2OK(t, err)
						if msg.Extra["seasprak.finish"] != "tool_calls" {
							t.Fatal("tool finish lost")
						}
						msg = p2FactoryReplayJSON(t, msg)
						results := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser}
						executed := 0
						for _, block := range msg.ContentBlocks {
							if call := block.FunctionToolCall; call != nil {
								if call.CallID != fmt.Sprintf("call-%d", executed) || call.Name != "calculate" {
									t.Fatal("call identity changed")
								}
								var args struct{ A, B int }
								p2OK(t, json.Unmarshal([]byte(call.Arguments), &args))
								if args.A != 2 || args.B != 3 {
									t.Fatal("arguments changed")
								}
								executed++
								result := fmt.Sprintf(`{"answer":%d}`, args.A+args.B)
								results.ContentBlocks = append(results.ContentBlocks, schema.NewContentBlock(&schema.FunctionToolResult{CallID: call.CallID, Name: call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: result}}}}))
							}
						}
						if executed != n {
							t.Fatalf("tool executions=%d want=%d", executed, n)
						}
						answer, err := p2FactoryReplayInvoke(ctx, m, stream, append(input, msg, results), tools)
						p2OK(t, err)
						if textOf(answer) != "5" || answer.Extra["seasprak.finish"] != "stop" {
							t.Fatal("final answer mismatch")
						}
						if physical.Load() != 2 || observed.Load() != 2 || len(requests) != 2 {
							t.Fatal("request accounting mismatch")
						}
						protocolAssertReplay(t, cfg.Protocol, requests[1], n)
						usage.mu.Lock()
						defer usage.mu.Unlock()
						if len(usage.snapshots) != 2 {
							t.Fatal("usage must be reported once per model request")
						}
						for _, s := range usage.snapshots {
							u := s.Usage
							if !known {
								if u != (llm.UsageRecord{}) {
									t.Fatal("missing usage became known")
								}
								continue
							}
							if !u.CacheRead.Known || u.CacheRead.Value != 600 || u.InputTotal.Source == "" || u.OutputTotal.Source == "" {
								t.Fatal("cache/input measurement source lost")
							}
							if cfg.Protocol == "anthropic-messages" {
								if !u.CacheWrite.Known || u.CacheWrite.Value != 100 || !u.UncachedInput.Known || u.UncachedInput.Value != 300 {
									t.Fatal("Claude cache write decomposition lost")
								}
							} else if u.CacheWrite.Known {
								t.Fatal("missing cache write fabricated")
							}
							if u.Estimate.Known {
								t.Fatal("unversioned price estimate fabricated")
							}
							if !u.InputTotal.Known || u.InputTotal.Value != 1000 || !u.OutputTotal.Known || u.OutputTotal.Value != 12 {
								t.Fatalf("usage totals=%+v", u)
							}
						}
						if known {
							for _, response := range []*schema.AgenticMessage{msg, answer} {
								if response.ResponseMeta == nil || response.ResponseMeta.TokenUsage == nil || response.ResponseMeta.TokenUsage.PromptTokens != 1000 || response.ResponseMeta.TokenUsage.CompletionTokens != 12 {
									t.Fatal("SDK usage disagrees with normalized usage")
								}
							}
						}
						if !known && answer.ResponseMeta != nil && answer.ResponseMeta.TokenUsage != nil {
							t.Fatal("missing usage synthesized by SDK")
						}
					})
				}
			}
		}
	}
}

func protocolAssertReplay(t *testing.T, protocol string, request map[string]any, n int) {
	t.Helper()
	// Walk the captured wire payload, checking each protocol's actual pairing
	// field, not just the presence of an ID somewhere in serialized JSON.
	calls, results := map[string]int{}, map[string]int{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			kind, _ := v["type"].(string)
			if kind == "function_call" {
				id, _ := v["call_id"].(string)
				calls[id]++
			}
			if kind == "function_call_output" {
				id, _ := v["call_id"].(string)
				results[id]++
				if !strings.Contains(fmt.Sprint(v["output"]), `"answer":5`) {
					t.Error("Responses result lost")
				}
			}
			if kind == "function" && v["function"] != nil && v["id"] != nil {
				id, _ := v["id"].(string)
				calls[id]++
			}
			if v["role"] == "tool" {
				id, _ := v["tool_call_id"].(string)
				results[id]++
				if !strings.Contains(fmt.Sprint(v["content"]), `"answer":5`) {
					t.Error("Chat result lost")
				}
			}
			if kind == "tool_use" {
				id, _ := v["id"].(string)
				calls[id]++
			}
			if kind == "tool_result" {
				id, _ := v["tool_use_id"].(string)
				results[id]++
				if !strings.Contains(fmt.Sprint(v["content"]), `"answer":5`) {
					t.Error("Claude result lost")
				}
			}
			if f, ok := v["functionCall"].(map[string]any); ok {
				id, _ := f["id"].(string)
				calls[id]++
			}
			if f, ok := v["functionResponse"].(map[string]any); ok {
				id, _ := f["id"].(string)
				results[id]++
				response, _ := f["response"].(map[string]any)
				if response["answer"] != float64(5) || f["name"] != "calculate" {
					t.Error("Gemini result lost")
				}
			}
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(request)
	if len(calls) != n || len(results) != n {
		t.Fatalf("%s replay pairing count mismatch", protocol)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call-%d", i)
		if calls[id] != 1 || results[id] != 1 {
			t.Fatal("replay identity must appear exactly once per call/result")
		}
	}
	if protocol == "openai-responses" {
		if request["store"] != false || request["previous_response_id"] != nil {
			t.Fatal("stateful replay enabled")
		}
	}
}

func protocolPrivateBody(kind string, stream bool) string {
	if kind == "responses" {
		item := reasoningFixtureItem("rs_contract", "synthetic-private-signature")
		if stream {
			return reasoningFixtureEvent("added", 0, item) + reasoningFixtureEvent("done", 0, item) + reasoningFixtureTerminal(item)
		}
		return `{"id":"resp_fixture","status":"completed","output":[` + item + `]}`
	}
	if kind == "gemini" {
		body := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"5","thoughtSignature":"c3ludGhldGljLXNpZ25hdHVyZQ=="}]},"finishReason":"STOP"}]}`
		if stream {
			return "data: " + body + "\n\n"
		}
		return body
	}
	block := `{"type":"thinking","thinking":"summary","signature":"synthetic-private-signature"}`
	if kind == "redacted" {
		block = `{"type":"redacted_thinking","data":"synthetic-opaque-data"}`
	}
	body := `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture","content":[` + block + `],"stop_reason":"end_turn"}`
	if !stream {
		return body
	}
	return "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture","content":[]}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":` + block + "}\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

func TestP2ProtocolContractPrivateHistory(t *testing.T) {
	for _, kind := range []string{"claude", "redacted", "responses", "gemini"} {
		for _, sourceStream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/source_stream_%t", kind, sourceStream), func(t *testing.T) {
				source := anthropicConfig()
				if kind == "responses" {
					source = p2ResponsesConfig()
				}
				if kind == "gemini" {
					source = geminiConfig()
				}
				var sourceCalls atomic.Int32
				m := protocolBind(t, source, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					return anthropicResponse(r, sourceStream, protocolPrivateBody(kind, sourceStream)), nil
				})})
				history, err := p2FactoryReplayInvoke(p2ChatContext(t, &sourceCalls), m, sourceStream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				p2OK(t, err)
				if len(history.ContentBlocks) != 1 || sourceCalls.Load() != 1 {
					t.Fatal("private fixture must produce one actual block/request")
				}
				history = p2FactoryReplayJSON(t, history)
				targets := append(protocolConfigs(), source)
				targets[len(targets)-1].Model += "-other"
				for _, cfg := range targets {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/%s/stream_%t", cfg.Protocol, cfg.Model, stream), func(t *testing.T) {
							var physical, observed atomic.Int32
							target := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
								physical.Add(1)
								return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, 0, false)), nil
							})})
							_, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), target, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe"), history, schema.UserAgenticMessage("continue")})
							if cfg.Key() == source.Key() {
								p2OK(t, err)
								if physical.Load() != 1 || observed.Load() != 1 {
									t.Fatal("same model history rejected")
								}
								return
							}
							p2Code(t, err, product.CodeUnsupportedCapability)
							if physical.Load() != 0 || observed.Load() != 0 {
								t.Fatal("private incompatible history reached transport")
							}
						})
					}
				}
			})
		}
	}
}

// Public reasoning is not private replay data. A model switch with ordinary
// text/tool history must remain valid; preserving that history is tested above.
func TestP2ProtocolContractPublicReasoning(t *testing.T) {
	for _, cfg := range protocolConfigs() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", cfg.Protocol, stream), func(t *testing.T) {
				var observed, physical atomic.Int32
				m := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, 0, false)), nil
				})})
				history := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.Reasoning{Text: "public summary"}), schema.NewContentBlock(&schema.AssistantGenText{Text: "5"})}}
				_, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe"), history, schema.UserAgenticMessage("continue")})
				p2OK(t, err)
				if physical.Load() != 1 || observed.Load() != 1 {
					t.Fatal("public reasoning falsely treated as private signature")
				}
			})
		}
	}
}

// Cancellation before admission must not resolve into any provider request.
func TestP2ProtocolContractCanceled(t *testing.T) {
	for _, cfg := range protocolConfigs() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", cfg.Protocol, stream), func(t *testing.T) {
				var physical, observed atomic.Int32
				m := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) { physical.Add(1); return nil, context.Canceled })})
				ctx, cancel := context.WithCancel(p2ChatContext(t, &observed))
				cancel()
				_, err := p2FactoryReplayInvoke(ctx, m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				if err != context.Canceled || physical.Load() != 0 || observed.Load() != 0 {
					t.Fatal("canceled request was admitted")
				}
			})
		}
	}
}
