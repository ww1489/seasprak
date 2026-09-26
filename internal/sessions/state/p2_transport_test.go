package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type transportWire func(*http.Request) (*http.Response, error)

func (f transportWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestP2TransportBudgetCommitAndReopen(t *testing.T) {
	m, st := fixture(t)
	r := accept(t, m, "transport", `{"text":"hi"}`)
	ctx := context.Background()
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	ledger := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	ledger.SetPersist(func(u agent.Usage) error { return m.SaveTraceBudget(ctx, r.TraceID, u) })
	if err := ledger.BeginTurnID("call"); err != nil {
		t.Fatal(err)
	}
	sends := 0
	transport := llm.NewObservedTransport(transportWire(func(req *http.Request) (*http.Response, error) {
		sends++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
	}))
	invoke := func(attempt, purpose string) error {
		observed := llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: "call", AttemptID: attempt, Purpose: purpose}, ledger)
		req, _ := http.NewRequestWithContext(observed, http.MethodPost, "https://example.invalid", nil)
		res, err := transport.RoundTrip(req)
		if res != nil {
			res.Body.Close()
		}
		return err
	}
	before := m.View().LastSeq
	st.fail = true
	if err := invoke("failed", "agent"); err == nil {
		t.Fatal("failed append accepted")
	}
	if sends != 0 || ledger.Snapshot().TransportRequests != 0 || m.View().LastSeq != before {
		t.Fatal("failed reservation changed state or started transport")
	}
	st.fail = false
	// A failed append deliberately poisons the manager until reopen. Rebuild
	// its committed view rather than pretending the same writer can continue.
	var reopenErr error
	m, reopenErr = state.NewManager(st, "session")
	if reopenErr != nil {
		t.Fatal(reopenErr)
	}
	for _, purpose := range []string{"agent", "cache_create", "cache_rebuild"} {
		if err := invoke(purpose, purpose); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := state.NewManager(st, "session")
	if err != nil {
		t.Fatal(err)
	}
	u := reopened.View().Traces[r.TraceID].Usage
	if u.TransportRequests != 3 || u.ModelRequests != 3 || u.LastTransport.Purpose != "cache_rebuild" || sends != 3 {
		t.Fatalf("lost persisted identity/usage: %+v sends=%d", u, sends)
	}
	events := 0
	for _, event := range reopened.View().Events {
		if event.Type != "model.transport_reserved" {
			continue
		}
		events++
		var value struct {
			Request        llm.TransportRequest
			LogicalRequest int
			TraceRequest   int
		}
		if err := json.Unmarshal(event.Payload, &value); err != nil {
			t.Fatal(err)
		}
		if value.Request.ModelCallID != "call" || value.Request.AttemptID == "" || value.LogicalRequest != events || value.TraceRequest != events {
			t.Fatalf("bad physical record: %+v", value)
		}
	}
	if events != 3 || reopened.View().LastSeq != before+3 {
		t.Fatal("identity and occupancy were not committed together")
	}
	changed := u
	changed.LastTransport.AttemptID = "rewritten"
	if err := reopened.SaveTraceBudget(ctx, r.TraceID, changed); err == nil {
		t.Fatal("transport identity changed without a new reservation")
	}
	ledger = agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	ledger.Restore(u)
	ledger.SetPersist(func(u agent.Usage) error { return reopened.SaveTraceBudget(ctx, r.TraceID, u) })
	if err = ledger.BeginTurnID("call"); err != nil {
		t.Fatal(err)
	}
	err = invoke("after-reopen", "agent")
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeBudgetExhausted || sends != 3 {
		t.Fatalf("reopen refunded quota: %v sends=%d", err, sends)
	}
}
