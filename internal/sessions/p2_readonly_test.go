package sessions_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2ReadOnlyOpenWhileWriterOwnsSession(t *testing.T) {
	opts := sessions.Options{SessionID: "browse", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: testkit.NewFake()}
	writer, err := sessions.CreateAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, writer)
	path := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "journal.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	opts.Model = nil
	reader, err := sessions.OpenAgentSession(context.Background(), opts)
	if err != nil {
		t.Fatalf("read-only browse must not acquire writer lock: %v", err)
	}
	closeSession(t, reader)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only open modified journal")
	}
}
