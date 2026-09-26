package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

func TestP2AttemptChatFactoryTruncatedToolNeverRuns(t *testing.T) {
	backend, err := memory.Open("chat-attempt", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "chat-attempt")
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var sent, runs atomic.Int32
	catalog := llm.NewCatalog(nil)
	err = catalog.RegisterOpenAIChat(&http.Client{Transport: sessionWire(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.Unmarshal(raw, &request); err != nil || !request.Stream {
			t.Error("Chat factory request is not streaming")
		}
		close(reached)
		<-release
		body := "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"provider\",\"type\":\"function\",\"function\":{\"name\":\"write\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	declared := llm.Capability{Status: llm.Declared, Evidence: []string{"offline attempt fixture"}}
	cfg := llm.ModelConfig{Provider: "fixture", Protocol: "openai-chat", Model: "fixture", Version: "v1", Endpoint: "https://example.invalid/v1", NoCredentials: true, AccountScope: "local", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapText: declared, llm.CapTextStream: declared, llm.CapTools: declared, llm.CapContextWindow: declared, llm.CapOutputLimit: declared, llm.CapPhysicalRequestMetering: declared}, ContextWindowTokens: 4096, MaxOutputTokens: 512}, Parameters: llm.ModelParameters{MaxOutputTokens: 128, PolicyVersion: "fixture"}}
	if err := catalog.Register(cfg); err != nil {
		t.Fatal(err)
	}
	model, err := catalog.Bind(cfg.Key(), llm.RequestedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{SessionID: "chat-attempt", Profile: ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{{Name: "write", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "written", nil }}}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	session, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	// A subscriber which never reads must not prevent model finalization.
	sub := session.SubscribeEvents(config.Limits{SubscriptionEvents: 1})
	defer sub.Close()
	receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"write"}`)})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	frame := activityFrame(t, session)
	close(release)
	activityWait(t, frame)
	v := manager.View()
	if sent.Load() != 1 || runs.Load() != 0 || len(v.Calls) != 0 || len(v.ModelAttempts) != 1 || len(v.AttemptResults) != 1 || v.Traces[receipt.TraceID].State != "failed" {
		t.Fatalf("truncated tool boundary: requests=%d runs=%d attempts=%d results=%d calls=%d trace=%s", sent.Load(), runs.Load(), len(v.ModelAttempts), len(v.AttemptResults), len(v.Calls), v.Traces[receipt.TraceID].State)
	}
	for _, result := range v.AttemptResults {
		if result.State != "incomplete" || result.FinishReason != "length" {
			t.Fatal("truncation terminal lost")
		}
	}
	if sub.Err() == nil {
		t.Error("slow subscriber was not detached")
	}
	for _, event := range v.Events {
		if event.Type == "message.snapshot" {
			t.Error("temporary snapshot was persisted")
		}
	}
}
