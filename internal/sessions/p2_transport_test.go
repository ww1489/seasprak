package sessions

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type sessionWire func(*http.Request) (*http.Response, error)

func (f sessionWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type sessionTransportModel struct {
	client  *http.Client
	started chan context.Context
	release chan struct{}
}

func (m *sessionTransportModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	// Auxiliary requests share the attempt's sequence and logical/trace limits.
	for _, purpose := range []string{"cache_query", "agent"} {
		req, _ := http.NewRequestWithContext(llm.WithRequestPurpose(ctx, purpose), http.MethodPost, "https://example.invalid", nil)
		res, err := m.client.Do(req)
		if err != nil {
			return nil, err
		}
		res.Body.Close()
	}
	m.started <- ctx
	select {
	case <-m.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return testkit.NewFake(testkit.Step{Text: "done"}).Generate(ctx, in, opts...)
}
func (m *sessionTransportModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestP2TransportSessionCommitsBeforeWireAndReopens(t *testing.T) {
	backend, err := memory.Open("transport-session", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	manager, err := state.NewManager(backend, "transport-session")
	if err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	m := &sessionTransportModel{started: make(chan context.Context, 1), release: make(chan struct{})}
	m.client = &http.Client{Transport: llm.NewObservedTransport(sessionWire(func(r *http.Request) (*http.Response, error) {
		count := int(sent.Add(1))
		v := manager.View()
		tr := v.Traces[v.ActiveTrace]
		if tr == nil || tr.Usage.TransportRequests != count || tr.Usage.LastTransport.TransportAttempt != uint64(count) {
			t.Error("wire started before committed physical occupancy")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))}
	session, err := Start(Options{SessionID: "transport-session", Profile: ProfileMemory, Store: backend, Model: llm.WithObservedTransportModel(m), Limits: config.Limits{LogicalModelRequests: 2}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	receipt, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.started:
	case <-time.After(time.Second):
		t.Fatal("model did not reach observed transport")
	}
	frame := activityFrame(t, session)
	close(m.release)
	activityWait(t, frame)
	tr := manager.View().Traces[receipt.TraceID]
	if tr.State != "completed" || !tr.ExecutionStopped || tr.Usage.TransportRequests != 2 || tr.Usage.ModelRequests != 2 || sent.Load() != 2 {
		t.Fatalf("session integration mismatch: %+v sent=%d", tr, sent.Load())
	}
	reopened, err := state.NewManager(backend, "transport-session")
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.View().Traces[receipt.TraceID].Usage
	if got != tr.Usage {
		t.Fatal("physical identity changed on replay")
	}
	events := 0
	for _, event := range reopened.View().Events {
		if event.Type == "model.transport_reserved" {
			events++
		}
	}
	if events != 2 {
		t.Fatal("request identity events missing or double charged")
	}
}
