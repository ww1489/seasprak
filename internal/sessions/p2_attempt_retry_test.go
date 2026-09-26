package sessions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type p2AttemptObservedModel struct {
	client *http.Client
	script *testkit.FakeModel
}

func (m *p2AttemptObservedModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	panic("real stream required")
}
func (m *p2AttemptObservedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid", nil)
	response, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	return m.script.Stream(ctx, in, opts...)
}
func TestP2AttemptStreamingRetryDoesNotRerunTool(t *testing.T) {
	backend, err := memory.Open("retry-attempt", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "retry-attempt")
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var sent, runs atomic.Int32
	model := &p2AttemptObservedModel{script: testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "write", Arguments: `{}`}}}, testkit.Step{Err: &product.Error{Code: product.CodeResourceUnavailable, Message: "temporary", Retryable: true}}, testkit.Step{Text: "done"})}
	model.client = &http.Client{Transport: llm.NewObservedTransport(sessionWire(func(r *http.Request) (*http.Response, error) {
		if sent.Add(1) == 1 {
			close(reached)
			<-release
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))}
	opts := Options{SessionID: "retry-attempt", Profile: ProfileMemory, Store: backend, Model: llm.WithObservedTransportModel(model), Tools: []tools.Definition{{Name: "write", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "written", nil }}}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	session, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"write"}`)})
	if err != nil {
		t.Fatal(err)
	}
	<-reached
	frame := activityFrame(t, session)
	close(release)
	activityWait(t, frame)
	v := manager.View()
	if sent.Load() != 3 || runs.Load() != 1 || len(v.ModelAttempts) != 3 || len(v.AttemptResults) != 3 || len(v.Turns) != 2 || v.Traces[receipt.TraceID].State != "completed" {
		t.Fatalf("retry result: requests=%d runs=%d attempts=%d terminal=%d turns=%d trace=%s error=%s", sent.Load(), runs.Load(), len(v.ModelAttempts), len(v.AttemptResults), len(v.Turns), v.Traces[receipt.TraceID].State, v.Traces[receipt.TraceID].Error)
	}
	accepted, failed := 0, 0
	for _, result := range v.AttemptResults {
		if result.State == "accepted" {
			accepted++
		} else if result.State == "failed" {
			failed++
		}
	}
	if accepted != 2 || failed != 1 {
		t.Fatalf("accepted=%d failed=%d", accepted, failed)
	}
}
