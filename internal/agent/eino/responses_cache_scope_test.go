package eino

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestResponsesCacheScopeFromExecution(t *testing.T) {
	cfg := llm.ModelConfig{Provider: "openai", Protocol: "openai-responses", Model: "fixture", Endpoint: "https://example.invalid/v1", Version: "v1", AccountScope: "account", NoCredentials: true}
	cap := llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline cache fixture"}}
	cfg.Capabilities.Items = map[llm.CapabilityName]llm.Capability{}
	for _, name := range []llm.CapabilityName{llm.CapText, llm.CapTextStream, llm.CapPhysicalRequestMetering, llm.CapOutputLimit, llm.CapContextWindow, llm.CapCacheShort} {
		cfg.Capabilities.Items[name] = cap
	}
	cfg.Capabilities.ContextWindowTokens = 8192
	cfg.Capabilities.MaxOutputTokens = 128
	cfg.Parameters.PolicyVersion = "v1"
	var keys []string
	c := llm.NewCatalog(nil)
	err := c.RegisterOpenAIResponses(&http.Client{Transport: p2Wire(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		key, _ := body["prompt_cache_key"].(string)
		keys = append(keys, key)
		if key == "" || key == "session-one" || body["store"] != false {
			t.Error("missing private cache key or store=false")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"response","object":"response","status":"completed","model":"fixture","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}`)), Request: r}, nil
	})}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Register(cfg); err != nil {
		t.Fatal(err)
	}
	bound, err := c.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: "short"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []agent.ExecutionScope{{SessionID: "session-one", TurnID: "turn-one"}, {SessionID: "session-one", TurnID: "turn-two"}, {SessionID: "session-two", TurnID: "turn-three"}} {
		budget := agent.NewBudget(config.DefaultLimits())
		validated := NewValidatedModel(bound, &factSink{}, budget, scope)
		_, err = validated.Generate(llm.WithSessionCacheScope(t.Context(), "untrusted-stale-scope"), []*schema.AgenticMessage{schema.UserAgenticMessage("question")})
		if err != nil {
			t.Fatal(err)
		}
		if budget.Snapshot().TransportRequests != 1 {
			t.Fatalf("budget=%+v", budget.Snapshot())
		}
	}
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] == keys[2] {
		t.Fatalf("cache routing did not preserve session isolation: %v", keys)
	}
}
