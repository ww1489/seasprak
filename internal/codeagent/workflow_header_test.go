package codeagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
)

func TestCodeOpenRejectsWorkflowHeaderWithoutWriting(t *testing.T) {
	root := agentRoots(t)
	id := "cross-type"
	dir, err := store.PrepareSessionDir(filepath.Join(root, "state"), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(store.Header{RecordType: "header", FormatVersion: 1, ResourceType: store.ResourceWorkflow, SessionID: id, RunID: id, Workspace: workspaceJSON(filepath.Join(root, "ws"), "")})
	raw = append(raw, '\n')
	path := filepath.Join(dir, "journal.jsonl")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenAgentSession(t.Context(), Options{Workspace: t.TempDir(), StateRoot: filepath.Join(root, "state"), SessionID: id, ReadOnly: true})
	if s != nil {
		s.Close(context.Background())
	}
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("cross-type code open %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("rejected open changed journal")
	}
}
