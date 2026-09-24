package jsonl_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/model"
	"github.com/ww1489/seasprak/session/history"
)

func TestAppendAndSecondWriter(t *testing.T) {
	root := t.TempDir()
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err == nil {
		t.Fatal("second writer must be rejected")
	}
	receipt, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.CommitSeq != 1 {
		t.Fatalf("seq = %d", receipt.CommitSeq)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	loaded, err := again.Load(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastSeq != 1 {
		t.Fatalf("reloaded seq = %d", loaded.LastSeq)
	}
}

func TestSyncFailureDoesNotAcknowledge(t *testing.T) {
	root := t.TempDir()
	calls := 0
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{SyncFile: func(f *os.File) error {
		calls++
		if calls > 1 {
			return os.ErrClosed
		}
		return f.Sync()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"})
	pe, ok := err.(*model.Error)
	if !ok || pe.Code != model.CodeStorageUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestTailAndMiddleDamage(t *testing.T) {
	root := t.TempDir()
	store, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), "s1", history.ExpectedCommit{}, history.Commit{CommitID: "c1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "s1", "journal.jsonl")
	store.Close()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"recordType\":\"commit\""); err != nil {
		t.Fatal(err)
	}
	f.Close()
	reopen, err := jsonl.Open("s1", root, history.Header{}, jsonl.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := reopen.Load(context.Background(), "s1")
	reopen.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.RepairRequired || loaded.LastSeq != 1 {
		t.Fatalf("tail repair = %+v", loaded)
	}
	if _, err := jsonl.Repair(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".orig"); err != nil {
		t.Fatal(err)
	}
}
