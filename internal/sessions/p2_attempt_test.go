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
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type p2AttemptStreamModel struct {
	*sessionTransportModel
	streams atomic.Int32
}

func (m *p2AttemptStreamModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.streams.Add(1)
	return m.sessionTransportModel.Stream(ctx, in, opts...)
}

func TestP2AttemptRegisteredBeforeTransportAndAcceptedAtomically(t *testing.T) {
	backend, err := memory.Open("attempt-session", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "attempt-session")
	if err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	m := &p2AttemptStreamModel{sessionTransportModel: &sessionTransportModel{started: make(chan context.Context, 1), release: make(chan struct{})}}
	m.client = &http.Client{Transport: llm.NewObservedTransport(sessionWire(func(r *http.Request) (*http.Response, error) {
		sent.Add(1)
		v := manager.View()
		request := v.Traces[v.ActiveTrace].Usage.LastTransport
		attempt, ok := v.ModelAttempts[request.AttemptID]
		if !ok || attempt.ModelCallID != request.ModelCallID || attempt.MessageID == "" || attempt.StreamID == "" || attempt.State != "started" {
			t.Error("physical request has no durable started attempt with candidate identities")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	}))}
	session, err := Start(Options{SessionID: "attempt-session", Profile: ProfileMemory, Store: backend, Model: llm.WithObservedTransportModel(m)}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	_, err = session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)})
	if err != nil {
		t.Fatal(err)
	}
	<-m.started
	frame := activityFrame(t, session)
	// Opening the journal while an attempt is active must preserve the unfinished fact.
	reopened, err := state.NewManager(backend, "attempt-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.View().ModelAttempts) != 1 {
		t.Error("unfinished attempt missing on replay")
	}
	close(m.release)
	activityWait(t, frame)
	if m.streams.Load() != 1 {
		t.Error("session did not use real model stream")
	}
	v := manager.View()
	if sent.Load() != 2 || len(v.ModelAttempts) != 1 || len(v.Calls) != 0 {
		t.Fatalf("requests=%d attempts=%d tools=%d", sent.Load(), len(v.ModelAttempts), len(v.Calls))
	}
	stored, err := backend.Load(t.Context(), "attempt-session")
	if err != nil {
		t.Fatal(err)
	}
	terminalCount := 0
	for _, commit := range stored.Commits {
		for _, record := range commit.ControlRecords {
			if record.Type != "model_attempt_transition" {
				continue
			}
			terminalCount++
			var result struct {
				AttemptID string `json:"attemptId"`
				State     string `json:"state"`
			}
			if err := json.Unmarshal(record.Payload, &result); err != nil {
				t.Fatal(err)
			}
			attempt := v.ModelAttempts[result.AttemptID]
			if result.State != "accepted" || len(commit.Entries) != 1 || commit.Entries[0].ID != attempt.MessageID {
				t.Error("acceptance and original candidate were not committed together")
			}
		}
	}
	if terminalCount != 1 {
		t.Fatalf("attempt terminal records=%d", terminalCount)
	}
}
