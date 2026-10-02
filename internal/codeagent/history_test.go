package codeagent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestInspectSessionHeaderValuesAndHistoryQueryWriteNothing(t *testing.T) {
	opts := Options{Workspace: t.TempDir(), StateRoot: t.TempDir(), SessionID: "header-query", Model: testkit.NewFake(), Profile: ProfileMemory, GenerationFingerprint: "inspect-test-v1"}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = SubscribeHistoryFrom(t.Context(), s, 0, config.Limits{}); err == nil {
		t.Fatal("writable session accepted for history-only query")
	}
	if err = s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "journal.jsonl")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkspace, err := filepath.EvalSymlinks(opts.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := InspectSessionHeader(t.Context(), opts.StateRoot, opts.SessionID, config.Limits{})
	if err != nil || desc.SessionID != opts.SessionID || desc.Workspace != wantWorkspace || desc.Generation == "" {
		t.Fatalf("descriptor=%+v err=%v", desc, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = InspectSessionHeader(ctx, opts.StateRoot, opts.SessionID, config.Limits{}); err != context.Canceled {
		t.Fatalf("cancel=%v", err)
	}
	opts.ReadOnly = true
	s, err = OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	sub, err := SubscribeHistoryFrom(t.Context(), s, 0, config.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	n := 0
	for {
		select {
		case _, ok := <-sub.Events:
			if !ok {
				goto ended
			}
			n++
		case <-time.After(5 * time.Second):
			t.Fatal("history did not reach EOF")
		}
	}
ended:
	if n == 0 || sub.Err() != nil {
		t.Fatalf("history events=%d err=%v", n, sub.Err())
	}
	after, err := os.ReadFile(journal)
	if err != nil || !bytes.Equal(before, after) || opts.Model.(*testkit.FakeModel).Calls() != 0 {
		t.Fatal("query wrote journal or executed model")
	}
	if _, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt"}); err == nil {
		t.Fatal("read-only history port gained execution authority")
	}
}

func TestInspectSessionHeaderMissingCorruptCrossTypeAndBounds(t *testing.T) {
	root, sid := t.TempDir(), "inspect-one"
	_, err := InspectSessionHeader(t.Context(), root, sid, config.Limits{})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeNotFound {
		t.Fatalf("missing=%v", err)
	}
	dir := filepath.Join(root, "sessions", sid)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{nil, []byte("  \n"), []byte("broken\n"), []byte(`{"recordType":"header","formatVersion":1,"resourceType":"workflow","runId":"inspect-one","sessionId":"inspect-one","workspace":{}}`)} {
		if err = os.WriteFile(filepath.Join(dir, "journal.jsonl"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = InspectSessionHeader(t.Context(), root, sid, config.Limits{})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
			t.Fatalf("corrupt/cross-type=%v", err)
		}
	}
	if err = os.WriteFile(filepath.Join(dir, "journal.jsonl"), inspectHeaderBytes(t, sid), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = InspectSessionHeader(t.Context(), root, sid, config.Limits{MaxCommitLineBytes: 8})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleVersion {
		t.Fatalf("bounded=%v", err)
	}
}
