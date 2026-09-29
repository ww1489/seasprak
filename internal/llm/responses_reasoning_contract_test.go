package llm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/agenticopenai"
	"github.com/cloudwego/eino/schema"
)

type responsesProbeTransport func(*http.Request) (*http.Response, error)

func (f responsesProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type responsesProbeObserver struct{ count atomic.Int32 }

func (o *responsesProbeObserver) BeforeRequest(context.Context, TransportRequest) error {
	o.count.Add(1)
	return nil
}

type responsesProbeBody struct {
	io.Reader
	io.Closer
}

// This real-HTTP probe exercises the public transport/model extension before
// the product factory installs it. No fake Reasoning or rewritten response is
// supplied to Eino; both the emitted block metadata and replay come from v0.2.4.
func TestResponsesReasoningPublicExtensionContract(t *testing.T) {
	const opaque = "synthetic-contract-private-value"
	item := `{"type":"reasoning","id":"rs_contract","status":"completed","summary":[],"encrypted_content":"` + opaque + `"}`
	body := "data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_contract","summary":[]}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.done","output_index":0,"item":` + item + "}\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_contract","status":"completed","output":[` + item + "],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	var calls atomic.Int32
	requests := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input map[string]any
		if json.NewDecoder(req.Body).Decode(&input) != nil {
			t.Error("invalid request JSON")
		}
		requests <- input
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, body)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_replay","status":"completed","output":[]}`)
		}
	}))
	defer server.Close()
	client := server.Client()
	observed := NewObservedTransport(client.Transport, UsageCollection{Protocol: "openai-responses", MaxBytes: 4096})
	digest := sha256.New()
	client.Transport = responsesProbeTransport(func(req *http.Request) (*http.Response, error) {
		response, err := observed.RoundTrip(req)
		if err == nil && response.Header.Get("Content-Type") == "text/event-stream" {
			// Hash the bytes delivered AFTER the read-through collector, exactly
			// where the real SDK consumes them, without retaining the response.
			response.Body = &responsesProbeBody{Reader: io.TeeReader(response.Body, digest), Closer: response.Body}
		}
		return response, err
	})
	zero, no := 0, false
	adapter, err := agenticopenai.NewResponsesModel(t.Context(), &agenticopenai.ResponsesConfig{APIKey: "synthetic-key", BaseURL: server.URL, Model: "fixture", HTTPClient: client, MaxRetries: &zero, Store: &no})
	if err != nil {
		t.Fatal("adapter creation failed")
	}
	observer := &responsesProbeObserver{}
	ctx := WithRequestObservation(t.Context(), RequestIdentity{ModelCallID: "call", AttemptID: "attempt", Purpose: "agent"}, observer)
	collector := newResponsesCollector(4096)
	reader, err := adapter.Stream(context.WithValue(ctx, responsesCollectorKey{}, collector), []*schema.AgenticMessage{schema.UserAgenticMessage("probe")})
	if err != nil {
		t.Fatal("stream establishment failed")
	}
	defer reader.Close()
	var chunks []*schema.AgenticMessage
	seen := false
	for {
		chunk, recvErr := reader.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal("stream failed")
		}
		for _, block := range chunk.ContentBlocks {
			if block.Reasoning != nil {
				seen = true
				value := reflect.ValueOf(block.Extra[responsesItemIDKey])
				if value.Kind() != reflect.String || value.String() != "rs_contract" || block.StreamingMeta == nil {
					t.Fatal("pinned adapter identity/index contract changed")
				}
			}
		}
		chunk, err = collector.associateResponsesReasoning(chunk)
		if err != nil {
			t.Fatal("public block association failed")
		}
		chunks = append(chunks, chunk)
	}
	patches, err := collector.responsesReasoningPatches()
	if err != nil || !seen {
		t.Fatal("lossless supplementation unavailable")
	}
	chunks = append(chunks, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: patches})
	message, err := schema.ConcatAgenticMessages(chunks)
	if err != nil || len(message.ContentBlocks) != 1 || message.ContentBlocks[0].Reasoning.Signature != opaque {
		t.Fatal("real SDK output was not supplemented exactly once")
	}
	expected := sha256.Sum256([]byte(body))
	if string(digest.Sum(nil)) != string(expected[:]) {
		t.Fatal("read-through collector changed HTTP response bytes")
	}
	snapshot, _ := json.Marshal(collector.Snapshot())
	if strings.Contains(string(snapshot), opaque) || strings.Contains(string(snapshot), "rs_contract") {
		t.Fatal("private replay data escaped through usage snapshot")
	}
	_, err = adapter.Generate(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("probe"), message, schema.UserAgenticMessage("continue")})
	if err != nil {
		t.Fatal("real SDK replay failed")
	}
	<-requests
	replay := <-requests
	found := 0
	for _, entry := range replay["input"].([]any) {
		item := entry.(map[string]any)
		if item["type"] == "reasoning" {
			found++
			if item["id"] != "rs_contract" || item["encrypted_content"] != opaque {
				t.Fatal("pinned adapter input replay contract changed")
			}
		}
	}
	if found != 1 || calls.Load() != 2 || observer.count.Load() != 2 {
		t.Fatal("replay or physical request count changed")
	}
}
