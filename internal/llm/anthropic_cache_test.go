package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticclaude"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestAnthropicExplicitCacheBoundaries(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, intent := range []string{"none", "short", "long"} {
			for _, toolResult := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/%s/result=%t", stream, intent, toolResult), func(t *testing.T) {
					secret := "synthetic-anthropic-secret"
					cfg := anthropicConfig()
					cap := llm.Capability{Status: llm.Verified, AdapterVersion: "agenticclaude-v0.1.7", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline explicit cache request"}}
					cfg.Capabilities.Items[llm.CapCacheShort] = cap
					cfg.Capabilities.Items[llm.CapCacheLong] = cap
					input := []*schema.AgenticMessage{schema.SystemAgenticMessage("system one"), schema.SystemAgenticMessage("system two"), schema.UserAgenticMessage("question")}
					if toolResult {
						input = append(input, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolCall, FunctionToolCall: &schema.FunctionToolCall{CallID: "call-a", Name: "lookup", Arguments: `{}`}}}}, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{{Type: schema.ContentBlockTypeFunctionToolResult, FunctionToolResult: &schema.FunctionToolResult{CallID: "call-a", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: "result"}}}}}}})
					}
					// Existing caller annotations must not override the resolved policy or
					// accumulate into more than the provider's permitted breakpoints.
					ctrl := anthropic.NewCacheControlEphemeralParam()
					for _, msg := range input {
						for i, b := range msg.ContentBlocks {
							if msg.Role != schema.AgenticRoleTypeAssistant {
								msg.ContentBlocks[i] = agenticclaude.SetContentBlockCacheControl(b, &ctrl)
							}
						}
					}
					tools := []*schema.ToolInfo{agenticclaude.SetToolInfoCacheControl(testkit.ToolInfo("first", "first tool"), &ctrl), agenticclaude.SetToolInfoCacheControl(testkit.ToolInfo("lookup", "lookup tool"), &ctrl)}
					before, _ := json.Marshal(input)
					toolsBefore, _ := json.Marshal(tools)
					var body map[string]any
					physical, occupied := 0, 0
					client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
						physical++
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							return nil, err
						}
						response := anthropicBody("end_turn", "done", false, true)
						if stream {
							response = anthropicStreamBody("end_turn", "done", false, true)
						}
						return anthropicResponse(r, stream, response), nil
					})}
					catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
					p2OK(t, catalog.RegisterAnthropicMessages(client, 1<<20))
					p2OK(t, catalog.Register(cfg))
					bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: intent})
					p2OK(t, err)
					ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "cache", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { occupied++; return nil }))
					opts := []model.Option{model.WithTools(tools)}
					if stream {
						r, e := bound.Stream(ctx, input, opts...)
						p2OK(t, e)
						defer r.Close()
						for {
							_, e = r.Recv()
							if e == io.EOF {
								break
							}
							p2OK(t, e)
						}
					} else {
						_, err = bound.Generate(ctx, input, opts...)
						p2OK(t, err)
					}
					if physical != 1 || occupied != 1 {
						t.Fatalf("physical=%d occupied=%d", physical, occupied)
					}
					after, _ := json.Marshal(input)
					toolsAfter, _ := json.Marshal(tools)
					if string(before) != string(after) || string(toolsBefore) != string(toolsAfter) {
						t.Fatal("cache decoration mutated caller history/tools")
					}
					if _, exists := body["cache_control"]; exists {
						t.Fatal("top-level auto caching must not coexist with explicit policy")
					}
					marks := 0
					var visit func(any)
					visit = func(v any) {
						switch x := v.(type) {
						case map[string]any:
							for k, v := range x {
								if k == "cache_control" {
									marks++
									c := v.(map[string]any)
									var ttl any // short omits TTL and uses the service default.
									if intent == "long" {
										ttl = "1h"
									}
									if c["type"] != "ephemeral" || c["ttl"] != ttl {
										t.Errorf("cache control=%v", c)
									}
								} else {
									visit(v)
								}
							}
						case []any:
							for _, v := range x {
								visit(v)
							}
						}
					}
					visit(body)
					want := 3
					if intent == "none" {
						want = 0
					}
					if marks != want {
						t.Fatalf("cache marks=%d want=%d", marks, want)
					}
					if intent != "none" {
						systems := body["system"].([]any)
						if _, ok := systems[len(systems)-1].(map[string]any)["cache_control"]; !ok {
							t.Error("last system block lacks cache mark")
						}
						ts := body["tools"].([]any)
						if _, ok := ts[len(ts)-1].(map[string]any)["cache_control"]; !ok {
							t.Error("last tool lacks cache mark")
						}
						msgs := body["messages"].([]any)
						last := msgs[len(msgs)-1].(map[string]any)
						blocks := last["content"].([]any)
						block := blocks[len(blocks)-1].(map[string]any)
						if _, ok := block["cache_control"]; !ok {
							t.Error("last user block lacks cache mark")
						}
						if toolResult && block["type"] != "tool_result" {
							t.Fatal("tool result cache boundary was lost")
						}
					}
				})
			}
		}
	}
}
