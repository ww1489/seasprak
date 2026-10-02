package state_test

import (
	"context"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

func TestSelectionRecordRoundTripsAndSupersedesPending(t *testing.T) {
	backend, err := memory.Open("selection-records", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "selection-records")
	if err != nil {
		t.Fatal(err)
	}
	scope := agent.ExecutionScope{SessionID: "selection-records", BranchID: "main", Generation: "gen"}
	first := state.Selection{ID: "selection-1", Scope: scope, Kind: "model", State: "pending", ModelName: "model-a", ModelVersion: "v1", Revision: 1}
	firstReceipt, err := manager.AcceptSelection(context.Background(), state.OperationCommand{Principal: "user", Kind: "select_model", Target: "next_trace", IdempotencyKey: "first", ExpectedRevision: 0, Content: []byte(`{"name":"model-a","version":"v1"}`)}, first)
	if err != nil {
		t.Fatal(err)
	}
	if firstReceipt.State != "accepted" || firstReceipt.OperationID == "" {
		t.Fatalf("unexpected first receipt: %+v", firstReceipt)
	}
	second := state.Selection{ID: "selection-2", Scope: scope, Kind: "model", State: "pending", ModelName: "model-b", ModelVersion: "v2", Revision: 1, Supersedes: first.ID}
	secondReceipt, err := manager.AcceptSelection(context.Background(), state.OperationCommand{Principal: "user", Kind: "select_model", Target: "next_trace", IdempotencyKey: "second", ExpectedRevision: manager.View().LastSeq, Content: []byte(`{"name":"model-b","version":"v2"}`)}, second)
	if err != nil {
		t.Fatal(err)
	}
	if secondReceipt.OperationID == firstReceipt.OperationID {
		t.Fatal("distinct selections reused an operation")
	}
	view := manager.View()
	if view.Selections[first.ID].State != "superseded" || view.Selections[second.ID].State != "pending" {
		t.Fatalf("pending selection lifecycle not persisted: %+v", view.Selections)
	}
	if _, err := state.NewManager(backend, "selection-records"); err != nil {
		t.Fatal(err)
	}
}
