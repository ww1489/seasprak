package codeagent_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2ReadOnlyOpenWhileWriterOwnsSession(t *testing.T) {
	opts := codeagent.Options{SessionID: "browse", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: codeagent.ProfileMemory, Model: testkit.NewFake()}
	writer, err := codeagent.CreateAgentSession(context.Background(), opts)
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
	reader, err := codeagent.OpenAgentSession(context.Background(), opts)
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
