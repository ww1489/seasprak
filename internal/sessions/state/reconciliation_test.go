package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestCommitReconciliationCompletesOperationAndKeepsOriginalFacts(t *testing.T) {
	ctx := context.Background()
	_, s := fixture(t)
	original := agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true, Content: "effect uncertain"}
	seedP2Call(t, s, &original)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.LatestObservation("call")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := m.AcceptOperation(ctx, state.OperationCommand{Principal: "operator", Kind: "reconcile", Target: "call", ExpectedRevision: m.View().LastSeq, Content: json.RawMessage(`{"observationVersion":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionOperation(ctx, receipt.OperationID, 1, "running", "", ""); err != nil {
		t.Fatal(err)
	}
	next := state.ObservationRevision{ID: "no-start", CallID: "call", Version: 2, PreviousID: first.ID, Observation: agent.ToolObservation{Status: "cancelled", SideEffect: "none", Executed: false, Content: "trusted no-start"}}
	result := state.Reconciliation{ID: "reconciliation", OperationID: receipt.OperationID, CallID: "call", ObservationID: first.ID, ObservationVersion: first.Version, NewObservationID: next.ID, EvidenceRefs: []string{"query-evidence"}, EvidenceSource: "query:test", CanResume: false, ResumeReason: "no compatible checkpoint"}
	if err := m.CommitReconciliation(ctx, receipt.OperationID, 2, next, result); err != nil {
		t.Fatal(err)
	}
	view := m.View()
	if view.Operations[receipt.OperationID].State != "completed" || view.Operations[receipt.OperationID].Revision != 3 || !reflect.DeepEqual(view.Calls["call"].Observation, &original) {
		t.Fatalf("reconciliation changed original operation facts: %+v", view)
	}
	if view.HasUnresolvedEffects() || view.TraceHasUnresolvedEffects("missing") || len(view.Reconciliations) != 1 {
		t.Fatalf("trusted no-start did not clear derived conflict: %+v", view)
	}
	hasStateChange := false
	for _, event := range view.Events {
		if event.Type == "tool.state_changed" {
			hasStateChange = true
			break
		}
	}
	if !hasStateChange {
		t.Fatal("reconciliation did not publish a durable tool state change")
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.View().Operations[receipt.OperationID].State != "completed" || len(reopened.View().Reconciliations) != 1 || reopened.View().Calls["call"].Observation == nil || reopened.View().Calls["call"].Observation.SideEffect != "unknown" {
		t.Fatal("reconciliation facts did not replay without rewriting the original call")
	}
}

func TestCommitReconciliationRejectsStaleEvidenceWithoutChangingView(t *testing.T) {
	ctx := context.Background()
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
	receipt, err := m.AcceptOperation(ctx, state.OperationCommand{Kind: "reconcile", Target: "call", ExpectedRevision: m.View().LastSeq, Content: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionOperation(ctx, receipt.OperationID, 1, "running", "", ""); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	next := state.ObservationRevision{ID: "stale", CallID: "call", Version: 3, PreviousID: "wrong", Observation: agent.ToolObservation{Status: "cancelled", SideEffect: "none"}}
	result := state.Reconciliation{ID: "reconciliation", OperationID: receipt.OperationID, CallID: "call", ObservationID: first.ID, ObservationVersion: first.Version, NewObservationID: next.ID, EvidenceRefs: []string{"evidence"}, EvidenceSource: "query:test"}
	err = m.CommitReconciliation(ctx, receipt.OperationID, 2, next, result)
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeStateConflict || !reflect.DeepEqual(before, m.View()) {
		t.Fatalf("stale reconciliation changed state: err=%v before=%+v after=%+v", err, before, m.View())
	}
}
