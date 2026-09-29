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

package agenticclaude

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

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/cloudwego/eino/schema"
)

type offlineRoundTripper func(*http.Request) (*http.Response, error)

func (f offlineRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOfflineRedactedConversion(t *testing.T) {
	for _, data := range []string{"synthetic-opaque", ""} {
		block, err := toAgenticContentBlock(anthropic.RedactedThinkingBlock{Data: data})
		if err != nil {
			t.Fatal(err)
		}
		if block == nil {
			t.Fatal("redacted thinking was dropped")
		}
		msg, err := toAssistantMessageParam(&schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}})
		if err != nil {
			t.Fatal(err)
		}
		if len(msg.Content) != 1 || msg.Content[0].OfRedactedThinking == nil || msg.Content[0].OfRedactedThinking.Data != data {
			t.Fatal("opaque data did not round-trip")
		}
	}
}

func TestOfflineRedactedRejectsInvalidMetadata(t *testing.T) {
	for _, block := range []*schema.ContentBlock{
		{Type: schema.ContentBlockTypeReasoning, Reasoning: &schema.Reasoning{}, Extra: map[string]any{redactedThinkingExtraKey: 42}},
		{Type: schema.ContentBlockTypeReasoning, Extra: map[string]any{redactedThinkingExtraKey: "synthetic"}},
		{Type: schema.ContentBlockTypeReasoning, Reasoning: &schema.Reasoning{Text: "visible"}, Extra: map[string]any{redactedThinkingExtraKey: "synthetic"}},
		{Type: schema.ContentBlockTypeReasoning, Reasoning: &schema.Reasoning{Signature: "synthetic"}, Extra: map[string]any{redactedThinkingExtraKey: "synthetic"}},
	} {
		_, err := toAssistantMessageParam(&schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}})
		if err == nil || err.Error() != "invalid redacted thinking block" {
			t.Fatal("invalid redacted metadata must fail without exposing data")
		}
	}
}

func TestOfflineRedactedHTTPReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			blocks := []string{`{"type":"thinking","thinking":"summary","signature":"synthetic-signature"}`, `{"type":"redacted_thinking","data":"synthetic-opaque-a"}`, `{"type":"redacted_thinking","data":"synthetic-opaque-b"}`, `{"type":"text","text":"done"}`}
			body := `{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture","content":[` + blocks[0] + "," + blocks[1] + "," + blocks[2] + "," + blocks[3] + `],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":3}}`
			var count atomic.Int32
			requests := make(chan map[string]any, 2)
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
					io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"fixture","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n")
					for i, b := range blocks {
						fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":%s}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i, b, i)
					}
					io.WriteString(w, "event: message_delta\ndata: "+`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`+"\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, body)
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
			m, err := New(context.Background(), &Config{APIKey: "synthetic", BaseURL: server.URL, Model: "fixture", MaxTokens: 1024, HTTPClient: httpClient})
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
			if len(msg.ContentBlocks) != 4 {
				t.Fatalf("received %d blocks want 4", len(msg.ContentBlocks))
			}
			for _, b := range msg.ContentBlocks[1:3] {
				if b.Reasoning == nil || b.Reasoning.Text != "" || b.Reasoning.Signature != "" {
					t.Error("opaque data must not become display text or thinking signature")
				}
			}
			encoded, e := json.Marshal(msg)
			if e != nil {
				t.Fatal(e)
			}
			var restored schema.AgenticMessage
			if e = json.Unmarshal(encoded, &restored); e != nil {
				t.Fatal(e)
			}
			if _, err = m.Generate(context.Background(), append(input, &restored, schema.UserAgenticMessage("continue"))); err != nil {
				t.Fatal(err)
			}
			<-requests
			request := <-requests
			messages := request["messages"].([]any)
			assistant := messages[1].(map[string]any)
			content := assistant["content"].([]any)
			if len(content) != 4 {
				t.Fatalf("replayed %d blocks want 4", len(content))
			}
			for i, raw := range blocks {
				var want map[string]any
				json.Unmarshal([]byte(raw), &want)
				got := content[i].(map[string]any)
				for k, v := range want {
					if got[k] != v {
						t.Errorf("block %d field %s changed", i, k)
					}
				}
			}
			if count.Load() != 2 {
				t.Error("expected exactly two HTTP invocations")
			}
		})
	}
}
