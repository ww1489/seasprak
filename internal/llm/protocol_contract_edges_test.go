package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// Existing default certification remains authoritative for active cache fields:
// TestP2OpenAIChatOptions, TestResponsesCacheRequestGenerateStream,
// TestAnthropicExplicitCacheBoundaries, TestGeminiExplicitCacheOptionsAndScope;
// thinking: Chat request options, Gemini typed/off, DeepSeekThinkingOptions.
// This file fills the remaining Responses thinking, Claude off, DeepSeek
// implicit-cache intent and shared in-flight cancellation cases only.
func TestP2ProtocolContractRemainingThinkingMappings(t *testing.T) {
	for _, cfg := range []llm.ModelConfig{p2ResponsesConfig(), anthropicConfig()} {
		for _, stream := range []bool{false, true} {
			for _, level := range []string{"off", "low", "bad_off", "bad_on"} {
				t.Run(fmt.Sprintf("%s/%t/%s", cfg.Protocol, stream, level), func(t *testing.T) {
					v := llm.Capability{Status: llm.Verified, AdapterVersion: "pinned-product-factory", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline protocol contract request fixture"}}
					cfg.Capabilities.MaxOutputTokens = 2048
					cfg.Parameters.MaxOutputTokens = 2048
					native, budget := "none", 0
					if level == "low" || level == "bad_off" {
						native = "low"
						if cfg.Protocol == "anthropic-messages" {
							native = "enabled"
							budget = 1024
						}
					}
					cfg.Parameters.DefaultThinking = level
					if level == "bad_off" {
						cfg.Parameters.DefaultThinking = "off"
						budget = 0
					}
					if level == "bad_on" {
						cfg.Parameters.DefaultThinking = "low"
					}
					cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{cfg.Parameters.DefaultThinking: {Capability: v, NativeValue: native, BudgetTokens: budget}}
					var observed, physical atomic.Int32
					var request map[string]any
					m := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
						physical.Add(1)
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							return nil, err
						}
						return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, 0, false)), nil
					})})
					_, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
					if level == "bad_off" || level == "bad_on" {
						p2Code(t, err, product.CodeUnsupportedCapability)
						if physical.Load() != 0 || observed.Load() != 0 {
							t.Fatal("invalid off reached provider")
						}
						return
					}
					p2OK(t, err)
					if observed.Load() != 1 || physical.Load() != 1 {
						t.Fatal("thinking request accounting mismatch")
					}
					if cfg.Protocol == "openai-responses" {
						r, _ := request["reasoning"].(map[string]any)
						if r["effort"] != native {
							t.Fatal("Responses reasoning mapping lost")
						}
					} else {
						r, _ := request["thinking"].(map[string]any)
						want := native
						if want == "none" {
							want = "disabled"
						}
						if r["type"] != want {
							t.Fatal("Claude thinking mapping lost")
						}
					}
				})
			}
		}
	}
}

func TestP2ProtocolContractDeepSeekCacheIntents(t *testing.T) {
	for _, intent := range []string{"none", "short", "long"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", intent, stream), func(t *testing.T) {
				cfg := p2DeepSeekConfig()
				c := llm.NewCatalog(nil)
				var observed, physical atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					for _, key := range []string{"cache_control", "cached_content", "prompt_cache_key", "prompt_cache_retention", "store", "previous_response_id"} {
						if _, ok := request[key]; ok {
							t.Error("implicit cache sent active cache field")
						}
					}
					return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, 0, true)), nil
				})}
				p2OK(t, c.RegisterDeepSeekChat(client, 1<<20))
				p2OK(t, c.Register(cfg))
				m, err := c.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: intent})
				p2OK(t, err)
				effective := m.(interface{ EffectiveOptions() llm.EffectiveOptions }).EffectiveOptions()
				want := intent
				if want == "long" {
					want = "short"
				}
				if effective.RequestedCacheIntent != intent || effective.CacheIntent != want || effective.ActiveCache || !effective.ImplicitCacheMayApply || effective.CacheReason == "" {
					t.Fatal("cache intent/fallback diagnostics mismatch")
				}
				_, err = p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
				p2OK(t, err)
				if physical.Load() != 1 || observed.Load() != 1 {
					t.Fatal("implicit cache must not create auxiliary requests")
				}
			})
		}
	}
}

func TestP2ProtocolContractInFlightCancellation(t *testing.T) {
	for _, cfg := range protocolConfigs() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", cfg.Protocol, stream), func(t *testing.T) {
				var observed, physical atomic.Int32
				started := make(chan struct{}, 4)
				m := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					started <- struct{}{}
					<-r.Context().Done()
					return nil, r.Context().Err()
				})})
				ctx, cancel := context.WithCancel(p2ChatContext(t, &observed))
				defer cancel()
				done := make(chan error, 1)
				go func() {
					_, err := p2FactoryReplayInvoke(ctx, m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
					done <- err
				}()
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not start")
				}
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation cause lost: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not exit")
				}
				if physical.Load() != 1 || observed.Load() != 1 {
					t.Fatal("cancellation retried or lost transport accounting")
				}
			})
		}
	}
}

func TestP2ProtocolContractUnscopedPrivateHistory(t *testing.T) {
	for _, cfg := range protocolConfigs() {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", cfg.Protocol, stream), func(t *testing.T) {
				var observed, physical atomic.Int32
				m := protocolBind(t, cfg, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					return anthropicResponse(r, stream, protocolBody(cfg.Protocol, stream, 0, false)), nil
				})})
				history := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.Reasoning{Signature: "synthetic-unscoped-private-data"})}}
				_, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{history})
				p2Code(t, err, product.CodeUnsupportedCapability)
				if physical.Load() != 0 || observed.Load() != 0 {
					t.Fatal("unscoped signature reached provider")
				}
			})
		}
	}
}
