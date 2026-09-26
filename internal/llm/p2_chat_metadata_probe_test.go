package llm_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/schema"
)

// Framework evidence only. Product finish normalization belongs to Step 7.
func TestP2FrameworkChatFinishMetadata(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, finish := range []string{"stop", "length", ""} {
			t.Run(fmt.Sprintf("stream_%t/finish_%s", stream, finish), func(t *testing.T) {
				var calls atomic.Int32
				client := &http.Client{Transport: p2RoundTripper(func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					var request struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					if request.Stream != stream {
						t.Error("request mode changed")
					}
					body := fmt.Sprintf(`{"id":"probe","choices":[{"index":0,"message":{"role":"assistant","content":"中文"},"finish_reason":%q}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010}}`, finish)
					kind := "application/json"
					if stream {
						kind = "text/event-stream"
						body = "data: {\"id\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"中文\"},\"finish_reason\":null}]}\n\n"
						if finish != "" {
							body += fmt.Sprintf("data: {\"id\":\"probe\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\n", finish)
						}
						body += "data: [DONE]\n\n"
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				m, err := agenticopenai.NewChatModel(t.Context(), &agenticopenai.ChatConfig{APIKey: "synthetic-test-key", Model: "probe", HTTPClient: client})
				if err != nil {
					t.Fatal(err)
				}
				input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
				var msg *schema.AgenticMessage
				if stream {
					reader, e := m.Stream(t.Context(), input)
					if e != nil {
						t.Fatal(e)
					}
					defer reader.Close()
					var chunks []*schema.AgenticMessage
					for {
						chunk, e := reader.Recv()
						if e == io.EOF {
							break
						}
						if e != nil {
							t.Fatal(e)
						}
						chunks = append(chunks, chunk)
					}
					msg, err = schema.ConcatAgenticMessages(chunks)
				} else {
					msg, err = m.Generate(t.Context(), input)
				}
				if err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 1 || msg == nil || msg.Role != schema.AgenticRoleTypeAssistant {
					t.Fatal("invalid request count or response")
				}
				got := ""
				if msg.ResponseMeta != nil && msg.ResponseMeta.Extension != nil {
					ext, ok := msg.ResponseMeta.Extension.(*agenticopenai.ChatResponseMetaExtension)
					if !ok {
						t.Fatalf("unexpected finish metadata type %T", msg.ResponseMeta.Extension)
					}
					got = ext.FinishReason
				}
				if got != finish {
					t.Fatalf("finish=%q want %q", got, finish)
				}
			})
		}
	}
}
