package llm_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func p2ResponsesConfig() llm.ModelConfig {
	cfg := p2Config()
	cfg.Provider = "openai"
	cfg.Protocol = "openai-responses"
	cfg.Model = "responses-fixture"
	cfg.Endpoint = "https://example.invalid/v1"
	return cfg
}

func p2ResponsesRegister(t *testing.T, c *llm.Catalog, client *http.Client, limit int) {
	t.Helper()
	p2OK(t, c.RegisterOpenAIResponses(client, limit))
}

func p2ResponsesResponse(status, reason string, output any, usage bool) string {
	body := map[string]any{
		"id":         "resp_fixture",
		"object":     "response",
		"created_at": 1,
		"status":     status,
		"model":      "responses-fixture",
		"output":     output,
	}
	if reason != "" {
		body["incomplete_details"] = map[string]any{"reason": reason}
	}
	if usage {
		body["usage"] = map[string]any{
			"input_tokens": 10,
			"input_tokens_details": map[string]any{
				"cached_tokens": 2,
			},
			"output_tokens": 3,
			"output_tokens_details": map[string]any{
				"reasoning_tokens": 1,
			},
			"total_tokens": 13,
		}
	}
	encoded, _ := json.Marshal(body)
	return string(encoded)
}

func p2ResponsesTextOutput(text string) []any {
	return []any{map[string]any{
		"id": "msg_fixture", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}}
}

func p2ResponsesToolOutput() []any {
	return []any{map[string]any{
		"id": "fc_fixture", "type": "function_call", "status": "completed", "call_id": "call-a",
		"name": "lookup", "arguments": `{"q":"中文😀"}`,
	}}
}

func p2ResponsesResponseJSON(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func p2ResponsesSSE(status, reason string, output any, usage bool, terminal bool) string {
	response := p2ResponsesResponse(status, reason, output, usage)
	var b strings.Builder
	b.WriteString("data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"item_id\":\"msg_fixture\",\"output_index\":0,\"content_index\":0,\"delta\":\"streamed\",\"logprobs\":[]}\n\n")
	if terminal {
		b.WriteString("data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":")
		b.WriteString(response)
		b.WriteString("}\n\n")
	}
	return b.String()
}

func TestP2OpenAIResponsesGenerateFactoryAndRequest(t *testing.T) {
	var physical, observed atomic.Int32
	var request map[string]any
	c := llm.NewCatalog(nil)
	cfg := p2ResponsesConfig()
	p2ResponsesRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		return p2ResponsesResponseJSON(r, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("hello"), true)), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	observer := &usageObserver{}
	msg, err := m.Generate(llm.WithUsageObservation(p2ChatContext(t, &observed), observer), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2OK(t, err)
	if msg.Extra["seasprak.finish"] != "stop" || textOf(msg) != "hello" {
		t.Fatalf("normalized response = %#v", msg)
	}
	if request["store"] != false {
		t.Fatalf("store = %#v", request["store"])
	}
	if _, ok := request["previous_response_id"]; ok {
		t.Fatal("stateful previous response was sent")
	}
	if _, ok := request["input"]; !ok {
		t.Fatal("full input was not sent")
	}
	if msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil {
		t.Fatal("known response usage was lost")
	}
	if physical.Load() != 1 || observed.Load() != 1 {
		t.Fatalf("physical=%d observed=%d", physical.Load(), observed.Load())
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.snapshots) != 1 || !observer.snapshots[0].Usage.InputTotal.Known || observer.snapshots[0].Usage.InputTotal.Value != 10 || !observer.snapshots[0].Usage.CacheRead.Known || observer.snapshots[0].Usage.CacheRead.Value != 2 {
		t.Fatalf("usage = %+v", observer.snapshots)
	}
}

func TestP2OpenAIResponsesTerminalNormalizationAndUsagePresence(t *testing.T) {
	refusalOutput := []any{map[string]any{
		"id": "msg_refusal", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "refusal", "refusal": "不能协助😀"}},
	}}
	for _, tc := range []struct {
		name, status, reason, want string
		output                     any
		usage                      bool
		wantError                  bool
	}{
		{name: "stop", status: "completed", want: "stop", output: p2ResponsesTextOutput("hello"), usage: true},
		{name: "tools", status: "completed", want: "tool_calls", output: p2ResponsesToolOutput(), usage: true},
		{name: "length", status: "incomplete", reason: "max_output_tokens", want: "length", output: p2ResponsesToolOutput(), usage: false},
		{name: "refusal", status: "completed", want: "refusal", output: refusalOutput, usage: true},
		{name: "missing", output: p2ResponsesTextOutput("partial"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var physical, observed atomic.Int32
			c := llm.NewCatalog(nil)
			cfg := p2ResponsesConfig()
			p2ResponsesRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				return p2ResponsesResponseJSON(r, p2ResponsesResponse(tc.status, tc.reason, tc.output, tc.usage)), nil
			})}, 1<<20)
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			observer := &usageObserver{}
			msg, err := m.Generate(llm.WithUsageObservation(p2ChatContext(t, &observed), observer), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
			if tc.wantError {
				p2Code(t, err, product.CodeInvalidArgument)
			} else {
				p2OK(t, err)
				if msg.Extra["seasprak.finish"] != tc.want {
					t.Fatalf("finish = %#v", msg.Extra["seasprak.finish"])
				}
				if tc.usage != (msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil) {
					t.Fatalf("SDK usage presence = %v", msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil)
				}
			}
			if physical.Load() != 1 || observed.Load() != 1 {
				t.Fatalf("physical=%d observed=%d", physical.Load(), observed.Load())
			}
			observer.mu.Lock()
			defer observer.mu.Unlock()
			if len(observer.snapshots) != 1 {
				t.Fatal("usage observation missing")
			}
			if tc.usage != observer.snapshots[0].Usage.InputTotal.Known {
				t.Fatalf("usage input presence = %+v", observer.snapshots[0].Usage)
			}
		})
	}
}

func TestP2OpenAIResponsesStreamRequiresTerminalAndPreservesUsage(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason, want string
		terminal, usage            bool
		wantError                  bool
	}{
		{name: "complete", status: "completed", want: "stop", terminal: true, usage: true},
		{name: "length", status: "incomplete", reason: "max_output_tokens", want: "length", terminal: true},
		{name: "missing_terminal", status: "completed", terminal: false, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var physical, observed atomic.Int32
			c := llm.NewCatalog(nil)
			cfg := p2ResponsesConfig()
			p2ResponsesRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				physical.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(p2ResponsesSSE(tc.status, tc.reason, p2ResponsesTextOutput("streamed"), tc.usage, tc.terminal))), Request: r}, nil
			})}, 1<<20)
			p2OK(t, c.Register(cfg))
			m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			observer := &usageObserver{}
			msg, err := p2ChatInvoke(llm.WithUsageObservation(p2ChatContext(t, &observed), observer), m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
			if tc.wantError {
				if err == nil {
					t.Fatal("incomplete stream was accepted")
				}
			} else {
				p2OK(t, err)
				if msg.Extra["seasprak.finish"] != tc.want || textOf(msg) != "streamed" {
					t.Fatalf("stream result = %#v", msg)
				}
				if tc.usage != (msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil) {
					t.Fatalf("stream usage presence = %v", msg.ResponseMeta != nil && msg.ResponseMeta.TokenUsage != nil)
				}
			}
			if physical.Load() != 1 || observed.Load() != 1 {
				t.Fatalf("physical=%d observed=%d", physical.Load(), observed.Load())
			}
		})
	}
}

func TestP2OpenAIResponsesRequestToolsAndFullReplay(t *testing.T) {
	var physical, observed atomic.Int32
	var requests []map[string]any
	c := llm.NewCatalog(nil)
	cfg := p2ResponsesConfig()
	p2ResponsesRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		physical.Add(1)
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
		return p2ResponsesResponseJSON(r, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("hello"), true)), nil
	})}, 1<<20)
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	tool := &schema.ToolInfo{Name: "lookup", Desc: "query", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"q": {Type: schema.String, Required: true}})}
	first, err := m.Generate(p2ChatContext(t, &observed), []*schema.AgenticMessage{schema.UserAgenticMessage("first")}, model.WithTools([]*schema.ToolInfo{tool}))
	p2OK(t, err)
	_, err = m.Generate(p2ChatContext(t, &observed), []*schema.AgenticMessage{first, schema.UserAgenticMessage("second")})
	p2OK(t, err)
	if physical.Load() != 2 || observed.Load() != 2 || len(requests) != 2 {
		t.Fatalf("physical=%d observed=%d requests=%d", physical.Load(), observed.Load(), len(requests))
	}
	if len(requests[0]["tools"].([]any)) != 1 {
		t.Fatal("function tool was not sent")
	}
	input, ok := requests[1]["input"].([]any)
	if !ok || len(input) < 2 {
		t.Fatalf("full replay input = %#v", requests[1]["input"])
	}
	encoded, _ := json.Marshal(input)
	if !strings.Contains(string(encoded), "hello") || !strings.Contains(string(encoded), "second") {
		t.Fatalf("replay lost prior message: %s", encoded)
	}
	for i, request := range requests {
		if request["store"] != false {
			t.Fatalf("request %d store = %#v", i, request["store"])
		}
		if _, ok := request["previous_response_id"]; ok {
			t.Fatalf("request %d unexpectedly used previous response", i)
		}
	}
}

func TestP2OpenAIResponsesStreamCloseCancelsHTTPRead(t *testing.T) {
	c := llm.NewCatalog(nil)
	cfg := p2ResponsesConfig()
	body := &p2ChatBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	var observed atomic.Int32
	p2ResponsesRegister(t, c, &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		body.ctx = r.Context()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
	})}, 4096)
	p2OK(t, c.Register(cfg))
	m, err := c.Bind(cfg.Key(), llm.RequestedOptions{})
	p2OK(t, err)
	r, err := m.Stream(p2ChatContext(t, &observed), []*schema.AgenticMessage{schema.UserAgenticMessage("input")})
	p2OK(t, err)
	select {
	case <-body.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Responses adapter did not begin reading")
	}
	received := make(chan error, 1)
	go func() { _, err := r.Recv(); received <- err }()
	r.Close()
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("closing Responses reader did not cancel HTTP read")
	}
	select {
	case err := <-received:
		if err == nil {
			t.Fatal("closed Responses stream reported success")
		}
	case <-time.After(time.Second):
		t.Fatal("closed Responses reader remained blocked")
	}
	if observed.Load() != 1 {
		t.Fatal("request occupancy mismatch")
	}
}
