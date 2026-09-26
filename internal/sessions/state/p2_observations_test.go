package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func seedP2Call(t *testing.T, s store.Store, observation *agent.ToolObservation) {
	t.Helper()
	call := agent.ToolRecord{Call: agent.FrozenCall{CallID: "call", Name: "write", Hash: "frozen-hash"}, Scope: agent.ExecutionScope{SessionID: "session", InvocationID: "invocation"}, Claimed: true, Observation: observation}
	raw, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Append(context.Background(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "legacy", CommitSeq: 1, ControlRecords: []store.Record{{Type: "tool_call", ID: "call", Version: 1, Payload: raw}}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestP2ObservationLegacyAppendAndReopen(t *testing.T) {
	ctx := context.Background()
	_, s := fixture(t)
	original := agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true, Content: "unknown"}
	seedP2Call(t, s, &original)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.LatestObservation("call")
	if err != nil || first.Version != 1 || first.PreviousID != "" || first.Observation != original {
		t.Fatalf("legacy observation is not initial revision: %+v %v", first, err)
	}
	receipt, err := m.AcceptOperation(ctx, state.OperationCommand{Kind: "reconcile", Target: "call", Content: json.RawMessage(`{}`), ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	next := state.ObservationRevision{ID: "observation-2", CallID: "call", PreviousID: first.ID, Version: 2, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true}, DetailsRef: "details", ArtifactRefs: []string{"artifact"}, FileFactsRef: "facts", StartEvidenceRef: "started"}
	reconciliation := state.Reconciliation{ID: "reconciliation", OperationID: receipt.OperationID, CallID: "call", ObservationID: first.ID, ObservationVersion: 1, NewObservationID: next.ID, EvidenceRefs: []string{"evidence"}, EvidenceSource: "executor", ConfirmedEffects: []string{"file"}, ConflictRestrictions: []string{"other-call"}, CanResume: false, ResumeReason: "checkpoint unavailable"}
	if err := m.AppendObservation(ctx, 1, next, &reconciliation); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := reopened.LatestObservation("call")
	if err != nil || !reflect.DeepEqual(latest, next) {
		t.Fatalf("lost appended observation: %+v %v", latest, err)
	}
	v := reopened.View()
	if v.Calls["call"].Observation == nil || *v.Calls["call"].Observation != original || !reflect.DeepEqual(v.Observations[first.ID], first) || !reflect.DeepEqual(v.Reconciliations[reconciliation.ID], reconciliation) {
		t.Fatal("reconciliation overwrote original facts")
	}
	before := reopened.View()
	requireP2Code(t, reopened.AppendObservation(ctx, 1, next, &reconciliation), product.CodeStateConflict)
	changed := next
	changed.Version = 3
	changed.PreviousID = next.ID
	changed.Observation.Content = "overwrite"
	requireP2Code(t, reopened.AppendObservation(ctx, 2, changed, nil), product.CodeStateConflict)
	if !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("rejected revision changed view")
	}
	call := v.Calls["call"]
	call.Observation = &next.Observation
	requireP2Code(t, reopened.SaveCall(ctx, call), product.CodeStateConflict)
}
func TestP2ObservationCannotEraseConfirmedExecution(t *testing.T) {
	_, s := fixture(t)
	seedP2Call(t, s, &agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true})
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	original, err := m.LatestObservation("call")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	for _, observation := range []agent.ToolObservation{
		{Status: "denied", SideEffect: "none", Executed: false},
		{Status: "failed", SideEffect: "none", Executed: true},
	} {
		err = m.AppendObservation(context.Background(), 1, state.ObservationRevision{ID: "replacement", CallID: "call", PreviousID: original.ID, Version: 2, Observation: observation}, nil)
		requireP2Code(t, err, product.CodeStateConflict)
		if !reflect.DeepEqual(before, m.View()) {
			t.Fatal("new evidence erased confirmed facts")
		}
	}
}

func TestP2ObservationFailedCommitInvisible(t *testing.T) {
	_, s := fixture(t)
	seedP2Call(t, s, nil)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	s.fail = true
	err = m.AppendObservation(context.Background(), 0, state.ObservationRevision{ID: "observation", CallID: "call", Version: 1, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "none"}}, nil)
	if err == nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatal("failed observation append changed view", err)
	}
}
func TestP2ObservationInitialSaveReopen(t *testing.T) {
	_, s := fixture(t)
	seedP2Call(t, s, nil)
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	first := state.ObservationRevision{ID: "first", CallID: "call", Version: 1, Observation: agent.ToolObservation{Status: "failed", SideEffect: "none"}}
	if err := m.AppendObservation(context.Background(), 0, first, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.LatestObservation("call")
	if err != nil || !reflect.DeepEqual(got, first) {
		t.Fatal("initial observation lost", err)
	}
}
