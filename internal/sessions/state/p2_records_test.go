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
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
)

// The raw journal fixture deliberately compiles against the pre-P2 implementation.
// It proves missing replay behavior rather than merely missing Go declarations.
func TestP2RecordsJournalReplay(t *testing.T) {
	for _, tc := range []struct{ kind, field, payload string }{
		{"model_attempt", "ModelAttempts", `{"id":"record","modelCallId":"model","messageId":"message","streamId":"stream","scope":{"SessionID":"session"},"attempt":1,"purpose":"agent","state":"registered"}`},
		{"frozen_execution", "FrozenExecutions", `{"id":"record","callId":"call","hash":"hash","origin":"direct","scope":{"SessionID":"session"},"tool":"read","generation":"g"}`},
		{"interaction", "Interactions", `{"id":"record","kind":"approval","callId":"call","state":"pending","approvalId":"approval"}`},
		{"approval", "Approvals", `{"id":"record","interactionId":"interaction","callId":"call","frozenExecutionId":"frozen","frozenHash":"hash","state":"asked"}`},
		{"operation", "Operations", `{"receipt":{"operationId":"record","state":"accepted","target":"trace","acceptedCommit":1},"principal":"user","sessionId":"session","kind":"pause","key":"k","digest":"hash","revision":1,"state":"accepted"}`},
		{"observation_revision", "Observations", `{"id":"record","callId":"call","version":1,"observation":{"Status":"outcome_unknown","SideEffect":"unknown"}}`},
		{"reconciliation", "Reconciliations", `{"id":"record","operationId":"operation","callId":"call","observationId":"observation","observationVersion":1,"newObservationId":"next","evidenceRefs":["evidence"]}`},
		{"checkpoint_ref", "Checkpoints", `{"id":"record","blobHash":"hash","scope":{"SessionID":"session","ExecutionID":"execution"},"target":{"Name":"main","Generation":"g"},"codecVersion":"1","einoVersion":"0.9.21"}`},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			_, s := fixture(t)
			controls := []store.Record{}
			if tc.kind == "observation_revision" || tc.kind == "reconciliation" {
				controls = append(controls, store.Record{Type: "tool_call", Version: 1, ID: "call", Payload: json.RawMessage(`{"Call":{"CallID":"call"}}`)})
			}
			if tc.kind == "reconciliation" {
				controls = append(controls,
					store.Record{Type: "operation", Version: 1, ID: "operation", Payload: json.RawMessage(`{"receipt":{"operationId":"operation","state":"accepted","target":"call","acceptedCommit":1},"sessionId":"session","kind":"reconcile","digest":"hash","revision":1,"state":"accepted"}`)},
					store.Record{Type: "observation_revision", Version: 1, ID: "observation", Payload: json.RawMessage(`{"id":"observation","callId":"call","version":1,"observation":{"Status":"outcome_unknown","SideEffect":"unknown"}}`)},
					store.Record{Type: "observation_revision", Version: 1, ID: "next", Payload: json.RawMessage(`{"id":"next","callId":"call","version":2,"previousId":"observation","observation":{"Status":"succeeded","SideEffect":"confirmed"}}`)},
				)
			}
			controls = append(controls, store.Record{Type: tc.kind, Version: 1, ID: "record", Payload: json.RawMessage(tc.payload)})
			_, err := s.Append(context.Background(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "commit", CommitSeq: 1, ControlRecords: controls})
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := state.NewManager(s, "session")
			if err != nil {
				t.Fatalf("saved P2 record cannot reopen: %v", err)
			}
			raw, _ := json.Marshal(reopened.View())
			var fields map[string]map[string]json.RawMessage
			var view map[string]json.RawMessage
			if err := json.Unmarshal(raw, &view); err != nil {
				t.Fatal(err)
			}
			fields = make(map[string]map[string]json.RawMessage)
			var items map[string]json.RawMessage
			if err := json.Unmarshal(view[tc.field], &items); err != nil {
				t.Fatal(err)
			}
			fields[tc.field] = items
			if len(fields[tc.field]["record"]) == 0 {
				t.Fatal("record lost on reopen")
			}
		})
	}
}

func TestP2SaveRecordsRoundTripAndCAS(t *testing.T) {
	ctx := context.Background()
	m, s := fixture(t)
	records := state.Records{
		ModelAttempts:    []state.ModelAttempt{{ID: "attempt", ModelCallID: "model", MessageID: "message", StreamID: "stream", Attempt: 1, Purpose: "agent", State: "registered", ModelConfigVersion: "config"}},
		FrozenExecutions: []state.FrozenExecution{{ID: "frozen", CallID: "call", Hash: "hash", Origin: "direct", Tool: "read", Generation: "generation", Resources: []state.ExecutionResource{{Identity: "file", ExpectedVersion: "v1"}}, Argv: []string{"read", "file"}, Mounts: []state.ExecutionMount{{SourceRef: "source", Target: "target", ReadOnly: true}}}},
		Interactions:     []state.Interaction{{ID: "interaction", Kind: "approval", CallID: "call", ApprovalID: "approval", State: "pending", Options: []string{"allowed-once", "rejected"}}},
		Approvals:        []state.Approval{{ID: "approval", InteractionID: "interaction", CallID: "call", FrozenExecutionID: "frozen", FrozenHash: "hash", State: "asked"}},
		Checkpoints:      []state.CheckpointRef{{ID: "checkpoint", BlobHash: "blob", BlobSize: 42, UnfinishedTurnIDs: []string{"turn"}, CallIDs: []string{"call"}, InteractionIDs: []string{"interaction"}, EinoVersion: "0.9.21", CodecVersion: "1"}},
	}
	if err := m.SaveRecords(ctx, 0, records); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	view := reopened.View()
	if !reflect.DeepEqual(view.ModelAttempts["attempt"], records.ModelAttempts[0]) || !reflect.DeepEqual(view.FrozenExecutions["frozen"], records.FrozenExecutions[0]) || !reflect.DeepEqual(view.Interactions["interaction"], records.Interactions[0]) || !reflect.DeepEqual(view.Approvals["approval"], records.Approvals[0]) || !reflect.DeepEqual(view.Checkpoints["checkpoint"], records.Checkpoints[0]) {
		t.Fatal("typed records did not round trip")
	}
	records.FrozenExecutions[0].Argv[0] = "mutated"
	view.Interactions["interaction"].Options[0] = "mutated"
	if reopened.View().FrozenExecutions["frozen"].Argv[0] != "read" || reopened.View().Interactions["interaction"].Options[0] != "allowed-once" {
		t.Fatal("record storage aliases caller")
	}
	requireP2Code(t, reopened.SaveRecords(ctx, 0, records), product.CodeStateConflict)
	requireP2Code(t, reopened.SaveRecords(ctx, 1, records), product.CodeStateConflict)
	if reopened.View().LastSeq != 1 {
		t.Fatal("CAS/immutable failure appended")
	}
	before := reopened.View()
	s.fail = true
	err = reopened.SaveRecords(ctx, 1, state.Records{Checkpoints: []state.CheckpointRef{{ID: "new"}}})
	if err == nil || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("failed record commit changed view")
	}
}

type unknownVersionStore struct{ store.Store }

func (s unknownVersionStore) Load(ctx context.Context, id string) (store.StoredSession, error) {
	stored, err := s.Store.Load(ctx, id)
	if err == nil {
		stored.Commits[0].ControlRecords[0].Version = 2
	}
	return stored, err
}

func TestP2UnknownCriticalRecordVersion(t *testing.T) {
	for _, kind := range []string{"model_attempt", "frozen_execution", "interaction", "approval", "checkpoint_ref", "operation", "observation_revision", "reconciliation"} {
		t.Run(kind, func(t *testing.T) {
			_, s := fixture(t)
			_, err := s.Append(context.Background(), "session", store.ExpectedCommit{}, store.Commit{RecordType: "commit", Version: 1, CommitID: "commit", CommitSeq: 1, ControlRecords: []store.Record{{Type: kind, Version: 1, ID: "record", Payload: json.RawMessage(`{}`)}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = state.NewManager(unknownVersionStore{s}, "session")
			requireP2Code(t, err, product.CodeIncompatibleVersion)
		})
	}
}

func TestP2FileJournalClosesAndReopens(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seedP2Call(t, s, &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true})
	m, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	records := state.Records{
		ModelAttempts:    []state.ModelAttempt{{ID: "attempt", ModelCallID: "model", MessageID: "message", StreamID: "stream", Attempt: 1, Purpose: "agent", State: "registered"}},
		FrozenExecutions: []state.FrozenExecution{{ID: "frozen", CallID: "call", Hash: "hash", Origin: "direct", Tool: "write", Resources: []state.ExecutionResource{{Identity: "file", ExpectedVersion: "v1"}}}},
		Interactions:     []state.Interaction{{ID: "interaction", ApprovalID: "approval", CallID: "call", Kind: "approval", State: "pending", Options: []string{"allowed-once", "rejected"}}},
		Approvals:        []state.Approval{{ID: "approval", InteractionID: "interaction", CallID: "call", FrozenExecutionID: "frozen", FrozenHash: "hash", State: "asked"}},
		Checkpoints:      []state.CheckpointRef{{ID: "checkpoint", BlobHash: "blob", BlobSize: 42, CallIDs: []string{"call"}, InteractionIDs: []string{"interaction"}, EinoVersion: "0.9.21", CodecVersion: "1"}},
	}
	if err := m.SaveRecords(ctx, 1, records); err != nil {
		t.Fatal(err)
	}
	cmd := state.OperationCommand{Kind: "reconcile", Target: "call", IdempotencyKey: "retry", ExpectedRevision: 2, Content: json.RawMessage(`{}`)}
	receipt, err := m.AcceptOperation(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	original, err := m.LatestObservation("call")
	if err != nil {
		t.Fatal(err)
	}
	next := state.ObservationRevision{ID: "observation", CallID: "call", PreviousID: original.ID, Version: 2, Observation: agent.ToolObservation{Status: "succeeded", SideEffect: "confirmed", Executed: true}, ArtifactRefs: []string{"artifact"}}
	reconciliation := state.Reconciliation{ID: "reconciliation", OperationID: receipt.OperationID, CallID: "call", ObservationID: original.ID, ObservationVersion: 1, NewObservationID: next.ID, EvidenceRefs: []string{"evidence"}, ConfirmedEffects: []string{"file"}, ResumeReason: "checkpoint not verified"}
	if err := m.AppendObservation(ctx, 1, next, &reconciliation); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedStore, err := jsonl.Open("session", root, store.Header{}, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedStore.Close()
	reopened, err := state.NewManager(reopenedStore, "session")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("file close/reopen lost P2 facts")
	}
	retried, err := reopened.AcceptOperation(ctx, cmd)
	if err != nil || receipt != retried || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatal("retry after file reopen changed original acceptance", err)
	}
}

func requireP2Code(t *testing.T, err error, code string) {
	t.Helper()
	var got *product.Error
	if !errors.As(err, &got) || got.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
