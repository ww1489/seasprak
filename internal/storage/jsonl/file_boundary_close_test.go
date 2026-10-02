package jsonl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestFileBoundaryStoreCloseFailureKeepsCodeAndReleasesWriter(t *testing.T) {
	root := t.TempDir()
	s, err := Open("close-boundary", root, store.Header{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.journal.Close(); err != nil {
		t.Fatal("close fault fixture:", err)
	}
	closeErr := s.Close()
	if pe, ok := product.AsError(closeErr); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Errorf("store close error must retain storage_unavailable, got %T", closeErr)
	}
	if !errors.Is(s.closeCause, os.ErrClosed) {
		t.Error("store discarded the internal journal close cause")
	}
	if s.journal != nil || s.lock != nil || !s.closed {
		t.Error("close failure retained owned journal or writer")
	}
	if err := s.Close(); err != nil {
		t.Fatal("repeat close:", err)
	}
	if _, err := s.Load(context.Background(), "close-boundary"); err == nil {
		t.Error("closed store remained readable")
	}
	competitor := flock.New(filepath.Join(root, "sessions", "close-boundary", "writer.lock"))
	locked, err := competitor.TryLock()
	if err != nil || !locked {
		t.Fatal("journal close failure retained writer lock")
	}
	if err := competitor.Unlock(); err != nil {
		t.Fatal("release competing writer:", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sessions", "close-boundary", "journal.jsonl")); err != nil {
		t.Fatal("close removed original journal:", err)
	}
}
