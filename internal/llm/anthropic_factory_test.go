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

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

func anthropicConfig() llm.ModelConfig {
	cfg := p2Config()
	cfg.Provider = "anthropic"
	cfg.Protocol = "anthropic-messages"
	cfg.Endpoint = "https://api.anthropic.com"
	cfg.NoCredentials = false
	cfg.CredentialRef = "anthropic-ref"
	cfg.AccountScope = "anthropic-account"
	return cfg
}

func anthropicCredentialResolver(secret *string) p2Resolver {
	return func(_ context.Context, _ string) (llm.ResolvedCredential, error) {
		return llm.ResolvedCredential{Secret: *secret, AccountScope: "anthropic-account", Provider: "anthropic", Endpoint: "https://api.anthropic.com"}, nil
	}
}

func anthropicBody(finish string, text string, tool bool, usage bool) string {
	content := []any{map[string]any{"type": "text", "text": text}}
	if tool {
		content = []any{map[string]any{"type": "tool_use", "id": "call-a", "name": "lookup", "input": map[string]any{"q": "中文😀"}}}
	}
	response := map[string]any{"id": "msg-fixture", "type": "message", "role": "assistant", "model": "fixture", "content": content, "stop_reason": finish, "stop_sequence": nil}
	if usage {
		response["usage"] = map[string]any{"input_tokens": 1000, "cache_read_input_tokens": 600, "cache_creation_input_tokens": 0, "output_tokens": 12}
	}
	b, _ := json.Marshal(response)
	return string(b)
}

func anthropicStreamBody(finish string, text string, tool bool, usage bool) string {
	messageStart := map[string]any{"type": "message_start", "message": map[string]any{"id": "msg-fixture", "type": "message", "role": "assistant", "model": "fixture", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 1000, "cache_read_input_tokens": 600, "cache_creation_input_tokens": 0, "output_tokens": 0}}}
	frame := func(event string, payload any) string {
		b, _ := json.Marshal(payload)
		return "event: " + event + "\ndata: " + string(b) + "\n\n"
	}
	var out string
	out += frame("message_start", messageStart)
	if tool {
		out += frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call-a", "name": "lookup", "input": map[string]any{}}})
		out += frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"q":"中文😀"}`}})
	} else {
		out += frame("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		out += frame("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	}
	out += frame("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	delta := map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": finish, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 12}}
	if !usage {
		delta["usage"] = map[string]any{"output_tokens": 0}
	}
	out += frame("message_delta", delta)
	out += frame("message_stop", map[string]any{"type": "message_stop"})
	return out
}

func anthropicResponse(r *http.Request, stream bool, body string) *http.Response {
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func anthropicInvoke(ctx context.Context, m llm.Model, stream bool, opts ...model.Option) (*schema.AgenticMessage, error) {
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

func TestAnthropicMessagesFactoryGenerateStreamAndTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream_%t/tool_%t", stream, tool), func(t *testing.T) {
				secret := "synthetic-anthropic-secret"
				cfg := anthropicConfig()
				var physical, occupied atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					if r.Header.Get("x-api-key") != secret {
						t.Error("request did not use the request credential snapshot")
					}
					return anthropicResponse(r, stream, func() string {
						finish := "end_turn"
						if tool {
							finish = "tool_use"
						}
						if stream {
							return anthropicStreamBody(finish, "中文😀", tool, true)
						}
						return anthropicBody(finish, "中文😀", tool, true)
					}()), nil
				})}
				catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
				p2OK(t, catalog.RegisterAnthropicMessages(client, 1<<20))
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
					opts = append(opts, model.WithTools([]*schema.ToolInfo{testkit.ToolInfo("lookup", "look up a value")}))
				}
				msg, err := anthropicInvoke(ctx, bound, stream, opts...)
				p2OK(t, err)
				wantFinish, wantOriginal := "stop", "end_turn"
				if tool {
					wantFinish, wantOriginal = "tool_calls", "tool_use"
				}
				if msg == nil || msg.Extra["seasprak.finish"] != wantFinish || msg.Extra["seasprak.original_finish"] != wantOriginal {
					t.Fatalf("finish metadata = %#v", msg)
				}
				if tool {
					if len(msg.ContentBlocks) == 0 || msg.ContentBlocks[0].FunctionToolCall == nil || msg.ContentBlocks[0].FunctionToolCall.Arguments != `{"q":"中文😀"}` {
						t.Fatal("Anthropic tool call was not preserved")
					}
				} else if len(msg.ContentBlocks) == 0 || msg.ContentBlocks[0].AssistantGenText == nil || msg.ContentBlocks[0].AssistantGenText.Text != "中文😀" {
					t.Fatal("Anthropic text was not preserved")
				}
				if physical.Load() != 1 || occupied.Load() != 1 {
					t.Fatalf("physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
			})
		}
	}
}

func TestAnthropicMessagesFactoryRejectsMissingTerminalAndTruncation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, kind := range []string{"missing_finish", "truncated"} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, kind), func(t *testing.T) {
				secret := "synthetic-anthropic-secret"
				cfg := anthropicConfig()
				limit := 1 << 20
				body := anthropicBody("", "partial", false, true)
				if stream {
					body = anthropicStreamBody("", "partial", false, true)
				}
				if kind == "truncated" {
					limit = 64
				}
				var physical atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					return anthropicResponse(r, stream, body), nil
				})}
				catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
				p2OK(t, catalog.RegisterAnthropicMessages(client, limit))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
				p2OK(t, err)
				_, err = anthropicInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, stream)
				if err == nil {
					t.Fatal("incomplete Anthropic response was accepted")
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

func TestAnthropicMessagesFactoryMapsThinkingAndCacheTypedFields(t *testing.T) {
	secret := "synthetic-anthropic-secret"
	cfg := anthropicConfig()
	cap := llm.Capability{Status: llm.Verified, AdapterVersion: "agenticclaude-v0.1.7", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline typed parameter fixture"}}
	cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: cap, NativeValue: "enabled", BudgetTokens: 1024}}
	cfg.Capabilities.Items[llm.CapCacheShort] = cap
	cfg.Parameters.DefaultThinking = "low"
	cfg.Capabilities.MaxOutputTokens = 2048
	cfg.Parameters.MaxOutputTokens = 2048
	cfg.Capabilities.ThinkingSharesOutput = true
	var request map[string]any
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		request = map[string]any{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		return anthropicResponse(r, false, anthropicBody("end_turn", "done", false, true)), nil
	})}
	catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
	p2OK(t, catalog.RegisterAnthropicMessages(client, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = anthropicInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, false)
	p2OK(t, err)
	thinking, ok := request["thinking"].(map[string]any)
	if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(1024) {
		t.Fatalf("typed Anthropic thinking = %#v", request["thinking"])
	}
	cache, ok := request["cache_control"].(map[string]any)
	if !ok || cache["type"] != "ephemeral" {
		t.Fatalf("typed Anthropic cache = %#v", request["cache_control"])
	}
}

func TestAnthropicMessagesFactoryValidatesThinkingBudgetBeforeRequest(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name       string
			budget     int
			maxTokens  int
			callTokens int
			valid      bool
		}{
			{name: "zero", budget: 0, maxTokens: 2048},
			{name: "below_minimum", budget: 1023, maxTokens: 2048},
			{name: "exceeds_output", budget: 1024, maxTokens: 128},
			{name: "equals_output", budget: 1024, maxTokens: 1024},
			{name: "call_time_equals_output", budget: 1024, maxTokens: 2048, callTokens: 1024},
			{name: "call_time_below_budget", budget: 1024, maxTokens: 2048, callTokens: 128},
			{name: "minimum_valid", budget: 1024, maxTokens: 1025, valid: true},
			{name: "call_time_valid", budget: 1024, maxTokens: 2048, callTokens: 1025, valid: true},
		} {
			t.Run(fmt.Sprintf("stream_%t/%s", stream, tc.name), func(t *testing.T) {
				secret := "synthetic-anthropic-secret"
				cfg := anthropicConfig()
				cfg.Capabilities.MaxOutputTokens = 2048
				cfg.Parameters.MaxOutputTokens = tc.maxTokens
				// Deliberately leave ThinkingSharesOutput false: a configuration
				// declaration cannot bypass the native Messages API constraint.
				cap := llm.Capability{Status: llm.Verified, AdapterVersion: "agenticclaude-v0.1.7", Endpoint: cfg.Endpoint, Model: cfg.Model, ModelVersion: "fixture", ConfigVersion: cfg.Version, Evidence: []string{"offline budget boundary fixture"}}
				cfg.Capabilities.Thinking = map[string]llm.ThinkingSupport{"low": {Capability: cap, NativeValue: "enabled", BudgetTokens: tc.budget}}
				var physical, occupied atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					physical.Add(1)
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					wantMax := tc.maxTokens
					if tc.callTokens != 0 {
						wantMax = tc.callTokens
					}
					thinking, ok := request["thinking"].(map[string]any)
					if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(tc.budget) || request["max_tokens"] != float64(wantMax) {
						t.Error("thinking budget or output limit was changed in the native request")
					}
					body := anthropicBody("end_turn", "done", false, true)
					if stream {
						body = anthropicStreamBody("end_turn", "done", false, true)
					}
					return anthropicResponse(r, stream, body), nil
				})}
				catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
				p2OK(t, catalog.RegisterAnthropicMessages(client, 1<<20))
				p2OK(t, catalog.Register(cfg))
				bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{Thinking: "low"})
				p2OK(t, err)
				ctx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error {
					occupied.Add(1)
					return nil
				}))
				var opts []model.Option
				if tc.callTokens != 0 {
					opts = append(opts, model.WithMaxTokens(tc.callTokens))
				}
				msg, err := anthropicInvoke(ctx, bound, stream, opts...)
				if tc.valid {
					p2OK(t, err)
					if msg == nil || physical.Load() != 1 || occupied.Load() != 1 {
						t.Fatalf("valid request: message=%v physical=%d occupied=%d", msg != nil, physical.Load(), occupied.Load())
					}
					return
				}
				if physical.Load() != 0 || occupied.Load() != 0 {
					t.Errorf("invalid budget reached transport: physical=%d occupied=%d", physical.Load(), occupied.Load())
				}
				p2Code(t, err, product.CodeInvalidArgument)
				if msg != nil {
					t.Fatal("invalid budget returned a message")
				}
			})
		}
	}
}

func TestAnthropicMessagesFactoryRejectsUnsupportedRoutesAndServerTools(t *testing.T) {
	for _, provider := range []string{"anthropic-bedrock", "anthropic-vertex", "anthropic-private"} {
		t.Run(provider, func(t *testing.T) {
			secret := "synthetic-anthropic-secret"
			cfg := anthropicConfig()
			cfg.Provider = provider
			var physical atomic.Int32
			catalog := llm.NewCatalog(p2Resolver(func(_ context.Context, _ string) (llm.ResolvedCredential, error) {
				return llm.ResolvedCredential{Secret: secret, AccountScope: "anthropic-account", Provider: provider, Endpoint: cfg.Endpoint}, nil
			}))
			p2OK(t, catalog.RegisterAnthropicMessages(&http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				return anthropicResponse(r, false, anthropicBody("end_turn", "unexpected", false, true)), nil
			})}, 1<<20))
			p2OK(t, catalog.Register(cfg))
			bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			_, err = anthropicInvoke(llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil })), bound, false)
			p2Code(t, err, product.CodeUnsupportedCapability)
			if physical.Load() != 0 {
				t.Fatal("unsupported provider route sent a request")
			}
		})
	}
	secret := "synthetic-anthropic-secret"
	cfg := anthropicConfig()
	catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
	p2OK(t, catalog.RegisterAnthropicMessages(&http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return anthropicResponse(r, false, anthropicBody("end_turn", "unexpected", false, true)), nil
	})}, 1<<20))
	p2OK(t, catalog.Register(cfg))
	bound, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	_, err = bound.Generate(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}, agenticclaude.WithServerTools(nil))
	p2Code(t, err, product.CodeUnsupportedCapability)
}

func TestAnthropicMessagesFactoryCredentialSnapshotAndClose(t *testing.T) {
	secret := "synthetic-anthropic-first"
	cfg := anthropicConfig()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var mu sync.Mutex
	seen := []string{}
	var requestNo atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		seen = append(seen, r.Header.Get("x-api-key"))
		mu.Unlock()
		if requestNo.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return nil, r.Context().Err()
		}
		return anthropicResponse(r, false, anthropicBody("end_turn", "done", false, true)), nil
	})}
	catalog := llm.NewCatalog(anthropicCredentialResolver(&secret))
	p2OK(t, catalog.RegisterAnthropicMessages(client, 1<<20))
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
	secret = "synthetic-anthropic-second"
	secondCtx := llm.WithRequestObservation(t.Context(), llm.RequestIdentity{ModelCallID: "call-2", AttemptID: "attempt-2", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
	if _, err := bound.Generate(secondCtx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) < 2 || seen[0] != "synthetic-anthropic-first" || seen[1] != "synthetic-anthropic-second" {
		t.Fatalf("credential snapshots were not isolated: %d requests", len(seen))
	}
}

func TestAnthropicMessagesFactoryRejectsInvalidRegistration(t *testing.T) {
	catalog := llm.NewCatalog(nil)
	if err := catalog.RegisterAnthropicMessages(nil, 0); err == nil {
		t.Fatal("invalid response limit was accepted")
	}
	cfg := anthropicConfig()
	cfg.Capabilities.Items[llm.CapServerTools] = cfg.Capabilities.Items[llm.CapText]
	if err := catalog.Register(cfg); err != nil {
		t.Fatal(err)
	}
}
