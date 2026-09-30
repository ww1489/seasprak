package sessions

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/testkit"
)

func catalogOptions(t *testing.T) (Options, *testkit.FakeModel) {
	t.Helper()
	model := testkit.NewFake()
	return Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), Principal: "local", Profile: ProfileMemory, Model: model, GenerationFingerprint: "catalog-test-v1"}, model
}
func TestCatalogCreateReplayAndReadOnly(t *testing.T) {
	opts, model := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	request := CatalogCreateRequest{IdempotencyKey: "same", Workspace: opts.Workspace, ModelRef: "default"}
	first, err := c.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Create(t.Context(), request)
	if err != nil || !second.Duplicate || second.Snapshot.SessionID != first.Snapshot.SessionID {
		t.Fatal("duplicate created another session")
	}
	request.ModelRef = "other"
	_, err = c.Create(t.Context(), request)
	if e, ok := product.AsError(err); !ok || e.Code != product.CodeIdempotencyConflict {
		t.Fatal("same key changed request accepted")
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	request.ModelRef = "default"
	reopened, err := c.Create(t.Context(), request)
	if err != nil || !reopened.Duplicate {
		t.Fatal("durable creation was lost")
	}
	want, _ := json.Marshal(first.Snapshot)
	got, _ := json.Marshal(reopened.Snapshot)
	if string(want) != string(got) {
		t.Fatal("original receipt changed")
	}
	journal := filepath.Join(opts.StateRoot, "sessions", first.Snapshot.SessionID, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(t.Context(), first.Snapshot.SessionID)
	if err != nil || snap.SessionID != first.Snapshot.SessionID {
		t.Fatal("snapshot failed")
	}
	list, err := c.List(t.Context(), CatalogListRequest{})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].SessionID != snap.SessionID {
		t.Fatal("list failed")
	}
	after, err := os.ReadFile(journal)
	if err != nil || string(before) != string(after) || model.Calls() != 0 {
		t.Fatal("read-only browsing wrote or executed")
	}
}
func TestCatalogReusesReservationAfterCrash(t *testing.T) {
	opts, model := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	req := CatalogCreateRequest{IdempotencyKey: "crash", Workspace: opts.Workspace, ModelRef: DefaultModelRef}
	pending := sessstore.CreationRecord{SessionID: "reserved01", Digest: c.digest(req), Generation: opts.GenerationFingerprint}
	if err = c.registry.Save(t.Context(), registryKey(opts.Principal, req.IdempotencyKey), pending); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err = NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	got, err := c.Create(t.Context(), req)
	if err != nil || got.Snapshot.SessionID != "reserved01" || !got.Duplicate || model.Calls() != 0 {
		t.Fatalf("pending reservation not reused: %v %+v", err, got.Snapshot.SessionID)
	}
	list, err := c.List(t.Context(), CatalogListRequest{})
	if err != nil || len(list.Sessions) != 1 {
		t.Fatal("reservation created a second session")
	}
}
func TestCatalogConcurrentCreate(t *testing.T) {
	opts, model := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	var wg sync.WaitGroup
	ids := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "one", Workspace: opts.Workspace, ModelRef: "default"})
			if e != nil {
				t.Error("concurrent creation failed")
				return
			}
			ids <- r.Snapshot.SessionID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	n := 0
	for got := range ids {
		n++
		if id != "" && got != id {
			t.Error("duplicate sid")
		}
		id = got
	}
	if n != 8 || model.Calls() != 0 {
		t.Fatal("creation counts mismatch")
	}
}
func TestCatalogRejectsForeignWorkspaceAndClosedAccess(t *testing.T) {
	opts, _ := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Create(t.Context(), CatalogCreateRequest{IdempotencyKey: "x", Workspace: t.TempDir(), ModelRef: "default"})
	if e, ok := product.AsError(err); !ok || e.Code != product.CodePermissionDenied {
		t.Fatal("foreign workspace accepted")
	}
	for _, id := range []string{"../escape", "missing"} {
		if _, err = c.Snapshot(t.Context(), id); err == nil {
			t.Fatal("invalid session read")
		}
	}
	if err = c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(t.Context(), CatalogListRequest{}); err == nil {
		t.Fatal("closed catalog readable")
	}
}
