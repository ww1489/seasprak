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

type numericChunkReader struct {
	reader io.Reader
	chunk  int
}

func (r *numericChunkReader) Read(p []byte) (int, error) {
	if r.chunk > 0 && len(p) > r.chunk {
		p = p[:r.chunk]
	}
	return r.reader.Read(p)
}

// The raw provider response, not an edited message, is the source of arguments.
func TestGeminiNumericResponseHTTP(t *testing.T) {
	for _, tc := range []struct {
		stream bool
		chunk  int
	}{{false, 0}, {true, 0}, {false, 1}, {true, 1}, {true, 17}} {
		t.Run(fmt.Sprintf("stream_%t/chunk_%d", tc.stream, tc.chunk), func(t *testing.T) {
			t.Parallel()
			stream := tc.stream
			var calls atomic.Int32
			client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) != 1 {
					return nil, fmt.Errorf("unexpected fixture request")
				}
				body := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"numeric-response-call","name":"lookup","args":{"integer":9007199254740993}}}]},"finishReason":"STOP"}]}`
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					body = "data: " + body + "\n\n"
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(&numericChunkReader{reader: strings.NewReader(body), chunk: tc.chunk}), Request: r}, nil
			})}
			cfg := geminiConfig()
			cfg.Endpoint = "https://gemini-numeric-response.invalid"
			m := p2FactoryReplayBind(t, cfg, func(c *llm.Catalog) error { return c.RegisterGeminiGenerateContent(client, 1<<20) })
			var observed atomic.Int32
			msg, err := p2FactoryReplayInvoke(p2ChatContext(t, &observed), m, stream, []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
			p2OK(t, err)
			if calls.Load() != 1 || observed.Load() != 1 {
				t.Fatal("response acceptance did not use exactly one observed physical request")
			}
			if msg == nil || len(msg.ContentBlocks) != 1 || msg.ContentBlocks[0].FunctionToolCall == nil || msg.Extra["seasprak.finish"] != "tool_calls" {
				t.Fatal("expected one accepted tool call")
			}
			call := msg.ContentBlocks[0].FunctionToolCall
			if call.CallID != "numeric-response-call" || call.Name != "lookup" {
				t.Fatal("accepted call identity changed")
			}
			var arguments map[string]json.RawMessage
			p2OK(t, json.Unmarshal([]byte(call.Arguments), &arguments))
			if len(arguments) != 1 || string(arguments["integer"]) != "9007199254740993" {
				t.Errorf("accepted provider arguments lost numeric precision: synthetic integer=%s, want 9007199254740993", arguments["integer"])
			}
		})
	}
}
