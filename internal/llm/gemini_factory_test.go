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
	"time"

	"github.com/cloudwego/eino-ext/components/model/agenticgemini"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func geminiConfig() llm.ModelConfig {
	cfg := p2Config()
	cfg.Provider = "google"
	cfg.Protocol = "gemini-generate-content"
	cfg.Endpoint = "https://generativelanguage.googleapis.com"
	cfg.NoCredentials = false
	cfg.CredentialRef = "gemini-ref"
	cfg.AccountScope = "gemini-account"
	return cfg
}

func geminiCredentialResolver(secret *string) p2Resolver {
	return func(_ context.Context, _ string) (llm.ResolvedCredential, error) {
		return llm.ResolvedCredential{Secret: *secret, AccountScope: "gemini-account", Provider: "google", Endpoint: "https://generativelanguage.googleapis.com"}, nil
	}
}

func geminiBody(finish string, text string, tool bool, usage bool) string {
	var parts []any
	if tool {
		parts = []any{map[string]any{"functionCall": map[string]any{"name": "lookup", "args": map[string]any{"q": "中文😀"}}}}
	} else {
		parts = []any{map[string]any{"text": text}}
	}
	response := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": parts}, "finishReason": finish, "index": 0}}}
	if usage {
		response["usageMetadata"] = map[string]any{"promptTokenCount": 1000, "candidatesTokenCount": 12, "thoughtsTokenCount": 0, "cachedContentTokenCount": 600, "totalTokenCount": 1012}
	}
	b, _ := json.Marshal(response)
	return string(b)
}

func geminiStreamBody(finish string, text string, tool bool, usage bool) string {
	return "data: " + geminiBody(finish, text, tool, usage) + "\n\n"
}

func geminiResponse(r *http.Request, stream bool, body string) *http.Response {
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func geminiInvoke(ctx context.Context, m llm.Model, stream bool, opts ...model.Option) (*schema.AgenticMessage, error) {
	input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
	if !stream {
		return m.Generate(ctx, input, opts...)
	}
	r, err := m.Stream(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, recvErr := r.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return nil, recvErr
		}
		chunks = append(chunks, chunk)
	}
	return schema.ConcatAgenticMessages(chunks)
}

func TestGeminiGenerateContentFactoryGenerateStreamAndTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream_%t/tool_%t", stream, tool), func(t *testing.T) {
				secret := "synthetic-gemini-secret"
				cfg := geminiConfig()
				var physical, occupied atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					if r.Header.Get("x-goog-api-key") != secret {
						t.Error("request did not use the request credential snapshot")
					}
					return geminiResponse(r, stream, geminiBody("STOP", "中文😀", tool, true)), nil
				})}
				if stream {
					client.Transport = p2RoundTripper(func(r *http.Request) (*http.Response, error) {
						physical.Add(1)
						if r.Header.Get("x-goog-api-key") != secret {
							t.Error("request did not use the request credential snapshot")
						}
						return geminiResponse(r, true, geminiStreamBody("STOP", "中文😀", tool, true)), nil
					})
				}
				catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
				p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, err)
				ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
					occupied.Add(1)
					return nil
				}))
				ctx = llm.WithUsageObservation(ctx, &usageObserver{})
				var opts []model.Option
				if tool {
					opts = append(opts, model.WithTools([]*schema.ToolInfo{{Name: "lookup", Desc: "look up a value", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String}})}}))
				}
				if tool {
					_, err := geminiInvoke(ctx, bound, stream, opts...)
					p2Code(t, err, product.CodeUnsupportedCapability)
					if physical.Load() != 0 || occupied.Load() != 0 {
						t.Fatalf("unsupported Gemini tools sent a request: physical=%d occupied=%d", physical.Load(), occupied.Load())
					}
					return
				}
				msg, err := geminiInvoke(ctx, bound, stream, opts...)
				p2OK(t, err)
				if msg == nil || msg.Extra["seasprak.finish"] != "stop" || msg.Extra["seasprak.original_finish"] != "STOP" {
					t.Fatalf("finish metadata = %#v", msg)
				}
				if len(msg.ContentBlocks) == 0 || msg.ContentBlocks[0].AssistantGenText == nil || msg.ContentBlocks[0].AssistantGenText.Text != "中文😀" {
					t.Fatal("Gemini text was not preserved")
				}
				if physical.Load() != 1 || occupied.Load() != 1 {
					t.Fatalf("physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
			})
		}
	}
}

func TestGeminiGenerateContentFactoryUsesOneSDKAttempt(t *testing.T) {
	secret := "synthetic-gemini-secret"
	cfg := geminiConfig()
	var physical atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":503,"message":"synthetic"}}`)), Request: r}, nil
	})}
	catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
	p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = geminiInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, false)
	if err == nil || physical.Load() != 1 {
		t.Fatalf("Gemini retry policy did not enforce one attempt: err=%v physical=%d", err, physical.Load())
	}
}

func TestGeminiGenerateContentFactoryRejectsMissingTerminalAndTruncation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"missing_finish", "truncated"} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, kind), func(t *testing.T) {
				secret := "synthetic-gemini-secret"
				cfg := geminiConfig()
				limit := 1 << 20
				body := geminiBody("FINISH_REASON_UNSPECIFIED", "partial", false, true)
				if stream {
					body = geminiStreamBody("FINISH_REASON_UNSPECIFIED", "partial", false, true)
				}
				if kind == "truncated" {
					limit = 64
				}
				var physical atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					return geminiResponse(r, stream, body), nil
				})}
				catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
				p2OK(t, catalog.RegisterGeminiGenerateContent(client, limit))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, err)
				_, err = geminiInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, stream)
				if err == nil {
					t.Fatal("incomplete Gemini response was accepted")
				}
				if kind == "missing_finish" {
					p2Code(t, err, product.CodeInvalidArgument)
				}
				if physical.Load() != 1 {
					t.Fatalf("physical requests=%d", physical.Load())
				}
			})
		}
	}
}

func TestGeminiGenerateContentFactoryMapsThinkingTypedField(t *testing.T) {
	secret := "synthetic-gemini-secret"
	cfg := geminiConfig()
	cap := llm.Capability{Status: llm.Verified, AdapterVersion: "agenticgemini-v0.2.5", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline typed parameter fixture"}}
	cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: cap, NativeValue: "low", BudgetTokens: 0}}
	cfg.Parameters.DefaultThinking = "low"
	var request map[string]any
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		request = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		return geminiResponse(r, false, geminiBody("STOP", "done", false, true)), nil
	})}
	catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
	p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = geminiInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, false)
	p2OK(t, err)
	config, ok := request["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing Gemini generationConfig: %#v", request)
	}
	thinking, ok := config["thinkingConfig"].(map[string]any)
	if !ok || thinking["thinkingLevel"] != "LOW" || thinking["includeThoughts"] != true {
		t.Fatalf("typed Gemini thinking = %#v", config["thinkingConfig"])
	}
}

func TestGeminiGenerateContentFactoryHonorsThinkingOff(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			modelName string
			requested string
			native    string
			verified  bool
			valid     bool
		}{
			{name: "flash_off", modelName: "gemini-2.5-flash", requested: "off", native: "none", verified: true, valid: true},
			{name: "flash_lite_off", modelName: "gemini-2.5-flash-lite", requested: "off", native: "none", verified: true, valid: true},
			{name: "unspecified", modelName: "gemini-2.5-flash", valid: true},
			{name: "unknown_model", modelName: "unknown", requested: "off", native: "none", verified: true},
			{name: "pro_cannot_disable", modelName: "gemini-2.5-pro", requested: "off", native: "none", verified: true},
			{name: "gemini3_cannot_disable", modelName: "gemini-3-flash-preview", requested: "off", native: "none", verified: true},
			{name: "unverified_off", modelName: "gemini-2.5-flash", requested: "off", native: "none"},
			{name: "off_is_not_minimal", modelName: "gemini-2.5-flash", requested: "off", native: "minimal", verified: true},
			{name: "positive_is_not_none", modelName: "gemini-2.5-flash", requested: "low", native: "none", verified: true},
		} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, tc.name), func(t *testing.T) {
				secret := "synthetic-gemini-secret"
				cfg := geminiConfig()
				cfg.Model = tc.modelName
				if tc.requested != "" {
					cap := llm.Capability{Status: llm.Declared, AdapterVersion: "agenticgemini-v0.2.5", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline typed off fixture, not live model certification"}}
					if tc.verified {
						cap.Status = llm.Verified
					}
					cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{tc.requested: {Capability: cap, NativeValue: tc.native}}
				}
				var physical, occupied atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					config, _ := request["generationConfig"].(map[string]any)
					thinking, hasThinking := config["thinkingConfig"].(map[string]any)
					if tc.requested == "" {
						if hasThinking {
							t.Error("unspecified thinking unexpectedly changed provider defaults")
						}
					} else if tc.valid {
						if !hasThinking || thinking["thinkingBudget"] != float64(0) {
							t.Errorf("explicit off did not send thinkingBudget zero: %#v", thinking)
						}
						if _, present := thinking["thinkingLevel"]; present || thinking["includeThoughts"] == true {
							t.Error("off request enabled thinking level or thought summaries")
						}
					}
					body := geminiBody("STOP", "done", false, true)
					if stream {
						body = geminiStreamBody("STOP", "done", false, true)
					}
					return geminiResponse(r, stream, body), nil
				})}
				catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
				p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{Thinking: tc.requested})
				var msg *schema.AgenticMessage
				if err == nil {
					ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
						occupied.Add(1)
						return nil
					}))
					msg, err = geminiInvoke(ctx, bound, stream)
				}
				if tc.valid {
					p2OK(t, err)
					if msg == nil || physical.Load() != 1 || occupied.Load() != 1 {
						t.Fatalf("valid request: message=%v physical=%d occupied=%d", msg != nil, physical.Load(), occupied.Load())
					}
					return
				}
				if physical.Load() != 0 || occupied.Load() != 0 {
					t.Errorf("unsupported thinking reached transport: physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
				p2Code(t, err, product.CodeUnsupportedCapability)
				if msg != nil {
					t.Fatal("unsupported thinking returned a message")
				}
			})
		}
	}
}

func TestGeminiGenerateContentRejectsUnsupportedRoutesAndServerTools(t *testing.T) {
	for _, provider := range []string{"google-vertex", "google-private"} {
		t.Run(provider, func(t *testing.T) {
			secret := "synthetic-gemini-secret"
			cfg := geminiConfig()
			cfg.Provider = provider
			var physical atomic.Int32
			catalog := llm.NewCatalog(p2Resolver(func(_ context.Context, _ string) (llm.ResolvedCredential, error) {
				return llm.ResolvedCredential{Secret: secret, AccountScope: "gemini-account", Provider: provider, Endpoint: cfg.Endpoint}, nil
			}))
			p2OK(t, catalog.RegisterGeminiGenerateContent(&http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				return geminiResponse(r, false, geminiBody("STOP", "unexpected", false, true)), nil
			})}, 1<<20))
			p2OK(t, catalog.Register(cfg))
			bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			_, err = geminiInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, false)
			p2Code(t, err, product.CodeUnsupportedCapability)
			if physical.Load() != 0 {
				t.Fatal("unsupported provider route sent a request")
			}
		})
	}
	secret := "synthetic-gemini-secret"
	cfg := geminiConfig()
	catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
	p2OK(t, catalog.RegisterGeminiGenerateContent(&http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return geminiResponse(r, false, geminiBody("STOP", "unexpected", false, true)), nil
	})}, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = bound.Generate(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}, agenticgemini.WithServerTools(nil))
	p2Code(t, err, product.CodeUnsupportedCapability)
}

func TestGeminiGenerateContentFactoryCredentialSnapshotAndClose(t *testing.T) {
	secret := "synthetic-gemini-first"
	cfg := geminiConfig()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var mu sync.Mutex
	seen := []string{}
	var requestNo atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Header.Get("x-goog-api-key"))
		mu.Unlock()
		if requestNo.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return nil, r.Context().Err()
		}
		return geminiResponse(r, false, geminiBody("STOP", "done", false, true)), nil
	})}
	catalog := llm.NewCatalog(geminiCredentialResolver(&secret))
	p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	ctx, cancel := context.WithCancel(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })))
	defer cancel()
	type streamResult struct {
		reader *schema.StreamReader[*schema.AgenticMessage]
		err    error
	}
	streamDone := make(chan streamResult, 1)
	go func() {
		r, streamErr := bound.Stream(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
		streamDone <- streamResult{reader: r, err: streamErr}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream request did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancelling stream did not cancel the transport")
	}
	select {
	case result := <-streamDone:
		if result.reader != nil {
			result.reader.Close()
		}
	case <-time.After(time.Second):
		t.Fatal("stream call did not return after cancellation")
	}
	secret = "synthetic-gemini-second"
	secondCtx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call-2", AttemptID: "attempt-2", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
	if _, err := bound.Generate(secondCtx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "synthetic-gemini-first" || seen[1] != "synthetic-gemini-second" {
		t.Fatalf("credential snapshots were not isolated: %d requests", len(seen))
	}
}

func TestGeminiGenerateContentFactoryRejectsInvalidRegistration(t *testing.T) {
	catalog := llm.NewCatalog(nil)
	if err := catalog.RegisterGeminiGenerateContent(nil, 0); err == nil {
		t.Fatal("invalid response limit was accepted")
	}
}
