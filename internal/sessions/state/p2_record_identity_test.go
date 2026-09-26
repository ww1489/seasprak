package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestP2ReconciliationOperationTargetMustMatchCall(t *testing.T) {
	_, s := fixture(t)
	seedP2Call(t, s, &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true})
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.LatestObservation("call")
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.AcceptOperation(context.Background(), state.OperationCommand{Kind: "reconcile", Target: "different-call", Content: json.RawMessage(`{}`), ExpectedRevision: m.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	next := state.ObservationRevision{ID: "next", CallID: "call", Version: 2, PreviousID: first.ID, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true}}
	reconciliation := state.Reconciliation{ID: "reconciliation", OperationID: r.OperationID, CallID: "call", ObservationID: first.ID, ObservationVersion: first.Version, NewObservationID: next.ID, EvidenceRefs: []string{"evidence"}, EvidenceSource: "executor"}
	err = m.AppendObservation(context.Background(), first.Version, next, &reconciliation)
	requireP2Code(t, err, product.CodeStateConflict)
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("mismatched operation committed observation")
	}
}
