/*
 * Copyright 2026 CloudWeGo Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package agenticgemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"google.golang.org/genai"
)

func TestOfflineSignatureOnlyPartsHTTPReplay(t *testing.T) {
	for _, withID := range []bool{true, false} {
		id := func(suffix string) string {
			if withID {
				return "provider-" + suffix
			}
			return ""
		}
		call := func(suffix string) string {
			if !withID {
				return fmt.Sprintf(`{"functionCall":{"name":"lookup","args":{"q":%q}}}`, suffix)
			}
			return fmt.Sprintf(`{"functionCall":{"id":%q,"name":"lookup","args":{"q":%q}}}`, id(suffix), suffix)
		}
		signature := func(suffix string) string {
			return fmt.Sprintf(`{"thoughtSignature":%q}`, base64.StdEncoding.EncodeToString([]byte("synthetic-"+suffix)))
		}
		cases := []struct {
			name   string
			frames [][]string
			calls  []string
			text   bool
		}{
			{"first_frame", [][]string{{call("a"), signature("a")}}, []string{"a"}, false},
			{"after_text_frame", [][]string{{`{"text":"preface"}`}, {call("a"), signature("a")}}, []string{"a"}, true},
			{"after_call_frame", [][]string{{call("a")}, {signature("a")}, {call("b"), signature("b")}}, []string{"a", "b"}, false},
			{"separate_signature_frame", [][]string{{call("a")}, {signature("a")}, {call("b")}, {signature("b")}}, []string{"a", "b"}, false},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/with_id_%t", tc.name, withID), func(t *testing.T) {
				var count atomic.Int32
				requests := make(chan map[string]any, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := count.Add(1)
					if n > 2 {
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					var request map[string]any
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("invalid fixture request")
						http.Error(w, "invalid request", http.StatusBadRequest)
						return
					}
					requests <- request
					if n == 1 {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, parts := range tc.frames {
							fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[%s]}}]}\n\n", strings.Join(parts, ","))
						}
						io.WriteString(w, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
					}
				}))
				defer server.Close()
				client := server.Client()
				client.Timeout = 5 * time.Second
				transport := client.Transport
				client.Transport = offlineRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme+"://"+r.URL.Host != server.URL {
						return nil, fmt.Errorf("non-fixture destination refused")
					}
					return transport.RoundTrip(r)
				})
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				gc, err := genai.NewClient(ctx, &genai.ClientConfig{APIKey: "synthetic", Backend: genai.BackendGeminiAPI, HTTPClient: client, HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta"}})
				if err != nil {
					t.Fatal(err)
				}
				m, err := New(ctx, &Config{Client: gc, Model: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
				reader, err := m.Stream(ctx, input)
				if err != nil {
					t.Fatal(err)
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
				msg, err := schema.ConcatAgenticMessages(chunks)
				if err != nil {
					t.Fatal(err)
				}
				assignedIDs := make(map[string]string)
				for _, b := range msg.ContentBlocks {
					if fc := b.FunctionToolCall; fc != nil {
						var args map[string]string
						if json.Unmarshal([]byte(fc.Arguments), &args) != nil || fc.CallID == "" {
							t.Fatal("invalid call arguments or missing assigned identity")
						}
						if withID && fc.CallID != id(args["q"]) {
							t.Fatal("provider identity changed")
						}
						assignedIDs[args["q"]] = fc.CallID
					}
				}
				id := func(suffix string) string { return assignedIDs[suffix] }
				check := func(stage string, msg *schema.AgenticMessage) {
					t.Helper()
					wantBlocks := len(tc.calls)
					if tc.text {
						wantBlocks++
					}
					if len(msg.ContentBlocks) != wantBlocks {
						t.Errorf("%s block count=%d want %d", stage, len(msg.ContentBlocks), wantBlocks)
					}
					calls, texts := 0, 0
					for _, b := range msg.ContentBlocks {
						if b.FunctionToolCall != nil {
							if calls >= len(tc.calls) {
								t.Errorf("%s unexpected extra function call", stage)
								continue
							}
							suffix := tc.calls[calls]
							calls++
							if b.FunctionToolCall.Name != "lookup" || b.FunctionToolCall.CallID != id(suffix) || b.FunctionToolCall.Arguments != fmt.Sprintf(`{"q":%q}`, suffix) {
								t.Errorf("%s call identity/name/arguments changed", stage)
							}
							if string(getThoughtSignature(b)) != "synthetic-"+suffix {
								t.Errorf("%s call %d signature changed", stage, calls-1)
							}
						} else if b.AssistantGenText != nil {
							texts++
							if !tc.text || b.AssistantGenText.Text != "preface" || len(getThoughtSignature(b)) != 0 {
								t.Errorf("%s signature attached to unrelated text", stage)
							}
						} else {
							t.Errorf("%s unexpected block type", stage)
						}
					}
					if calls != len(tc.calls) {
						t.Errorf("%s effective call count=%d want %d", stage, calls, len(tc.calls))
					}
					wantTexts := 0
					if tc.text {
						wantTexts = 1
					}
					if texts != wantTexts {
						t.Errorf("%s text count=%d want %d", stage, texts, wantTexts)
					}
				}
				check("concat", msg)
				data, err := json.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				var restored schema.AgenticMessage
				if err = json.Unmarshal(data, &restored); err != nil {
					t.Fatal(err)
				}
				check("json", &restored)
				input = append(input, &restored)
				for _, suffix := range tc.calls {
					input = append(input, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: id(suffix), Name: "lookup"})}})
				}
				if _, err = m.Generate(ctx, input); err != nil {
					t.Fatalf("next-turn replay failed: %v", err)
				}
				if count.Load() != 2 {
					t.Fatalf("HTTP invocation count=%d want 2", count.Load())
				}
				<-requests
				request := <-requests
				contents, _ := request["contents"].([]any)
				calls, results := 0, 0
				for _, raw := range contents {
					content, _ := raw.(map[string]any)
					parts, _ := content["parts"].([]any)
					for _, rawPart := range parts {
						part, _ := rawPart.(map[string]any)
						if fc, ok := part["functionCall"].(map[string]any); ok {
							if calls >= len(tc.calls) {
								t.Error("replay extra function call")
								continue
							}
							suffix := tc.calls[calls]
							calls++
							gotID, _ := fc["id"].(string)
							args, _ := fc["args"].(map[string]any)
							if gotID != id(suffix) || fc["name"] != "lookup" || len(args) != 1 || args["q"] != suffix {
								t.Error("replay identity/name/arguments changed")
							}
							if part["thoughtSignature"] != base64.StdEncoding.EncodeToString([]byte("synthetic-"+suffix)) {
								t.Error("replay call signature lost")
							}
						} else if _, ok := part["thoughtSignature"]; ok {
							t.Error("replay signature detached from call")
						}
						if fr, ok := part["functionResponse"].(map[string]any); ok {
							if results >= len(tc.calls) {
								t.Error("replay extra function response")
								continue
							}
							suffix := tc.calls[results]
							results++
							gotID, _ := fr["id"].(string)
							if gotID != id(suffix) || fr["name"] != "lookup" {
								t.Error("replay result identity changed")
							}
						}
					}
				}
				if calls != len(tc.calls) || results != len(tc.calls) {
					t.Error("replay effective call/result count changed")
				}
			})
		}
	}
}
