package llm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

func numericResponseParts(parts string, terminal bool) string {
	finish := ""
	if terminal {
		finish = `,"finishReason":"STOP"`
	}
	return `{"candidates":[{"index":0,"content":{"role":"model","parts":[` + parts + `]}` + finish + `}]}`
}

func numericResponseCall(number string) string {
	return `{"thoughtSignature":"c3ludGhldGlj","functionCall":{"name":"lookup","args":{"integer":` + number + `}}}`
}

func numericResponseAssert(t *testing.T, msg *schema.AgenticMessage, numbers ...string) {
	t.Helper()
	if msg == nil || len(msg.ContentBlocks) != len(numbers) || msg.Extra["seasprak.finish"] != "tool_calls" {
		t.Fatal("unexpected accepted tool calls")
	}
	ids := map[string]bool{}
	for i, block := range msg.ContentBlocks {
		call := block.FunctionToolCall
		if call == nil || call.Name != "lookup" || call.CallID == "" || ids[call.CallID] {
			t.Fatal("call identity lost or conflated")
		}
		ids[call.CallID] = true
		var args map[string]json.RawMessage
		p2OK(t, json.Unmarshal([]byte(call.Arguments), &args))
		if string(args["integer"]) != numbers[i] {
			t.Fatal("accepted number differs from raw response")
		}
	}
}

func TestGeminiNumericPrefetchAndGateway(t *testing.T) {
	for _, kind := range []string{"native_prefetch", "gateway_fragments", "same_frame"} {
		t.Run(kind, func(t *testing.T) {
			const first, second = "9007199254740993", "9007199254740995"
			body := "data: " + numericResponseParts(numericResponseCall(first), false) + "\n\n"
			body += "data: " + numericResponseParts(numericResponseCall(second), true) + "\n\n"
			want := []string{first, second}
			if kind == "same_frame" {
				body = "data: " + numericResponseParts(numericResponseCall(first)+","+numericResponseCall(second), true) + "\n\n"
			}
			if kind == "gateway_fragments" {
				body = "data: " + numericResponseParts(`{"functionCall":{"name":"lookup","args":{}}}`, false) + "\n\n"
				for i, fragment := range []string{`{"integer":9007199`, `254740993}`} {
					part, err := json.Marshal(map[string]any{"functionCall": map[string]any{"args": map[string]any{"arguments": fragment}}})
					p2OK(t, err)
					body += "data: " + numericResponseParts(string(part), i == 1) + "\n\n"
				}
				want = []string{first}
			}
			var calls atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://numeric-prefetch.invalid"
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, true, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			p2OK(t, err)
			numericResponseAssert(t, msg, want...)
			if calls.Load() != 1 || observed.Load() != 1 {
				t.Fatal("unexpected physical request count")
			}
		})
	}
}

func TestGeminiNumericConcurrentCalls(t *testing.T) {
	var calls, observed atomic.Int32
	client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		var request struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if len(request.Contents) != 1 || len(request.Contents[0].Parts) != 1 {
			return nil, fmt.Errorf("unexpected synthetic request")
		}
		body := numericResponseParts(numericResponseCall(request.Contents[0].Parts[0].Text), true)
		contentType := "application/json"
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			contentType = "text/event-stream"
			body = "data: " + body + "\n\n"
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(&numericChunkReader{reader: strings.NewReader(body), chunk: 1}), Request: r}, nil
	})}
	cfg := geminiConfig()
	cfg.Endpoint = "https://numeric-concurrent.invalid"
	m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
	t.Cleanup(func() {
		if calls.Load() != 8 || observed.Load() != 8 {
			t.Error("concurrent calls lost physical accounting")
		}
	})
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			number := fmt.Sprint(int64(9007199254740993) + int64(i))
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, i%2 == 0, []*schema.AgenticMessage{schema.UserAgenticMessage(number)})
			p2OK(t, err)
			numericResponseAssert(t, msg, number)
		})
	}
}

func TestGeminiNumericCaptureOverflowRejectsCalls(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls, observed atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := numericResponseParts(numericResponseCall("9007199254740993"), true)
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					body = "data: " + body + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://numeric-overflow.invalid"
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 64) })
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			p2Code(t, err, product.CodeInvalidArgument)
			if msg != nil || calls.Load() != 1 || observed.Load() != 1 {
				t.Fatal("overflow accepted a call or changed physical accounting")
			}
			if strings.Contains(err.Error(), "9007199254740993") {
				t.Fatal("error leaked raw arguments")
			}
		})
	}
}

func TestGeminiNumericResponseReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls, observed atomic.Int32
			requests := make(chan map[string]any, 2)
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				var request map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&request); err != nil {
					return nil, err
				}
				requests <- request
				body := numericResponseParts(numericResponseCall("9007199254740993"), true)
				if n == 2 {
					body = geminiBody("STOP", "done", false, true)
				}
				if n > 2 {
					return nil, fmt.Errorf("unexpected numeric replay request")
				}
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					body = "data: " + body + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://numeric-response-replay.invalid"
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, input)
			p2OK(t, err)
			numericResponseAssert(t, msg, "9007199254740993")
			msg = p2FactoryReplayJSON(t, msg)
			call := msg.ContentBlocks[0].FunctionToolCall
			result := &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: call.CallID, Name: call.Name, Content: []*schema.FunctionToolResultContentBlock{{Type: schema.FunctionToolResultContentBlockTypeText, Text: &schema.UserInputText{Text: `{"result":9007199254740993}`}}}})}}
			_, err = p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, append(input, msg, result))
			p2OK(t, err)
			if calls.Load() != 2 || observed.Load() != 2 {
				t.Fatal("unexpected replay request count")
			}
			p2ProbeRequest(t, requests)
			request := p2ProbeRequest(t, requests)
			contents := request["contents"].([]any)
			part := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
			wireCall := part["functionCall"].(map[string]any)
			if wireCall["id"] != call.CallID || wireCall["name"] != call.Name || part["thoughtSignature"] != "c3ludGhldGlj" || wireCall["args"].(map[string]any)["integer"] != json.Number("9007199254740993") {
				t.Fatal("raw-response replay lost identity, signature or precision")
			}
		})
	}
}

func TestGeminiNumericCacheFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls, observed atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				n := calls.Add(1)
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					return nil, err
				}
				status, contentType := 200, "application/json"
				var body string
				switch n {
				case 1:
					if !strings.HasSuffix(r.URL.Path, "/cachedContents") {
						return nil, fmt.Errorf("cache creation missing")
					}
					body = `{"name":"cachedContents/numeric","expireTime":"` + time.Now().Add(time.Hour).Format(time.RFC3339Nano) + `"}`
				case 2:
					if request["cachedContent"] != "cachedContents/numeric" {
						return nil, fmt.Errorf("cached generation missing")
					}
					status, body = 404, `{"error":{"code":404,"message":"synthetic missing cache"}}`
				case 3:
					if request["cachedContent"] != nil || request["systemInstruction"] == nil {
						return nil, fmt.Errorf("full fallback missing")
					}
					body = numericResponseParts(numericResponseCall("9007199254740993"), true)
					if stream {
						contentType = "text/event-stream"
						body = "data: " + body + "\n\n"
					}
				default:
					return nil, fmt.Errorf("unexpected fallback request")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://numeric-cache.invalid"
			cfg.Capabilities.Items[llm.CapCacheResource] = llm.Capability{Status: llm.Declared, Evidence: []string{"synthetic numeric cache fixture"}}
			catalog := llm.NewCatalog(p2Resolver(func(context.Context, string) (llm.ResolvedCredential, error) {
				return llm.ResolvedCredential{Secret: "synthetic", AccountScope: cfg.AccountScope, Provider: cfg.Provider, Endpoint: cfg.Endpoint}, nil
			}))
			p2OK(t, catalog.RegisterGeminiGenerateContent(client, 1<<20))
			p2OK(t, catalog.Register(cfg))
			m, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{ExplicitCacheResource: true})
			p2OK(t, err)
			ctx := llm.WithSessionCacheScope(p2ChatContext(t, &observed), "numeric-session")
			msg, err := p2FactoryReplayInvoke(ctx, m, stream, geminiCacheInput())
			p2OK(t, err)
			numericResponseAssert(t, msg, "9007199254740993")
			if calls.Load() != 3 || observed.Load() != 3 {
				t.Fatal("fallback did not account for three physical requests")
			}
		})
	}
}
