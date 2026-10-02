package storage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestCreationRegistryDurableReservation(t *testing.T) {
	root := t.TempDir()
	r, err := OpenCreationRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := OpenCreationRegistry(root); err == nil {
		other.Close()
		t.Fatal("second writer allowed")
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("second writer error=%v", err)
	}
	record := CreationRecord{SessionID: "session-one", Digest: "digest-one", Generation: "generation-one"}
	if err = r.Save(t.Context(), "key-one", record); err != nil {
		t.Fatal(err)
	}
	record.Receipt = json.RawMessage(`{"sessionId":"session-one"}`)
	if err = r.Save(t.Context(), "key-one", record); err != nil {
		t.Fatal(err)
	}
	changed := record
	changed.Receipt = json.RawMessage(`{"sessionId":"changed"}`)
	if err = r.Save(t.Context(), "key-one", changed); err == nil {
		t.Fatal("original receipt was replaced")
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("immutable receipt error=%v", err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = OpenCreationRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, found, err := r.Load(t.Context(), "key-one")
	if err != nil || !found || got.SessionID != record.SessionID || string(got.Receipt) != string(record.Receipt) {
		t.Fatal("reservation or receipt lost")
	}
	got.Receipt[0] = 'x'
	again, _, err := r.Load(t.Context(), "key-one")
	if err != nil || string(again.Receipt) != string(record.Receipt) {
		t.Fatal("record aliases caller")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = r.Save(ctx, "cancelled", record); err != context.Canceled {
		t.Fatal("cancel ignored")
	}
	if _, found, err = r.Load(t.Context(), "cancelled"); err != nil || found {
		t.Fatal("cancelled write visible")
	}
}

func TestCreationRegistryRejectsLinksAndCorruption(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "catalog")); err != nil {
		t.Skip("symlink creation unavailable")
	}
	if r, err := OpenCreationRegistry(root); err == nil {
		r.Close()
		t.Fatal("linked catalog accepted")
	}
}

func TestCreationRegistryRejectsCorruption(t *testing.T) {
	root := t.TempDir()
	r, err := OpenCreationRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err = r.Save(t.Context(), "key", CreationRecord{SessionID: "id", Digest: "digest", Generation: "gen"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "catalog", "creations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			if err = os.WriteFile(filepath.Join(root, "catalog", "creations", e.Name()), []byte("broken"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, _, err = r.Load(t.Context(), "key")
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatal("corrupt registration was accepted")
	}
}
