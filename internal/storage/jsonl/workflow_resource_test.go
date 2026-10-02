package jsonl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestWorkflowResourceNamespaceHeaderAndWriter(t *testing.T) {
	root := t.TempDir()
	id := "same-id"
	code, err := Open(id, root, store.Header{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer code.Close()
	w, err := Open(id, root, store.Header{ResourceType: store.ResourceWorkflow, RunID: id}, Options{ResourceType: store.ResourceWorkflow})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := w.Load(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Header.SessionID != "" || loaded.Header.RunID != id || loaded.Header.ResourceType != store.ResourceWorkflow {
		t.Fatalf("header %+v", loaded.Header)
	}
	_, err = Open(id, root, store.Header{}, Options{ResourceType: store.ResourceWorkflow, OpenExisting: true})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("writer error %v", err)
	}
	ev := agent.Event{SchemaVersion: 1, Type: "workflow.state_changed", EventID: agent.MustID(), Scope: agent.EventScope{WorkflowRunID: id}, Payload: json.RawMessage(`{}`)}
	_, err = w.Append(context.Background(), id, store.ExpectedCommit{}, store.Commit{CommitID: agent.MustID(), Events: []agent.Event{ev}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workflow-runs", id, "journal.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := Open(id, root, store.Header{}, Options{ResourceType: store.ResourceWorkflow, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ro.Load(t.Context(), id); err != nil || got.LastSeq != 1 {
		t.Fatalf("reload %+v %v", got, err)
	}
	ro.Close()
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("readonly modified bytes")
	}
}

func TestWorkflowResourceRejectsCodeHeaderWithoutRepair(t *testing.T) {
	root := t.TempDir()
	id := "wrong"
	dir, err := store.PrepareResourceDir(root, store.ResourceWorkflow, id)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"recordType":"header","formatVersion":1,"sessionId":"wrong","workspace":{}}` + "\n")
	path := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{false, true} {
		_, err := Open(id, root, store.Header{}, Options{ResourceType: store.ResourceWorkflow, OpenExisting: true, ReadOnly: ro})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
			t.Fatalf("ro=%v error %v", ro, err)
		}
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("rejected open changed journal")
	}
	if _, err := os.Stat(filepath.Join(dir, "writer.lock")); !os.IsNotExist(err) {
		t.Fatal("rejected header acquired writer")
	}
}

func TestWorkflowResourceRejectsArbitraryNamespaceAndUnsafeID(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		kind store.ResourceType
		id   string
	}{{store.ResourceType("../escape"), "id"}, {store.ResourceWorkflow, "CON"}, {store.ResourceWorkflow, "../escape"}} {
		_, err := Open(tc.id, root, store.Header{}, Options{ResourceType: tc.kind})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeInvalidArgument {
			t.Fatalf("namespace %q id %q: %v", tc.kind, tc.id, err)
		}
	}
}
