package state_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func TestP2OperationIdempotencyAndTransitions(t *testing.T) {
	ctx := context.Background()
	m, s := fixture(t)
	cmd := state.OperationCommand{Principal: "principal", Kind: "pause", Target: "trace", IdempotencyKey: "key", Content: json.RawMessage(`{"b":2,"a":9007199254740993}`)}
	receipt, err := m.AcceptOperation(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OperationID == "" || receipt.AcceptedCommit != 1 || receipt.State != "accepted" {
		t.Fatalf("bad receipt: %+v", receipt)
	}
	if err := m.TransitionOperation(ctx, receipt.OperationID, 1, "running", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := m.TransitionOperation(ctx, receipt.OperationID, 2, "completed", "result", ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	cmd.Content = json.RawMessage(`{"a":9007199254740993,"b":2}`)
	repeated, err := reopened.AcceptOperation(ctx, cmd)
	if err != nil || receipt != repeated || reopened.View().LastSeq != 3 {
		t.Fatalf("retry changed original receipt/commit: %+v %v", repeated, err)
	}
	got, err := reopened.GetOperation(receipt.OperationID)
	if err != nil || got.State != "completed" || got.AcceptedCommit != receipt.AcceptedCommit || got.ResultRef != "result" {
		t.Fatalf("lost operation: %+v %v", got, err)
	}
	cmd.Content = json.RawMessage(`{"a":9007199254740992,"b":2}`)
	_, err = reopened.AcceptOperation(ctx, cmd)
	requireP2Code(t, err, product.CodeIdempotencyConflict)
	requireP2Code(t, reopened.TransitionOperation(ctx, receipt.OperationID, 2, "failed", "", "error"), product.CodeStateConflict)
	requireP2Code(t, reopened.TransitionOperation(ctx, receipt.OperationID, 3, "running", "", ""), product.CodeStateConflict)
	cmd.IdempotencyKey = "new"
	_, err = reopened.AcceptOperation(ctx, cmd)
	requireP2Code(t, err, product.CodeStateConflict)
	cmd.ExpectedRevision = 3
	cmd.IdempotencyKey = "key"
	cmd.Principal = "other"
	other, err := reopened.AcceptOperation(ctx, cmd)
	if err != nil || other.OperationID == receipt.OperationID {
		t.Fatal("principal not isolated", err)
	}
	cmd.ExpectedRevision = 4
	cmd.Kind = "resume"
	other, err = reopened.AcceptOperation(ctx, cmd)
	if err != nil || other.OperationID == receipt.OperationID {
		t.Fatal("kind not isolated", err)
	}
}

func TestP2OperationTransitionCommitFailure(t *testing.T) {
	m, s := fixture(t)
	receipt, err := m.AcceptOperation(context.Background(), state.OperationCommand{Kind: "pause", Target: "trace", Content: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	before := m.View()
	s.fail = true
	if err := m.TransitionOperation(context.Background(), receipt.OperationID, 1, "running", "", ""); err == nil {
		t.Fatal("expected append failure")
	}
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("failed operation transition changed view")
	}
}

func TestP2OperationFailedCommitInvisible(t *testing.T) {
	m, s := fixture(t)
	before := m.View()
	s.fail = true
	receipt, err := m.AcceptOperation(context.Background(), state.OperationCommand{Kind: "pause", Target: "trace", IdempotencyKey: "key", Content: json.RawMessage(`{}`)})
	if err == nil || receipt.OperationID != "" || !reflect.DeepEqual(before, m.View()) {
		t.Fatal("failed acceptance exposed receipt or state", err)
	}
}

type lostOperationResponseStore struct{ store.Store }

func (s lostOperationResponseStore) Append(ctx context.Context, id string, e store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	_, err := s.Store.Append(ctx, id, e, c)
	if err != nil {
		return store.CommitReceipt{}, err
	}
	return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "response lost after append")
}
func TestP2OperationLostResponseReopensOriginal(t *testing.T) {
	_, s := fixture(t)
	m, err := state.NewManager(lostOperationResponseStore{s}, "session")
	if err != nil {
		t.Fatal(err)
	}
	cmd := state.OperationCommand{Kind: "reconcile", Target: "call", IdempotencyKey: "key", Content: json.RawMessage(`{}`)}
	if _, err = m.AcceptOperation(context.Background(), cmd); err == nil {
		t.Fatal("expected lost response")
	}
	if m.View().LastSeq != 0 {
		t.Fatal("failed append response changed local view")
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	persisted := reopened.View()
	receipt, err := reopened.AcceptOperation(context.Background(), cmd)
	if err != nil || receipt.AcceptedCommit != 1 || persisted.Operations[receipt.OperationID].Receipt != receipt || reopened.View().LastSeq != 1 {
		t.Fatal("lost response duplicated operation", err)
	}
}
func TestP2ConcurrentOperationAcceptance(t *testing.T) {
	m, _ := fixture(t)
	cmd := state.OperationCommand{Kind: "pause", Target: "trace", IdempotencyKey: "key", Content: json.RawMessage(`{}`)}
	var wg sync.WaitGroup
	receipts := make(chan state.OperationReceipt, 12)
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := m.AcceptOperation(context.Background(), cmd)
			receipts <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(receipts)
	close(errs)
	var first state.OperationReceipt
	for r := range receipts {
		if first.OperationID == "" {
			first = r
		}
		if r != first {
			t.Fatal("duplicate operation")
		}
	}
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if m.View().LastSeq != 1 {
		t.Fatal("more than one accepted commit")
	}
}
