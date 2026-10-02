package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

type catalogFailReceiptRegistry struct {
	store.CreationRegistry
	fail bool
}

func (r *catalogFailReceiptRegistry) Save(ctx context.Context, key string, record store.CreationRecord) error {
	if r.fail && len(record.Receipt) != 0 {
		r.fail = false
		return product.NewError(product.CodeStorageUnavailable, "fixture receipt publication failed")
	}
	return r.CreationRegistry.Save(ctx, key, record)
}

func TestCatalogFailedReceiptPublicationReusesMaterializedSessionAndOriginalReceipt(t *testing.T) {
	opts, model := catalogOptions(t)
	c, err := NewCatalog(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	c.registry = &catalogFailReceiptRegistry{CreationRegistry: c.registry, fail: true}
	req := CatalogCreateRequest{Workspace: opts.Workspace, ModelRef: DefaultModelRef, IdempotencyKey: "lost-receipt"}
	_, err = c.Create(t.Context(), req)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatalf("receipt publication error=%v", err)
	}
	reserved, found, err := c.registry.Load(t.Context(), registryKey(opts.Principal, req.IdempotencyKey))
	if err != nil || !found || len(reserved.Receipt) != 0 || len(c.writers) != 1 {
		t.Fatal("failed publication lost reservation or materialized writer")
	}
	journal := filepath.Join(opts.StateRoot, "sessions", reserved.SessionID, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.Create(t.Context(), req)
	if err != nil || !created.Duplicate || created.Snapshot.SessionID != reserved.SessionID {
		t.Fatalf("retry allocated another session: %v", err)
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || model.Calls() != 0 {
		t.Fatal("retry changed session facts or executed")
	}
	entries, err := os.ReadDir(filepath.Join(opts.StateRoot, "sessions"))
	if err != nil || len(entries) != 1 {
		t.Fatal("retry created a second directory")
	}
	original, _ := json.Marshal(created.Snapshot)
	session, err := c.Writer(t.Context(), reserved.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"execute after creation"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	duplicate, err := c.Create(t.Context(), req)
	if err != nil || !duplicate.Duplicate {
		t.Fatal("original creation receipt unavailable after execution")
	}
	got, _ := json.Marshal(duplicate.Snapshot)
	if !bytes.Equal(original, got) {
		t.Fatal("execution changed immutable creation receipt")
	}
}
