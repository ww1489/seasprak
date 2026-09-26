package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func p2ChatVerified(cfg llm.ModelConfig) llm.Capability {
	return llm.Capability{Status: llm.Verified, AdapterVersion: "agenticopenai-v0.2.4", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture-v1", ConfigVersion: cfg.Version, Evidence: []string{"offline Chat field fixture"}}
}

func TestP2OpenAIChatOptions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, name := range []string{"default", "low", "off", "bad_off", "invalid_native", "budget", "short", "long", "none", "fallback"} {
			t.Run(fmt.Sprintf("%t/%s", stream, name), func(t *testing.T) {
				cfg := p2ChatConfig()
				v := p2ChatVerified(cfg)
				cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: v, NativeValue: "low"}, "off": {Capability: v, NativeValue: "none"}}
				requested := llm.RequestedOptions{}
				wantEffort, wantCache := "", ""
				reject := false
				switch name {
				case "low":
					requested.Thinking = "low"
					wantEffort = "low"
				case "off":
					requested.Thinking = "off"
					wantEffort = "none"
				case "bad_off":
					requested.Thinking = "off"
					cfg.Capabilities.Thinking["off"] = llm.ThinkingSupport{Capability: v, NativeValue: "low"}
					reject = true
				case "invalid_native":
					requested.Thinking = "low"
					cfg.Capabilities.Thinking["low"] = llm.ThinkingSupport{Capability: v, NativeValue: "arbitrary"}
					reject = true
				case "budget":
					requested.Thinking = "low"
					cfg.Capabilities.Thinking["low"] = llm.ThinkingSupport{Capability: v, NativeValue: "low", BudgetTokens: 10}
					reject = true
				case "short":
					cfg.Capabilities.Items[llm.CapCacheShort] = v
					requested.CacheIntent = "short"
					wantCache = "in_memory"
				case "long":
					cfg.Capabilities.Items[llm.CapCacheLong] = v
					requested.CacheIntent = "long"
					wantCache = "24h"
				case "none":
					cfg.Capabilities.Items[llm.CapCacheShort] = v
					requested.CacheIntent = "none"
				case "fallback":
					requested.CacheIntent = "long"
				}
				c := llm.NewCatalog(nil)
				var physical, observations atomic.Int32
				p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					var payload map[string]any
					if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
						return nil, e
					}
					if payload["max_completion_tokens"] != float64(64) {
						t.Error("resolved lowered output limit not honored")
					}
					if _, ok := payload["max_tokens"]; ok {
						t.Error("conflicting legacy output limit was sent")
					}
					if wantEffort == "" {
						if _, ok := payload["reasoning_effort"]; ok {
							t.Error("unsupported thinking field sent")
						}
					} else if payload["reasoning_effort"] != wantEffort {
						t.Error("thinking mapping mismatch")
					}
					if wantCache == "" {
						if _, ok := payload["prompt_cache_retention"]; ok {
							t.Error("unsupported active cache field sent")
						}
					} else if payload["prompt_cache_retention"] != wantCache {
						t.Error("cache retention mapping mismatch")
					}
					for _, key := range []string{"prompt_cache_key", "store", "previous_response_id"} {
						if _, ok := payload[key]; ok {
							t.Errorf("unexpected request field %s", key)
						}
					}
					return p2ChatResponse(r, stream, p2ChatBody(stream, "stop", "text", false, false, false)), nil
				})}, 1<<20)
				p2OK(t, c.Register(cfg))
				m, e := c.Bind(cfg.Key(), requested)
				p2OK(t, e)
				_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")}, model.WithMaxTokens(64))
				if reject {
					p2Code(t, e, product.CodeUnsupportedCapability)
					if physical.Load() != 0 || observations.Load() != 0 {
						t.Fatal("unsupported options sent request")
					}
				} else {
					p2OK(t, e)
					if physical.Load() != 1 || observations.Load() != 1 {
						t.Fatal("request count mismatch")
					}
				}
			})
		}
	}
}

func TestP2OpenAIChatCredentialsRetriesAndRedaction(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := p2ChatConfig()
			cfg.NoCredentials = false
			cfg.CredentialRef = "synthetic/reference"
			credential := llm.ResolvedCredential{Secret: "synthetic-first", AccountScope: cfg.AccountScope, Provider: cfg.Provider, Endpoint: cfg.Endpoint}
			resolves := 0
			var status atomic.Int32
			status.Store(200)
			var physical, observations atomic.Int32
			c := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) { resolves++; return credential, nil }))
			p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+credential.Secret {
					t.Error("credential rotation lost")
				}
				response := p2ChatResponse(r, stream, p2ChatBody(stream, "stop", "text", false, false, false))
				response.StatusCode = int(status.Load())
				if response.StatusCode != 200 {
					response.Header.Set("Content-Type", "application/json")
					response.Body = io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic-first synthetic-second","type":"fixture"}}`))
				}
				return response, nil
			})}, 1<<20)
			p2OK(t, c.Register(cfg))
			m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, e)
			if resolves != 0 {
				t.Fatal("credentials resolved before request")
			}
			for _, key := range []string{"synthetic-first", "synthetic-second"} {
				credential.Secret = key
				_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
				p2OK(t, e)
			}
			for _, code := range []int{401, 429, 503} {
				status.Store(int32(code))
				before := physical.Load()
				_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
				p2Code(t, e, product.CodeResourceUnavailable)
				if strings.Contains(e.Error(), "synthetic-") {
					t.Fatal("provider error leaked sensitive content")
				}
				if physical.Load() != before+1 {
					t.Fatal("SDK retried a physical request")
				}
			}
			credential.Secret = ""
			_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, nil)
			p2Code(t, e, product.CodeUnauthenticated)
			if physical.Load() != 5 || observations.Load() != 5 || resolves != 6 {
				t.Fatal("credential or occupancy counts mismatch")
			}
		})
	}
}

func TestP2OpenAIChatToolMultimodalFidelity(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			cfg := p2ChatConfig()
			cfg.Capabilities.Items[llm.CapInputImage] = cfg.Capabilities.Items[llm.CapText]
			in := []*schema.AgenticMessage{
				schema.SystemAgenticMessage("system"),
				{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputText{Text: "中文😀"}), schema.NewContentBlock(&schema.UserInputImage{URL: "https://image.invalid/a.png"})}},
				{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "previous", Name: "lookup", Arguments: `{"q":"旧"}`})}},
				{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "previous", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "结果"}}}})}},
			}
			before, _ := json.Marshal(in)
			c := llm.NewCatalog(nil)
			var physical, observations atomic.Int32
			p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				var payload map[string]any
				if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
					return nil, e
				}
				messages, ok := payload["messages"].([]any)
				if !ok || len(messages) != 4 {
					t.Error("history ordering lost")
					return nil, errors.New("fixture request mismatch")
				}
				if messages[0].(map[string]any)["content"] != "system" {
					t.Error("system content lost")
				}
				content := messages[1].(map[string]any)["content"].([]any)
				if len(content) != 2 || content[0].(map[string]any)["text"] != "中文😀" || content[1].(map[string]any)["image_url"].(map[string]any)["url"] != "https://image.invalid/a.png" {
					t.Error("multimodal input changed")
				}
				call := messages[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
				if call["id"] != "previous" || call["function"].(map[string]any)["arguments"] != `{"q":"旧"}` {
					t.Error("tool replay changed")
				}
				result := messages[3].(map[string]any)
				if result["tool_call_id"] != "previous" || result["content"] != "结果" {
					t.Error("tool result pairing lost")
				}
				tools := payload["tools"].([]any)
				if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "lookup" {
					t.Error("tool definition lost")
				}
				return p2ChatResponse(r, stream, p2ChatBody(stream, "tool_calls", "", false, true, true)), nil
			})}, 1<<20)
			p2OK(t, c.Register(cfg))
			m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, e)
			tool := &schema.ToolInfo{Name: "lookup", Desc: "test tool", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String, Required: true}})}
			_, e = p2ChatInvoke(p2ChatContext(t, &observations), m, stream, in, model.WithTools([]*schema.ToolInfo{tool}))
			p2OK(t, e)
			after, _ := json.Marshal(in)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("adapter mutated history")
			}
			if physical.Load() != 1 || observations.Load() != 1 {
				t.Fatal("request count mismatch")
			}
		})
	}
}

func TestP2OpenAIChatMultipleTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			c := llm.NewCatalog(nil)
			cfg := p2ChatConfig()
			cfg.Capabilities.Items[llm.CapMultipleTools] = cfg.Capabilities.Items[llm.CapTools]
			var observed, physical atomic.Int32
			p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				calls := `[{"index":0,"id":"first","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"中文\"}"}},{"index":1,"id":"second","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"😀\"}"}}]`
				body := `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":` + calls + `},"finish_reason":"tool_calls"}]}`
				if stream {
					body = "data: " + `{"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":` + calls + `},"finish_reason":null}]}` + "\n\ndata: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"
				}
				return p2ChatResponse(r, stream, body), nil
			})}, 4096)
			p2OK(t, c.Register(cfg))
			m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, e)
			msg, e := p2ChatInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
			p2OK(t, e)
			got := map[string]string{}
			for _, b := range msg.ContentBlocks {
				if b.FunctionToolCall != nil {
					got[b.FunctionToolCall.CallID] = b.FunctionToolCall.Arguments
				}
			}
			if !reflect.DeepEqual(got, map[string]string{"first": `{"q":"中文"}`, "second": `{"q":"😀"}`}) || msg.Extra["seasprak.finish"] != "tool_calls" || physical.Load() != 1 || observed.Load() != 1 {
				t.Fatal("multiple tool identities or arguments lost")
			}
		})
	}
}

func TestP2OpenAIChatRegistrationBoundary(t *testing.T) {
	c := llm.NewCatalog(nil)
	p2Code(t, c.RegisterOpenAIChat(nil, 0), product.CodeInvalidArgument)
	p2OK(t, c.RegisterOpenAIChat(nil, 4096))
	p2Code(t, c.RegisterOpenAIChat(nil, 4096), product.CodeInvalidArgument)
	for _, protocol := range []string{"openai-responses", "anthropic-messages", "gemini-generate-content", "deepseek-chat"} {
		cfg := p2Config()
		cfg.Protocol = protocol
		p2OK(t, c.Register(cfg))
		_, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
		p2Code(t, e, product.CodeUnsupportedCapability)
	}
}

func TestP2OpenAIChatRejectBeforeRequest(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ChatConfig()
	var physical, observations atomic.Int32
	p2ChatRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return nil, errors.New("unexpected transport")
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, e := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, e)
	ctx := p2ChatContext(t, &observations)
	_, e = m.Generate(ctx, []*schema.AgenticMessage{{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.UserInputImage{URL: "https://image.invalid"})}}})
	p2Code(t, e, product.CodeUnsupportedCapability)
	denied := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
		return product.NewError(product.CodeBudgetExhausted, "denied")
	}))
	_, e = m.Generate(denied, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2Code(t, e, product.CodeBudgetExhausted)
	_, e = m.Generate(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2Code(t, e, product.CodeInvalidArgument)
	if physical.Load() != 0 || observations.Load() != 0 {
		t.Fatal("rejected request reached provider")
	}
}
