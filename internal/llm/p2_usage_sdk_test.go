package llm_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticclaude"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

func TestP2UsageOtherProtocolsAndUnknownSubsets(t *testing.T) {
	for _, tc := range []struct {
		protocol, body           string
		input, output, reasoning int64
		outputKnown              bool
	}{
		{"openai-chat", `{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":600},"completion_tokens_details":{"reasoning_tokens":20}}}`, 1000, 50, 20, true},
		{"openai-responses", `{"type":"response.completed","response":{"usage":{"input_tokens":1000,"output_tokens":50,"input_tokens_details":{"cached_tokens":600},"output_tokens_details":{"reasoning_tokens":20}}}}`, 1000, 50, 20, true},
		{"gemini-generate-content", `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":30,"thoughtsTokenCount":20,"cachedContentTokenCount":600}}`, 1000, 50, 20, true},
		{"gemini-generate-content", `{"usageMetadata":{"promptTokenCount":1000,"candidatesTokenCount":50,"cachedContentTokenCount":600}}`, 1000, 0, 0, false},
	} {
		c := llm.NewUsageCollector(tc.protocol, 4096)
		r := llm.WrapUsageBody(io.NopCloser(strings.NewReader(tc.body)), "application/json", c)
		_, err := io.ReadAll(r)
		p2OK(t, err)
		r.Close()
		u := c.Snapshot().Usage
		if !u.InputTotal.Known || u.InputTotal.Value != tc.input || u.OutputTotal.Known != tc.outputKnown || u.OutputTotal.Value != tc.output || u.Reasoning.Value != tc.reasoning || u.CacheRead.Value != 600 || u.CacheWrite.Known || u.UncachedInput.Known || u.Estimate.Known {
			t.Fatalf("wrong %s normalization: %+v", tc.protocol, u)
		}
	}
}

func TestP2UsageClaudeSDKSupplementDoesNotDoubleCount(t *testing.T) {
	body := `{"id":"msg-fixture","type":"message","role":"assistant","model":"fixture","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":300,"cache_read_input_tokens":600,"cache_creation_input_tokens":100,"output_tokens":50}}`
	for _, collect := range []bool{false, true} {
		observed := &usageObserver{}
		ctx := llm.WithUsageObservation(t.Context(), observed)
		ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "logical", AttemptID: "attempt", Purpose: "agent"}, p2RequestObserver(func(_ context.Context, _ llm.TransportRequest) error { return nil }))
		var settings []llm.UsageCollection
		if collect {
			settings = append(settings, llm.UsageCollection{Protocol: "anthropic-messages", MaxBytes: 4096})
		}
		client := &http.Client{Transport: llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
		}), settings...)}
		m, err := agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "fixture", MaxTokens: 128, HTTPClient: client})
		if err != nil {
			t.Fatal(err)
		}
		msg, err := m.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
		if err != nil {
			t.Fatal(err)
		}
		if msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil || msg.ResponseMeta.TokenUsage.PromptTokens != 1000 {
			t.Fatal("collector altered SDK input total")
		}
		if collect {
			if len(observed.snapshots) != 1 || observed.snapshots[0].Usage.InputTotal.Value != 1000 || observed.snapshots[0].Usage.CacheWrite.Value != 100 {
				t.Fatal("raw usage supplement missing")
			}
		} else if len(observed.snapshots) != 0 {
			t.Fatal("collection enabled without opt-in")
		}
	}
}

func TestP2UsageClaudeSDKStreamSupplement(t *testing.T) {
	body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg-fixture","type":"message","role":"assistant","model":"fixture","content":[],"stop_reason":null,"usage":{"input_tokens":300,"cache_read_input_tokens":600,"cache_creation_input_tokens":100,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":50}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	observed := &usageObserver{}
	ctx := llm.WithUsageObservation(t.Context(), observed)
	ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "logical", AttemptID: "stream", Purpose: "agent"}, p2RequestObserver(func(context.Context, llm.TransportRequest) error { return nil }))
	client := &http.Client{Transport: llm.NewObservedTransport(p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &usageBody{data: []byte(body), chunk: 7, end: io.EOF}, Request: r}, nil
	}), llm.UsageCollection{Protocol: "anthropic-messages", MaxBytes: 4096})}
	m, err := agenticclaude.New(ctx, &agenticclaude.Config{APIKey: "synthetic-test-key", Model: "fixture", MaxTokens: 128, HTTPClient: client})
	p2OK(t, err)
	r, err := m.Stream(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
	p2OK(t, err)
	var chunks []*schema.AgenticMessage
	for {
		chunk, e := r.Recv()
		if e == io.EOF {
			break
		}
		p2OK(t, e)
		chunks = append(chunks, chunk)
	}
	r.Close()
	msg, err := schema.ConcatAgenticMessages(chunks)
	p2OK(t, err)
	if msg.ResponseMeta == nil || msg.ResponseMeta.TokenUsage == nil || msg.ResponseMeta.TokenUsage.PromptTokens != 1000 {
		t.Fatal("stream collector changed SDK total")
	}
	if len(observed.snapshots) != 1 || observed.snapshots[0].Usage.InputTotal.Value != 1000 || observed.snapshots[0].Usage.CacheWrite.Value != 100 || observed.snapshots[0].Usage.OutputTotal.Value != 50 {
		t.Fatal("stream usage supplement missing or counted deltas twice")
	}
}
