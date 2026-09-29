package sessions

import (
	"context"
	"reflect"
	"sync"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func approvalSnapshot(t *testing.T, s *AgentSession) Snapshot {
	t.Helper()
	snap, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestApprovalResponseIsRuntimeOnly(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	before := f.manager.View()
	snap := approvalSnapshot(t, f.s)
	var id string
	for key := range snap.Interactions {
		id = key
	}
	response := InteractionResponse{InteractionID: id, Decision: "allowed-once", ExpectedRevision: snap.Revision, IdempotencyKey: "answer"}
	receipt, err := f.s.RespondInteraction(t.Context(), response)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.manager.View()) {
		t.Fatal("approval response changed durable journal view/revision/cursor")
	}
	if receipt.AcceptedCommit != 0 || receipt.State != "accepted" {
		t.Fatalf("non-durable receipt = %+v", receipt)
	}
	after := approvalSnapshot(t, f.s)
	if after.Interactions[id].State != "allowed-once" || after.Operations[receipt.OperationID].State != "completed" {
		t.Fatal("runtime answer/status missing")
	}
	status, err := f.s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	retry, err := f.s.RespondInteraction(t.Context(), response)
	if err != nil || retry != receipt {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	response.Decision = "rejected"
	_, err = f.s.RespondInteraction(t.Context(), response)
	requireSessionCode(t, err, product.CodeIdempotencyConflict)
	response.IdempotencyKey = "another"
	_, err = f.s.RespondInteraction(t.Context(), response)
	requireSessionCode(t, err, product.CodeStateConflict)
	if f.runs.Load() != 0 || f.model.Calls() != 1 {
		t.Fatal("answer executed work")
	}
	if err := f.s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, err = f.s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" {
		t.Fatalf("closed status=%+v err=%v", status, err)
	}
}

func TestApprovalConcurrentOppositeAnswersHaveOneWinner(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	snap := approvalSnapshot(t, f.s)
	var id string
	for key := range snap.Interactions {
		id = key
	}
	before := f.manager.View()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, decision := range []string{"allowed-once", "rejected"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: id, Decision: decision, ExpectedRevision: snap.Revision, IdempotencyKey: decision})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else {
			requireSessionCode(t, err, product.CodeStateConflict)
		}
	}
	if wins != 1 || !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 {
		t.Fatal("competing answers changed durable state or had multiple winners")
	}
}
