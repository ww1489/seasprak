package llm_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/llm"
)

// Capture serialized HTTP bodies without opening sockets or decoding numbers as float64.
func TestGeminiNumericReplayHTTP(t *testing.T) {
	const signature = "c3ludGhldGljLXNpZ25hdHVyZQ=="
	const numbers = `{"integer":9007199254740993,"nested":{"negative":-9007199254740993},"array":[18446744073709551615,0.1234567890123456789]}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			var calls atomic.Int32
			requests := make(chan map[string]any, 2)
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				if n > 2 {
					return nil, fmt.Errorf("unexpected fixture request")
				}
				var request map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&request); err != nil {
					return nil, err
				}
				requests <- request
				body := `{"candidates":[{"content":{"role":"model","parts":[{"thoughtSignature":"` + signature + `","functionCall":{"id":"numeric-call","name":"lookup","args":{}}}]},"finishReason":"STOP"}]}`
				if n == 2 {
					body = geminiBody("STOP", "done", false, true)
				}
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					body = "data: " + body + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://gemini-numeric.invalid"
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, input)
			p2OK(t, err)
			if len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].FunctionToolCall == nil {
				t.Fatal("expected one accepted call")
			}
			// Isolate outbound replay from provider response decoding: the persisted
			// call contains exact JSON, as can also arrive from gateway fragments.
			msg.ContentBlocks[0].FunctionToolCall.Arguments = numbers
			msg = p2FactoryReplayJSON(t, msg)
			result := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: "numeric-call", Name: "lookup", Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: numbers}}}})}}
			_, err = p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, append(input, msg, p2FactoryReplayJSON(t, result)))
			p2OK(t, err)
			if calls.Load() != 2 || observed.Load() != 2 {
				t.Fatal("replay did not issue exactly two physical requests")
			}
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			contents := request["contents"].([]any)
			callPart := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
			call := callPart["functionCall"].(map[string]any)
			response := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
			if call["id"] != "numeric-call" || response["id"] != "numeric-call" || callPart["thoughtSignature"] != signature || call["name"] != "lookup" || response["name"] != "lookup" {
				t.Fatal("call/result identity or thought signature changed")
			}
			for name, value := range map[string]any{"arguments": call["args"], "result": response["response"]} {
				object := value.(map[string]any)
				if object["integer"] != json.Number("9007199254740993") || object["nested"].(map[string]any)["negative"] != json.Number("-9007199254740993") || object["array"].([]any)[0] != json.Number("18446744073709551615") || object["array"].([]any)[1] != json.Number("0.1234567890123456789") {
					t.Errorf("%s numeric precision lost in serialized next-round HTTP request: synthetic values=%v", name, object)
				}
			}
		})
	}
}
