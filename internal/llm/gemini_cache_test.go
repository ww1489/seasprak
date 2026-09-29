package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

// A verified retention capability must not implicitly opt an application into
// creation of billable Gemini cache resources, or disable ordinary generation.
func TestGeminiCacheDefaultRemainsImplicit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, intent := range []string{"", "none", "short", "long"} {
			t.Run(fmt.Sprintf("stream_%t/intent_%s", stream, intent), func(t *testing.T) {
				var physical, occupied atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					physical.Add(1)
					if !strings.Contains(r.URL.Path, "GenerateContent") && !strings.Contains(r.URL.Path, "generateContent") {
						t.Error("implicit caching initiated a resource request")
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if _, ok := body["cachedContent"]; ok {
						t.Error("implicit request carried explicit cache handle")
					}
					if body["systemInstruction"] == nil || body["tools"] == nil {
						t.Error("complete system and tools must be sent")
					}
					contents, _ := body["contents"].([]any)
					if len(contents) != 3 {
						t.Errorf("complete history was truncated: %d contents", len(contents))
					}
					encoded, _ := json.Marshal(body)
					for _, text := range []string{"stable instruction", "prefix question", "prefix answer", "current question", "lookup"} {
						if !strings.Contains(string(encoded), text) {
							t.Errorf("missing full request component %q", text)
						}
					}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, geminiStreamBody("STOP", "answer", false, true))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, geminiBody("STOP", "answer", false, true))
					}
				}))
				defer server.Close()
				cfg := geminiConfig()
				cfg.Endpoint = server.URL
				cap := llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"synthetic retention capability"}}
				cfg.Capabilities.Items[llm.CapCacheShort] = cap
				cfg.Capabilities.Items[llm.CapCacheLong] = cap
				cfg.Capabilities.Items[llm.CapCacheResource] = cap
				catalog := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
					return llm.ResolvedCredential{Secret: "synthetic", AccountScope: cfg.AccountScope, Provider: cfg.Provider, Endpoint: cfg.Endpoint}, nil
				}))
				p2OK(t, catalog.RegisterGeminiGenerateContent(server.Client(), 1<<20))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: intent})
				p2OK(t, err)
				effective := bound.(interface{ EffectiveOptions() llm.EffectiveOptions }).EffectiveOptions()
				if effective.ActiveCache || !effective.ImplicitCacheMayApply {
					t.Fatalf("implicit cache options = %+v", effective)
				}
				if intent != "none" && effective.CacheReason != "gemini_explicit_cache_not_requested" {
					t.Fatalf("cache reason = %q", effective.CacheReason)
				}
				in := []*schema.AgenticMessage{schema.SystemAgenticMessage("stable instruction"), schema.UserAgenticMessage("prefix question"), {Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "prefix answer"})}}, schema.UserAgenticMessage("current question")}
				opts := []model.Option{model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "lookup", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String}})}})}
				for round := 0; round < 2; round++ {
					ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: fmt.Sprint(round), AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { occupied.Add(1); return nil }))
					if stream {
						reader, err := bound.Stream(ctx, in, opts...)
						p2OK(t, err)
						for {
							_, err = reader.Recv()
							if err == io.EOF {
								break
							}
							p2OK(t, err)
						}
						reader.Close()
					} else {
						_, err := bound.Generate(ctx, in, opts...)
						p2OK(t, err)
					}
				}
				if physical.Load() != 2 || occupied.Load() != 2 {
					t.Fatalf("physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
			})
		}
	}
}
