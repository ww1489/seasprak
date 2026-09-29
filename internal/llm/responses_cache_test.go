package llm_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestResponsesCacheRequestGenerateStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, intent := range []string{"none", "short", "long"} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, intent), func(t *testing.T) {
				cfg := p2ResponsesConfig()
				cap := llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"cache fixture"}}
				cfg.Capabilities.Items[llm.CapCacheShort] = cap
				cfg.Capabilities.Items[llm.CapCacheLong] = cap
				var physical, occupied atomic.Int32
				var payload map[string]any
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						return nil, err
					}
					if !stream {
						return p2ResponsesResponseJSON(r, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("ok"), true)), nil
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2ResponsesSSE("completed", "", p2ResponsesTextOutput("streamed"), true, true))), Request: r}, nil
				})}
				catalog := llm.NewCatalog(nil)
				p2ResponsesRegister(t, catalog, client, 1<<20)
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: intent})
				p2OK(t, err)
				ctx := llm.WithSessionCacheScope(p2ChatContext(t, &occupied), "session-private")
				input := []*schema.AgenticMessage{schema.SystemAgenticMessage("policy"), schema.UserAgenticMessage("question")}
				before, _ := json.Marshal(input)
				if stream {
					r, e := bound.Stream(ctx, input)
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
					_, err = bound.Generate(ctx, input)
					p2OK(t, err)
				}
				after, _ := json.Marshal(input)
				if string(before) != string(after) {
					t.Fatal("request mutated history")
				}
				if physical.Load() != 1 || occupied.Load() != 1 {
					t.Fatalf("physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
				if payload["store"] != false || payload["previous_response_id"] != nil {
					t.Fatal("cache enabled stateful response storage")
				}
				if len(payload["input"].([]any)) != 2 {
					t.Fatal("full history not sent")
				}
				key, _ := payload["prompt_cache_key"].(string)
				if intent == "none" {
					if key != "" || payload["prompt_cache_retention"] != nil {
						t.Fatal("none emitted active cache fields")
					}
				} else {
					if len(key) != 64 || strings.Contains(key, "session") {
						t.Fatalf("cache key=%q", key)
					}
					var retention any // short follows the provider default, as in pi.
					if intent == "long" {
						retention = "24h"
					}
					if payload["prompt_cache_retention"] != retention {
						t.Fatalf("retention=%v", payload["prompt_cache_retention"])
					}
				}
			})
		}
	}
}

func TestResponsesActiveCacheRequiresTrustedScope(t *testing.T) {
	cfg := p2ResponsesConfig()
	cfg.Capabilities.Items[llm.CapCacheShort] = llm.Capability{Status: llm.Verified, AdapterVersion: "fixture", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"cache fixture"}}
	var physical, occupied atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return p2ResponsesResponseJSON(r, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("ok"), true)), nil
	})}
	catalog := llm.NewCatalog(nil)
	p2ResponsesRegister(t, catalog, client, 1<<20)
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{CacheIntent: "short"})
	p2OK(t, err)
	_, err = bound.Generate(p2ChatContext(t, &occupied), []*schema.AgenticMessage{schema.UserAgenticMessage("test")})
	var public *product.Error
	if !errors.As(err, &public) || public.Code != product.CodeInvalidArgument || physical.Load() != 0 || occupied.Load() != 0 {
		t.Fatalf("err=%v physical=%d occupied=%d", err, physical.Load(), occupied.Load())
	}
}
