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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"google.golang.org/genai"
)

type offlineRoundTripper func(*http.Request) (*http.Response, error)

func (f offlineRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOfflineFunctionIdentityConversion(t *testing.T) {
	for _, id := range []string{"provider-call", ""} {
		block, err := convAgenticFC(&genai.FunctionCall{ID: id, Name: "lookup", Args: map[string]any{"q": "a"}})
		if err != nil {
			t.Fatal(err)
		}
		if block.FunctionToolCall.CallID == "" || id != "" && block.FunctionToolCall.CallID != id {
			t.Error("response call identity changed")
		}
		part, err := convFunctionToolCall(&schema.FunctionToolCall{CallID: id, Name: "lookup", Arguments: `{"q":"a"}`})
		if err != nil {
			t.Fatal(err)
		}
		if part.FunctionCall.ID != id {
			t.Error("request call identity changed")
		}
		part, err = convFunctionToolResult(&schema.FunctionToolResult{CallID: id, Name: "lookup"})
		if err != nil {
			t.Fatal(err)
		}
		if part.FunctionResponse.ID != id {
			t.Error("request result identity changed")
		}
	}
}

func TestOfflineFunctionFramesAndReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, id := range []string{"provider-call", ""} {
			t.Run(fmt.Sprintf("stream_%t/id_%s", stream, id), func(t *testing.T) {
				var count atomic.Int32
				requests := make(chan map[string]any, 2)
				frame := func(suffix string) string {
					return fmt.Sprintf(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":%q,"name":"lookup","args":{"q":%q}},"thoughtSignature":"c3ludGhldGlj"}]},"finishReason":"STOP"}]}`, func() string {
						if id == "" {
							return ""
						}
						return id + suffix
					}(), suffix)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := count.Add(1)
					if n > 2 {
						http.Error(w, "unexpected fixture request", http.StatusBadRequest)
						return
					}
					var req map[string]any
					if json.NewDecoder(r.Body).Decode(&req) != nil {
						t.Error("invalid request")
						w.WriteHeader(400)
						return
					}
					requests <- req
					if n == 1 && stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\ndata: %s\n\n", frame("a"), frame("b"))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if n == 1 {
						io.WriteString(w, frame("a"))
					} else {
						io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`)
					}
				}))
				defer server.Close()
				httpClient := server.Client()
				httpClient.Timeout = 5 * time.Second
				transport := httpClient.Transport
				httpClient.Transport = offlineRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme+"://"+r.URL.Host != server.URL {
						return nil, fmt.Errorf("non-fixture destination refused")
					}
					return transport.RoundTrip(r)
				})
				httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				client, err := genai.NewClient(context.Background(), &genai.ClientConfig{APIKey: "synthetic", Backend: genai.BackendGeminiAPI, HTTPClient: httpClient, HTTPOptions: genai.HTTPOptions{BaseURL: server.URL, APIVersion: "v1beta"}})
				if err != nil {
					t.Fatal(err)
				}
				m, err := New(context.Background(), &Config{Client: client, Model: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				input := []*schema.AgenticMessage{schema.UserAgenticMessage("probe")}
				var msg *schema.AgenticMessage
				if stream {
					reader, e := m.Stream(context.Background(), input)
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
					msg, err = m.Generate(context.Background(), input)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := 1
				if stream {
					want = 2
				}
				if len(msg.ContentBlocks) != want {
					t.Fatalf("call count=%d want %d", len(msg.ContentBlocks), want)
				}
				for i, b := range msg.ContentBlocks {
					wantID := ""
					if id != "" {
						wantID = id + string(rune('a'+i))
					}
					if b.FunctionToolCall == nil || b.FunctionToolCall.CallID == "" || id != "" && b.FunctionToolCall.CallID != wantID {
						t.Error("provider identity lost")
					} else if !json.Valid([]byte(b.FunctionToolCall.Arguments)) {
						t.Error("arguments merged")
					}
					if string(getThoughtSignature(b)) != "synthetic" {
						t.Error("thought signature lost")
					}
				}
				// JSON persistence must also preserve metadata before the next turn.
				encoded, e := json.Marshal(msg)
				if e != nil {
					t.Fatal(e)
				}
				var restored schema.AgenticMessage
				if e = json.Unmarshal(encoded, &restored); e != nil {
					t.Fatal(e)
				}
				input = append(input, &restored)
				for _, b := range restored.ContentBlocks {
					input = append(input, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolResult{CallID: b.FunctionToolCall.CallID, Name: "lookup"})}})
				}
				if _, err = m.Generate(context.Background(), input); err != nil {
					t.Fatal(err)
				}
				<-requests
				req := <-requests
				contents := req["contents"].([]any)
				calls, results := 0, 0
				for _, c := range contents {
					for _, p := range c.(map[string]any)["parts"].([]any) {
						part := p.(map[string]any)
						for _, key := range []string{"functionCall", "functionResponse"} {
							if raw, ok := part[key]; ok {
								v := raw.(map[string]any)
								index := calls
								if key == "functionCall" {
									calls++
								} else {
									index = results
									results++
								}
								wantID := msg.ContentBlocks[index].FunctionToolCall.CallID
								got, _ := v["id"].(string)
								if got != wantID {
									t.Errorf("%s identity changed", key)
								}
								if key == "functionCall" && part["thoughtSignature"] != "c3ludGhldGlj" {
									t.Error("replay thought signature lost")
								}
							}
						}
					}
				}
				if calls != want || results != want || count.Load() != 2 {
					t.Error("wrong invocation/block counts")
				}
			})
		}
	}
}

func TestOfflineSignatureJSONRoundTrip(t *testing.T) {
	block := schema.NewContentBlock(&schema.FunctionToolCall{Name: "lookup"})
	setThoughtSignature(block, []byte("synthetic"))
	data, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var restored schema.ContentBlock
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if string(getThoughtSignature(&restored)) != "synthetic" {
		t.Error("JSON persistence lost thought signature")
	}
}

func TestOfflineSignatureOnlyFrameKeepsCallIndex(t *testing.T) {
	first, _ := convAgenticFC(&genai.FunctionCall{ID: "id", Name: "lookup", Args: map[string]any{}})
	index, kind := populateStreamingMeta([]*schema.ContentBlock{first}, 0, "")
	signature := createContentBlockFromType(kind)
	setThoughtSignature(signature, []byte("synthetic"))
	next, _ := populateStreamingMeta([]*schema.ContentBlock{signature}, index, kind)
	if next != index {
		t.Error("signature-only chunk detached from call")
	}
}
