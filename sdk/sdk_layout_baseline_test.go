package sdk_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/testkit"
	"github.com/ww1489/seasprak/sdk"
)

// SDK export list for the single sdk.go entry. These names must stay declarable
// without importing internal. Nested Eino message and tool-info types stay Eino types.
//
// Session: CreateAgentSession, OpenAgentSession, SessionOptions, AgentSession,
// Snapshot, Subscription, ProfileDefault, ProfileMemory.
// Snapshot fields: TraceState, InputState, AgentMessage, TurnRecord, ToolRecord.
// Input: InputCommand, InputReceipt, TargetAgent.
// Errors: Error, NewError, Errorf, AsError, and every Code* constant.
// Limits: Limits, DefaultLimits, WithDefaults.
// Model: Model, ModelConfig, EffectiveOptions.
// Tools and execution: ToolDefinition, NewExecutor, Executor, Outcome,
// NewBudget, BudgetLedger, Usage, NewAgent, AgentDeps, ExecutionScope, Fact,
// FrozenCall, DecisionAllow, DecisionDeny, DecisionCancel, DecisionAsk,
// ExecutionSink, ToolAuthorizer, BoundaryController, TurnPlan, TurnFact.
// Store: SessionStore, Commit, ExpectedCommit, CommitReceipt, Header,
// StoredSession, CommitReader, Record, BranchUpdate.
const layoutSessionID = "sess-golden"

type countingStore struct {
	mu      sync.Mutex
	commits []sdk.Commit
	appends int
}

func (s *countingStore) Load(context.Context, string) (sdk.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sdk.StoredSession{Commits: append([]sdk.Commit(nil), s.commits...)}, nil
}

func (s *countingStore) Append(_ context.Context, _ string, _ sdk.ExpectedCommit, commit sdk.Commit) (sdk.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends++
	s.commits = append(s.commits, commit)
	return sdk.CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq}, nil
}

func (s *countingStore) ReadAfter(_ context.Context, _ string, after uint64) (sdk.CommitReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []sdk.Commit
	for _, c := range s.commits {
		if c.CommitSeq > after {
			items = append(items, c)
		}
	}
	return &commitSlice{items: items}, nil
}

func (s *countingStore) Close() error { return nil }

type commitSlice struct {
	items []sdk.Commit
	i     int
}

func (r *commitSlice) Next() (sdk.Commit, error) {
	if r.i >= len(r.items) {
		return sdk.Commit{}, io.EOF
	}
	item := r.items[r.i]
	r.i++
	return item, nil
}

func TestLayoutInjectedStoreAndTypedError(t *testing.T) {
	_, err := sdk.CreateAgentSession(context.Background(), sdk.SessionOptions{Profile: sdk.ProfileMemory})
	pe, ok := sdk.AsError(err)
	if !ok || pe.Code != sdk.CodeInvalidArgument {
		t.Fatalf("missing workspace error = %#v", err)
	}

	ws := t.TempDir()
	store := &countingStore{}
	fake := testkit.NewFake(testkit.Step{Text: "stored"})
	created, err := sdk.CreateAgentSession(context.Background(), sdk.SessionOptions{
		Workspace: ws, StateRoot: "memory", Profile: sdk.ProfileMemory,
		SessionID: layoutSessionID, Model: fake, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := created.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"injected"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitLayout(t, created, receipt.TraceID, "completed")
	if err := created.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if store.appends == 0 {
		t.Fatal("injected store received no commits")
	}
	if fake.Calls() == 0 {
		t.Fatal("model was not called")
	}
}

func TestLayoutGoldenJournalKeepsCommittedBytes(t *testing.T) {
	fixture := filepath.Join("testdata", "layout")
	if os.Getenv("SEASPRAK_CAPTURE_LAYOUT") == "1" {
		captureLayoutFixture(t, fixture)
		return
	}
	oldRoot, err := os.ReadFile(filepath.Join(fixture, "workspace.txt"))
	if err != nil {
		t.Fatalf("layout fixture missing (%v); capture it before moving packages", err)
	}
	ws := t.TempDir()
	state := t.TempDir()
	if err := copyTree(filepath.Join(fixture, "session"), filepath.Join(state, "sessions", layoutSessionID)); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(state, "sessions", layoutSessionID, "journal.jsonl")
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	line, rest, found := bytes.Cut(raw, []byte("\n"))
	if !found {
		t.Fatal("journal header is incomplete")
	}
	encodedOld, err := json.Marshal(strings.TrimSpace(string(oldRoot)))
	if err != nil {
		t.Fatal(err)
	}
	encodedNew, err := json.Marshal(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, encodedOld) {
		t.Fatal("captured workspace binding is not in the journal header")
	}
	if bytes.Contains(rest, encodedOld) {
		t.Fatal("workspace binding was stored outside the header")
	}
	patched := append(bytes.Replace(line, encodedOld, encodedNew, 1), '\n')
	patched = append(patched, rest...)
	if err := os.WriteFile(journal, patched, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := testkit.NewFake(testkit.Step{Text: "should-not-run"})
	opened, err := sdk.OpenAgentSession(context.Background(), sdk.SessionOptions{
		Workspace: ws, StateRoot: state, SessionID: layoutSessionID, Model: fake, Profile: sdk.ProfileMemory,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	snap, err := opened.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snap.RepairRequired || len(snap.Messages) == 0 || snap.Traces == nil {
		t.Fatalf("fixture snapshot = %+v", snap)
	}
	rawSnap, err := json.Marshal(snap.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rawSnap, []byte("layout-hello")) {
		t.Fatal("committed message was not restored from the fixture")
	}
	time.Sleep(100 * time.Millisecond)
	if fake.Calls() != 0 {
		t.Fatal("opening the fixture executed the model")
	}
}

func captureLayoutFixture(t *testing.T, fixture string) {
	t.Helper()
	ws := t.TempDir()
	state := t.TempDir()
	fake := testkit.NewFake(testkit.Step{Text: "captured"})
	created, err := sdk.CreateAgentSession(context.Background(), sdk.SessionOptions{
		Workspace: ws, StateRoot: state, SessionID: layoutSessionID, Profile: sdk.ProfileMemory, Model: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	receipt, err := created.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"layout-hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitLayout(t, created, receipt.TraceID, "completed")
	if err := created.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(fixture, "session")
	_ = os.RemoveAll(fixture)
	if err := copyTree(filepath.Join(state, "sessions", layoutSessionID), dest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "workspace.txt"), []byte(ws), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitLayout(t *testing.T, s *sdk.AgentSession, traceID, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if trace := snap.Traces[traceID]; trace != nil && trace.Settled {
			if trace.State != want {
				t.Fatalf("trace settled as %s, want %s; error=%s", trace.State, want, trace.Error)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("trace %s did not reach %s", traceID, want)
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return os.ErrInvalid
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}
