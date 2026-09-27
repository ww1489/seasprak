package llm_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/llm"
)

func p2DeepSeekConfig() llm.ModelConfig {
	cfg := p2Config()
	cfg.Provider = "deepseek"
	cfg.Protocol = "deepseek-chat"
	cfg.Model = "deepseek-fixture"
	cfg.Endpoint = "https://example.invalid/v1"
	return cfg
}

func p2DeepSeekRegister(t *testing.T, c *llm.Catalog, client *http.Client, limit int) {
	t.Helper()
	p2OK(t, c.RegisterDeepSeekChat(client, limit))
}

func p2DeepSeekResponse(r *http.Request, finish string, usage bool) *http.Response {
	body := map[string]any{
		"id":     "deepseek-fixture",
		"object": "chat.completion",
		"model":  "deepseek-fixture",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "hello"},
			"finish_reason": finish,
		}},
	}
	if usage {
		body["usage"] = map[string]any{
			"prompt_tokens": 10, "completion_tokens": 3,
			"prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": 8,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 1},
		}
	}
	encoded, _ := json.Marshal(body)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(encoded))), Request: r}
}

func TestP2DeepSeekGenerateFactoryAndFinish(t *testing.T) {
	var physical, observed atomic.Int32
	c := llm.NewCatalog(nil)
	cfg := p2DeepSeekConfig()
	p2DeepSeekRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return p2DeepSeekResponse(r, "stop", true), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	msg, err := m.Generate(p2ChatContext(t, &observed), nil)
	p2OK(t, err)
	if msg.Extra["seasprak.finish"] != "stop" || textOf(msg) != "hello" {
		t.Fatalf("normalized response = %#v", msg)
	}
	if physical.Load() != 1 || observed.Load() != 1 {
		t.Fatalf("physical=%d observed=%d", physical.Load(), observed.Load())
	}
}

func TestP2DeepSeekRegistrationBoundary(t *testing.T) {
	c := llm.NewCatalog(nil)
	p2Code(t, c.RegisterDeepSeekChat(nil, 0), "invalid_argument")
	p2OK(t, c.RegisterDeepSeekChat(nil, 4096))
	p2Code(t, c.RegisterDeepSeekChat(nil, 4096), "invalid_argument")
}
