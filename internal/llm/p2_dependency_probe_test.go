package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino-ext/components/model/agenticdeepseek"
	"github.com/cloudwego/eino-ext/components/model/agenticgemini"
	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"google.golang.org/genai"
)

// Constructor/transport compatibility probes against the versions pinned in go.mod.
// These do not certify the product factory, response admission or any endpoint.
type p2RoundTripper func(*http.Request) (*http.Response, error)

func (f p2RoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestP2FrameworkClaudeHiddenRetries(t *testing.T) {
	for _, networkFailure := range []bool{false, true} {
		name := "server_error"
		if networkFailure {
			name = "no_response"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if networkFailure {
					return nil, errors.New("synthetic transport failure")
				}
				return &http.Response{StatusCode: 500, Header: http.Header{
					"Content-Type": {"application/json"}, "Retry-After-Ms": {"1"},
				}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"api_error","message":"synthetic failure"}}`)), Request: r}, nil
			})}
			m, err := agenticclaude.New(t.Context(), &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "probe", MaxTokens: 16, HTTPClient: client})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.Generate(t.Context(), []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}); err == nil {
				t.Fatal("failed transport accepted")
			}
			// v0.1.7 exposes no MaxRetries option; the SDK defaults to two
			// retries even without a response. Product budgeting must count
			// RoundTrip calls, not just model callbacks.
			if got := calls.Load(); got != 3 {
				t.Fatalf("physical calls=%d, want 3", got)
			}
		})
	}
}

func TestP2FrameworkProtocolClients(t *testing.T) {
	factories := []struct {
		name string
		make func(context.Context, *http.Client) (model.AgenticModel, error)
	}{
		{"openai-chat", func(ctx context.Context, c *http.Client) (model.AgenticModel, error) {
			return agenticopenai.NewChatModel(ctx, &agenticopenai.ChatConfig{APIKey: "synthetic-test-key", Model: "probe", HTTPClient: c})
		}},
		{"openai-responses", func(ctx context.Context, c *http.Client) (model.AgenticModel, error) {
			zero, no := 0, false
			return agenticopenai.NewResponsesModel(ctx, &agenticopenai.ResponsesConfig{APIKey: "synthetic-test-key", Model: "probe", HTTPClient: c, MaxRetries: &zero, Store: &no, EnableAutoCache: false})
		}},
		{"anthropic-messages", func(ctx context.Context, c *http.Client) (model.AgenticModel, error) {
			return agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "probe", MaxTokens: 16, HTTPClient: c})
		}},
		{"gemini-generate-content", func(ctx context.Context, c *http.Client) (model.AgenticModel, error) {
			client, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: "synthetic-test-key", Backend: genai.BackendGeminiAPI, HTTPClient: c})
			if err != nil {
				return nil, err
			}
			return agenticgemini.New(ctx, &agenticgemini.Config{Client: client, Model: "probe"})
		}},
		{"deepseek-chat", func(ctx context.Context, c *http.Client) (model.AgenticModel, error) {
			return agenticdeepseek.New(ctx, &agenticdeepseek.Config{APIKey: "synthetic-test-key", Model: "probe", HTTPClient: c})
		}},
	}
	for _, f := range factories {
		t.Run(f.name, func(t *testing.T) {
			var calls atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				if f.name == "openai-responses" {
					if string(body["store"]) != "false" {
						t.Error("Responses did not explicitly disable storage")
					}
					if _, ok := body["previous_response_id"]; ok {
						t.Error("Responses unexpectedly used stateful continuation")
					}
				}
				// A terminal error avoids SDK retry delays and must traverse exactly
				// this transport. There is no network fallback in this fake client.
				return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"synthetic terminal error","code":400,"status":"INVALID_ARGUMENT"}}`)), Request: r}, nil
			})}
			m, err := f.make(t.Context(), client)
			if err != nil {
				t.Fatal(err)
			}
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			if _, err := m.Generate(t.Context(), input); err == nil {
				t.Fatal("terminal response accepted")
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("Generate transport calls=%d", n)
			}
			reader, err := m.Stream(t.Context(), input)
			if err == nil {
				defer reader.Close()
				_, err = reader.Recv()
			}
			if err == nil {
				t.Fatal("terminal stream response accepted")
			}
			if n := calls.Load(); n != 2 {
				t.Fatalf("Generate+Stream transport calls=%d", n)
			}
		})
	}
}
