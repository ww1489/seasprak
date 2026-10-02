package web

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestCatalogReservationCannotMaterializeForeignWorkspace(t *testing.T) {
	model := testkit.NewFake()
	root, served, foreign := t.TempDir(), t.TempDir(), t.TempDir()
	opts := codeagent.Options{Workspace: foreign, StateRoot: root, SessionID: "foreign-reservation", Profile: codeagent.ProfileMemory, Principal: localPrincipal, GenerationFingerprint: "reservation-binding-v1", Model: model}
	session, err := codeagent.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err = session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "sessions", opts.SessionID, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	opts.Workspace = served
	catalog, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close(t.Context())
	req := CatalogCreateRequest{Workspace: served, ModelRef: DefaultModelRef, IdempotencyKey: "reserved-foreign"}
	key := registryKey(localPrincipal, req.IdempotencyKey)
	reservation := storage.CreationRecord{SessionID: opts.SessionID, Digest: catalog.digest(req), Generation: opts.GenerationFingerprint}
	if err = catalog.registry.Save(t.Context(), key, reservation); err != nil {
		t.Fatal(err)
	}
	result, err := catalog.Create(t.Context(), req)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || result.Snapshot.SessionID != "" {
		t.Errorf("foreign reserved session became a served creation: err=%v sid=%q", err, result.Snapshot.SessionID)
	}
	if len(catalog.writers) != 0 {
		t.Error("foreign workspace acquired a served writer")
	}
	stored, found, loadErr := catalog.registry.Load(t.Context(), key)
	if loadErr != nil || !found || stored.SessionID != reservation.SessionID || len(stored.Receipt) != 0 {
		t.Error("foreign reservation was changed or published as an accepted receipt")
	}
	after, readErr := os.ReadFile(journal)
	if readErr != nil || !bytes.Equal(before, after) || model.Calls() != 0 {
		t.Error("foreign materialization changed its journal or invoked the model")
	}
}
