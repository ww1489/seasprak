package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticclaude"
	"github.com/ww1489/seasprak/internal/llm/einoext/agenticgemini"
	"google.golang.org/genai"
)

// Gemini and Claude probes certify the repository's private patched adapters,
// not the original unpatched upstream tags. Responses uses the product factory
// to verify the local lossless replay fix.
// Every opaque value is a synthetic fixture; failures never print message bodies.
type p2ProbeReply struct {
	stream bool
	body   string
}

func p2ProbeServer(t *testing.T, replies ...p2ProbeReply) (string, *http.Client, <-chan map[string]any) {
	t.Helper()
	var calls atomic.Int32
	requests := make(chan map[string]any, len(replies))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1)) - 1
		if n >= len(replies) {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("request JSON could not be decoded")
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		requests <- request
		contentType := "application/json"
		if replies[n].stream {
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, replies[n].body)
	}))
	t.Cleanup(func() {
		server.Close()
		if got := calls.Load(); got != int32(len(replies)) {
			t.Errorf("HTTP invocation count=%d, want %d", got, len(replies))
		}
	})
	client := server.Client()
	client.Timeout = 5 * time.Second
	// Refuse redirects and non-fixture destinations, including any SDK fallback.
	transport := client.Transport
	client.Transport = p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme+"://"+r.URL.Host != server.URL {
			return nil, fmt.Errorf("non-fixture destination refused")
		}
		return transport.RoundTrip(r)
	})
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return server.URL, client, requests
}

func p2ProbeRequest(t *testing.T, requests <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case request := <-requests:
		return request
	default:
		t.Fatal("expected captured HTTP request is missing")
		return nil
	}
}

func p2ProbeInvoke(t *testing.T, m model.AgenticModel, stream bool, input []*schema.AgenticMessage) *schema.AgenticMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !stream {
		msg, err := m.Generate(ctx, input)
		if err != nil {
			t.Fatal("adapter Generate failed before protocol assertions")
		}
		return msg
	}
	r, err := m.Stream(ctx, input)
	if err != nil {
		t.Fatal("adapter Stream failed before protocol assertions")
	}
	defer r.Close()
	var chunks []*schema.AgenticMessage
	for {
		chunk, err := r.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("adapter stream receive failed before protocol assertions")
		}
		chunks = append(chunks, chunk)
	}
	msg, err := schema.ConcatAgenticMessages(chunks)
	if err != nil {
		t.Fatal("adapter chunks could not be concatenated")
	}
	return msg
}

func p2ProbeGemini(t *testing.T, url string, client *http.Client) model.AgenticModel {
	t.Helper()
	gc, err := genai.NewClient(t.Context(), &genai.ClientConfig{
		APIKey: "synthetic-probe-key", Backend: genai.BackendGeminiAPI, HTTPClient: client,
		HTTPOptions: genai.HTTPOptions{BaseURL: url, APIVersion: "v1beta"},
	})
	if err != nil {
		t.Fatal("Gemini fixture client construction failed")
	}
	m, err := agenticgemini.New(t.Context(), &agenticgemini.Config{Client: gc, Model: "probe"})
	if err != nil {
		t.Fatal("Gemini adapter construction failed")
	}
	return m
}

func TestP2FixedAdapterGeminiResponseCallID(t *testing.T) {
	body := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"provider-call-a","name":"lookup","args":{"q":"a"}}}]},"finishReason":"STOP"}]}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			reply := body
			if stream {
				reply = "data: " + body + "\n\n"
			}
			url, client, _ := p2ProbeServer(t, p2ProbeReply{stream, reply})
			msg := p2ProbeInvoke(t, p2ProbeGemini(t, url, client), stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].FunctionToolCall == nil {
				t.Fatal("expected exactly one function call")
			}
			call := msg.ContentBlocks[0].FunctionToolCall
			if call.Name != "lookup" || call.Arguments != `{"q":"a"}` {
				t.Error("function name or arguments changed")
			}
			if call.CallID != "provider-call-a" {
				t.Error("provider FunctionCall.ID was not preserved in CallID")
			}
		})
	}
}

func TestP2FixedAdapterGeminiReplayCallAndResponseID(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			body := geminiBody("STOP", "done", false, true)
			if stream {
				body = "data: " + body + "\n\n"
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body})
			// Supply a valid provider identity independently of the response bug.
			input := []*schema.AgenticMessage{
				schema.UserAgenticMessage("probe"),
				{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider-call-a", Name: "lookup", Arguments: `{"q":"a"}`})}},
				{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "provider-call-a", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: `{"ok":true}`}}}})}},
			}
			p2ProbeInvoke(t, p2ProbeGemini(t, url, client), stream, input)
			request := p2ProbeRequest(t, requests)
			contents, _ := request["contents"].([]any)
			counts := map[string]int{}
			for _, raw := range contents {
				content, _ := raw.(map[string]any)
				parts, _ := content["parts"].([]any)
				for _, rawPart := range parts {
					part, _ := rawPart.(map[string]any)
					for _, field := range []string{"functionCall", "functionResponse"} {
						if value, ok := part[field].(map[string]any); ok {
							counts[field]++
							if value["id"] != "provider-call-a" {
								t.Errorf("replay %s.id was not preserved", field)
							}
							if value["name"] != "lookup" {
								t.Errorf("replay %s.name changed", field)
							}
						}
					}
				}
			}
			if counts["functionCall"] != 1 || counts["functionResponse"] != 1 {
				t.Error("replay must contain exactly one call and one tool response")
			}
		})
	}
}

func TestP2FixedAdapterGeminiDistinctCallsAcrossFrames(t *testing.T) {
	body := "data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"provider-call-a","name":"lookup","args":{"q":"a"}}}]}}]}` + "\n\n" +
		"data: " + `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"provider-call-b","name":"lookup","args":{"q":"b"}}}]},"finishReason":"STOP"}]}` + "\n\n"
	url, client, _ := p2ProbeServer(t, p2ProbeReply{true, body})
	msg := p2ProbeInvoke(t, p2ProbeGemini(t, url, client), true, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
	var calls []*schema.FunctionToolCall
	for _, block := range msg.ContentBlocks {
		if block.FunctionToolCall != nil {
			calls = append(calls, block.FunctionToolCall)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("distinct cross-frame function call count=%d, want 2", len(calls))
	}
	for i, id := range []string{"provider-call-a", "provider-call-b"} {
		if calls[i].CallID != id || calls[i].Name != "lookup" || !json.Valid([]byte(calls[i].Arguments)) {
			t.Errorf("cross-frame call %d identity/name/arguments were not preserved", i)
		}
	}
}

func TestP2FixedAdapterResponsesEncryptedContentReplay(t *testing.T) {
	const opaque = "synthetic-opaque-reasoning"
	for _, mode := range []string{"generate_control", "stream_item_added_control", "stream_item_done"} {
		t.Run(mode, func(t *testing.T) {
			item := `{"type":"reasoning","id":"rs_probe","status":"completed","summary":[],"encrypted_content":"` + opaque + `"}`
			complete := p2ResponsesResponse("completed", "", []json.RawMessage{json.RawMessage(item)}, true)
			body := complete
			stream := mode != "generate_control"
			if stream {
				added := `{"type":"reasoning","id":"rs_probe","status":"in_progress","summary":[]}`
				if mode == "stream_item_added_control" {
					added = item
				}
				body = "event: response.output_item.added\ndata: " + `{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":` + added + "}\n\n" +
					"event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":` + item + "}\n\n" +
					"event: response.completed\ndata: " + `{"type":"response.completed","sequence_number":3,"response":` + complete + "}\n\n"
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body}, p2ProbeReply{false, p2ResponsesResponse("completed", "", p2ResponsesTextOutput("done"), true)})
			catalog := llm.NewCatalog(nil)
			cfg := p2ResponsesConfig()
			cfg.Endpoint = url + "/v1"
			p2ResponsesRegister(t, catalog, client, 1<<20)
			p2OK(t, catalog.Register(cfg))
			m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
			p2OK(t, err)
			var observed atomic.Int32
			ctx := p2ChatContext(t, &observed)
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2ChatInvoke(ctx, m, stream, input)
			p2OK(t, err)
			if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].Reasoning == nil {
				t.Error("expected exactly one reasoning block")
			} else if msg.ContentBlocks[0].Reasoning.Signature != opaque {
				t.Error("response reasoning encrypted_content was not preserved")
			}
			_, err = p2ChatInvoke(ctx, m, false, append(input, msg, schema.UserAgenticMessage("continue")))
			p2OK(t, err)
			if observed.Load() != 2 {
				t.Error("Responses observed HTTP count must equal two")
			}
			first := p2ProbeRequest(t, requests)
			include, _ := first["include"].([]any)
			if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
				t.Error("encrypted reasoning was not explicitly requested")
			}
			request := p2ProbeRequest(t, requests)
			if request["store"] != false || request["previous_response_id"] != nil {
				t.Error("replay must be stateless with store=false")
			}
			items, _ := request["input"].([]any)
			count := 0
			for _, raw := range items {
				value, _ := raw.(map[string]any)
				if value["type"] == "reasoning" {
					count++
					if value["id"] != "rs_probe" {
						t.Error("replay reasoning identity was lost")
					}
					if value["encrypted_content"] != opaque {
						t.Error("replay reasoning encrypted_content was lost")
					}
				}
			}
			if count != 1 {
				t.Errorf("replayed reasoning count=%d, want 1", count)
			}
		})
	}
}

func TestP2FixedAdapterClaudeRedactedThinkingReplay(t *testing.T) {
	const opaque = "synthetic-redacted-data"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			body := `{"id":"msg_probe","type":"message","role":"assistant","model":"probe","content":[{"type":"thinking","thinking":"summary","signature":"synthetic-signature"},{"type":"redacted_thinking","data":"` + opaque + `"},{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":3}}`
			if stream {
				body = "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_probe","type":"message","role":"assistant","model":"probe","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n"
				for i, block := range []string{`{"type":"thinking","thinking":"summary","signature":"synthetic-signature"}`, `{"type":"redacted_thinking","data":"` + opaque + `"}`, `{"type":"text","text":"done"}`} {
					body += fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, block, i)
				}
				body += "event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			}
			url, client, requests := p2ProbeServer(t, p2ProbeReply{stream, body}, p2ProbeReply{false, anthropicBody("end_turn", "done", false, true)})
			m, err := agenticclaude.New(t.Context(), &agenticclaude.Config{APIKey: "synthetic-probe-key", BaseURL: url, Model: "probe", MaxTokens: 2048, HTTPClient: client})
			if err != nil {
				t.Fatal("Claude adapter construction failed")
			}
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg := p2ProbeInvoke(t, m, stream, input)
			if len(msg.ContentBlocks) != 3 {
				t.Errorf("response content block count=%d, want 3 including redacted thinking", len(msg.ContentBlocks))
			}
			p2ProbeInvoke(t, m, false, append(input, msg, schema.UserAgenticMessage("continue")))
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			messages, _ := request["messages"].([]any)
			var blocks []any
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if message["role"] == "assistant" {
					blocks, _ = message["content"].([]any)
				}
			}
			if len(blocks) != 3 {
				t.Errorf("replayed assistant block count=%d, want 3", len(blocks))
			}
			thinking, redacted := 0, 0
			for i, raw := range blocks {
				block, _ := raw.(map[string]any)
				switch block["type"] {
				case "thinking":
					thinking++
					if i != 0 || block["thinking"] != "summary" || block["signature"] != "synthetic-signature" {
						t.Error("ordinary thinking control did not round-trip")
					}
				case "redacted_thinking":
					redacted++
					if i != 1 || block["data"] != opaque {
						t.Error("redacted thinking order or opaque data changed")
					}
				}
			}
			if thinking != 1 || redacted != 1 {
				t.Errorf("replay thinking count=%d redacted_thinking count=%d, want 1 each", thinking, redacted)
			}
		})
	}
}
