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

type releaseReplayStore struct {
	store.Store
	mutate func(*store.Commit)
}

func (s releaseReplayStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	stored, err := s.Store.Load(ctx, id)
	if err != nil {
		return stored, err
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return stored, err
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return stored, err
	}
	s.mutate(&stored.Commits[len(stored.Commits)-1])
	return stored, nil
}

func TestResourceHoldReleaseReplayRequiresCompleteTrustedTransaction(t *testing.T) {
	_, backend := fixture(t)
	original := agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true}
	call := agent.ToolRecord{Call: agent.FrozenCall{CallID: "call"}, Scope: agent.ExecutionScope{SessionID: "session", TraceID: "trace", InvocationID: "invocation", ExecutionID: "execution"}, Claimed: true, Observation: &original}
	trace := state.TraceState{ID: "trace", State: "paused", Started: true, InvocationID: "invocation", ExecutionID: "execution", ExecutionStopped: true, Activity: state.ActivityBudget{Known: true}}
	callRaw, _ := json.Marshal(call)
	traceRaw, _ := json.Marshal(trace)
	if _, err := backend.Append(t.Context(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "seed", CommitSeq: 1, ControlRecords: []store.Record{{Type: "trace", ID: trace.ID, Version: 1, Payload: traceRaw}, {Type: "tool_call", ID: "call", Version: 1, Payload: callRaw}}}); err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "session")
	if err != nil {
		t.Fatal(err)
	}
	before := manager.View()
	receipt, err := manager.AcceptOperation(t.Context(), state.OperationCommand{Principal: "operator", Kind: "reconcile", Target: "call", ExpectedRevision: before.LastSeq, Content: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.TransitionOperation(t.Context(), receipt.OperationID, 1, "running", "", ""); err != nil {
		t.Fatal(err)
	}
	next := state.ObservationRevision{ID: "no-start", CallID: "call", Version: 2, PreviousID: "legacy:call", Observation: agent.ToolObservation{Status: "cancelled", SideEffect: "none"}}
	result := state.Reconciliation{ID: "result", OperationID: receipt.OperationID, CallID: "call", ObservationID: next.PreviousID, ObservationVersion: 1, NewObservationID: next.ID, QueryID: "trusted-query", TrustedNoStart: true, EvidenceSource: "executor", EvidenceRefs: []string{"proof"}}
	if err := manager.CommitReconciliation(t.Context(), receipt.OperationID, 2, next, result); err != nil {
		t.Fatal(err)
	}
	after := manager.View()
	if len(after.ResourceHoldReleases) != 1 || !after.ResourceHoldReleased("call") || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Observations["legacy:call"], after.Observations["legacy:call"]) {
		t.Fatal("release missing or historical facts changed")
	}
	for _, kind := range []string{"resource_hold_release", "observation_revision", "reconciliation", "operation"} {
		t.Run("missing-"+kind, func(t *testing.T) {
			_, err := state.NewManager(releaseReplayStore{Store: backend, mutate: func(c *store.Commit) {
				for i, r := range c.ControlRecords {
					if r.Type == kind {
						c.ControlRecords = append(c.ControlRecords[:i], c.ControlRecords[i+1:]...)
						break
					}
				}
			}}, "session")
			requireP2Code(t, err, product.CodeStateConflict)
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(*state.ResourceHoldRelease)
	}{
		{"execution", func(r *state.ResourceHoldRelease) { r.ExecutionID = "other-execution" }},
		{"call", func(r *state.ResourceHoldRelease) { r.CallID = "other-call" }},
		{"observation", func(r *state.ResourceHoldRelease) { r.ObservationID = "legacy:call" }},
	} {
		t.Run("mismatched-"+tc.name, func(t *testing.T) {
			_, err := state.NewManager(releaseReplayStore{Store: backend, mutate: func(c *store.Commit) {
				for i, r := range c.ControlRecords {
					if r.Type != "resource_hold_release" {
						continue
					}
					var release state.ResourceHoldRelease
					if err := json.Unmarshal(r.Payload, &release); err != nil {
						t.Fatal(err)
					}
					tc.mutate(&release)
					c.ControlRecords[i].Payload, _ = json.Marshal(release)
				}
			}}, "session")
			requireP2Code(t, err, product.CodeStateConflict)
		})
	}
	// A late confirmed effect is appended, never allowed to erase the release
	// audit record, but makes that release ineffective before and after replay.
	late := state.ObservationRevision{ID: "late", CallID: "call", Version: 3, PreviousID: next.ID, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true}}
	if err := manager.AppendObservation(t.Context(), 2, late, nil); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []state.View{manager.View(), reopened.View()} {
		if view.ResourceHoldReleased("call") || !view.ReconciliationUnresolved("call") || len(view.ResourceHoldReleases) != 1 || !reflect.DeepEqual(view.Calls, before.Calls) {
			t.Fatal("late confirmed fact did not invalidate release while preserving history")
		}
	}
}
