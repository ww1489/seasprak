package memory_test

import (
	"bytes"
	"context"
	"errors"
	product "github.com/ww1489/seasprak/internal/errors"
	"io"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	contract "github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

func TestCursorSurvivesCommitWithoutEvents(t *testing.T) {
	store := openMem(t)
	if _, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{}, commitWithEvent("c1", "e1", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{ExpectedPreviousSeq: 1}, contract.Commit{CommitID: "c2", ExpectedPreviousSeq: 1}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DurableCursor != 1 {
		t.Fatalf("cursor = %d, want 1 after a commit with no events", loaded.DurableCursor)
	}
	receipt, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{ExpectedPreviousSeq: 2}, commitWithEvent("c3", "e2", 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.DurableSeq) != 1 || receipt.DurableSeq[0] != 2 {
		t.Fatalf("next durable seq = %v, want [2]", receipt.DurableSeq)
	}
}

func TestReadAfterSkipsEarlierCommits(t *testing.T) {
	store := openMem(t)
	appendSeq(t, store, "c1", 0)
	appendSeq(t, store, "c2", 1)
	reader, err := store.ReadAfter(context.Background(), "s1", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitID != "c2" {
		t.Fatalf("first commit after 1 = %s", got.CommitID)
	}
	_, err = reader.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("end = %v, want io.EOF", err)
	}
}

func TestLoadCopiesDoNotAliasStore(t *testing.T) {
	store := openMem(t)
	if _, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{}, contract.Commit{
		CommitID: "c1",
		Entries:  []contract.Record{{Type: "message", Version: 1, ID: "m1", Payload: []byte(`{"text":"a"}`)}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Commits[0].Entries[0].Payload[0] = 'X'
	again, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(again.Commits[0].Entries[0].Payload, []byte("X")) {
		t.Fatalf("store payload aliased caller copy: %s", again.Commits[0].Entries[0].Payload)
	}
}

func TestSameKeyDifferentDigestConflicts(t *testing.T) {
	store := openMem(t)
	key := contract.ExpectedCommit{IdempotencyKey: "k", ContentDigest: []byte("one")}
	if _, err := store.Append(context.Background(), "s1", key, contract.Commit{CommitID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{IdempotencyKey: "k", ContentDigest: []byte("two")}, contract.Commit{CommitID: "c2"})
	pe, ok := err.(*product.Error)
	if !ok || pe.Code != product.CodeIdempotencyConflict {
		t.Fatalf("err = %v, want idempotency_conflict", err)
	}
	loaded, err := store.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 1 {
		t.Fatalf("conflicting retry wrote seq %d", loaded.LastSeq)
	}
	if loaded.Commits[0].IdempotencyKey != "k" || !bytes.Equal(loaded.Commits[0].ContentDigest, []byte("one")) {
		t.Fatalf("commit did not keep idempotency fields: %+v", loaded.Commits[0])
	}
}

func TestLoadRejectsOtherSessionAndCanceledContext(t *testing.T) {
	store := openMem(t)
	_, err := store.Load(context.Background(), "other")
	pe, ok := err.(*product.Error)
	if !ok || pe.Code != product.CodeNotFound {
		t.Fatalf("load other = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = store.Load(ctx, "s1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("load canceled = %v", err)
	}
	_, err = store.Append(ctx, "s1", contract.ExpectedCommit{}, contract.Commit{CommitID: "c1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("append canceled = %v", err)
	}
	_, err = store.ReadAfter(ctx, "s1", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read canceled = %v", err)
	}
}

func TestClosedStoreRejectsUse(t *testing.T) {
	store := openMem(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(context.Background(), "s1")
	pe, ok := err.(*product.Error)
	if !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("load after close = %v", err)
	}
}

func TestRejectsSessionIDOutsideOneSegment(t *testing.T) {
	for _, id := range []string{"../x", `a\b`, "a/b", `C:s`, `/abs`, `\\srv\s`} {
		_, err := memory.Open(id, contract.Header{})
		if err == nil {
			t.Fatalf("session id %q was accepted", id)
		}
	}
}

func openMem(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memory.Open("s1", contract.Header{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func appendSeq(t *testing.T, store *memory.Store, id string, prev uint64) {
	t.Helper()
	_, err := store.Append(context.Background(), "s1", contract.ExpectedCommit{ExpectedPreviousSeq: prev}, contract.Commit{CommitID: id, ExpectedPreviousSeq: prev})
	if err != nil {
		t.Fatal(err)
	}
}

func commitWithEvent(commitID, eventID string, prev uint64) contract.Commit {
	return contract.Commit{
		CommitID:            commitID,
		ExpectedPreviousSeq: prev,
		Events: []agent.Event{{
			SchemaVersion: 1,
			Type:          "message.finalized",
			Scope:         agent.EventScope{SessionID: "s1"},
			EventID:       eventID,
			Payload:       []byte(`{}`),
		}},
	}
}
