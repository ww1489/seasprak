package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

// Only the public SDK is imported. The legal header comes from an actual
// default Create; no private workspace binding or generation hash is invented.
type consumerCloseStore struct {
	mu       sync.Mutex
	stored   sdk.StoredSession
	appends  atomic.Int32
	closes   atomic.Int32
	faults   atomic.Int32
	failed   chan struct{}
	closeErr error
}

func (s *consumerCloseStore) Load(context.Context, string) (sdk.StoredSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stored, nil
}

func (s *consumerCloseStore) Append(_ context.Context, _ string, expected sdk.ExpectedCommit, commit sdk.Commit) (sdk.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appends.Add(1)
	if expected.ExpectedPreviousSeq != s.stored.LastSeq || commit.CommitSeq != s.stored.LastSeq+1 {
		return sdk.CommitReceipt{}, errors.New("consumer append sequence mismatch")
	}
	s.stored.Commits = append(s.stored.Commits, commit)
	s.stored.LastSeq = commit.CommitSeq
	receipt := sdk.CommitReceipt{CommitID: commit.CommitID, CommitSeq: commit.CommitSeq}
	for _, ev := range commit.Events {
		if ev.DurableSeq != nil {
			s.stored.DurableCursor = *ev.DurableSeq
			receipt.DurableSeq = append(receipt.DurableSeq, *ev.DurableSeq)
		}
	}
	if s.failed != nil {
		for _, rec := range commit.ControlRecords {
			var run struct {
				State string `json:"state"`
			}
			if rec.Type == "workflow_run" && json.Unmarshal(rec.Payload, &run) == nil && run.State == "completed" {
				s.faults.Add(1)
				close(s.failed)
				return sdk.CommitReceipt{}, errors.New("consumer-private-close-marker/lost-terminal-receipt")
			}
		}
	}
	return receipt, nil
}

type consumerCloseReader struct {
	commits []sdk.Commit
}

func (r *consumerCloseReader) Next() (sdk.Commit, error) {
	if len(r.commits) == 0 {
		return sdk.Commit{}, io.EOF
	}
	commit := r.commits[0]
	r.commits = r.commits[1:]
	return commit, nil
}

func (s *consumerCloseStore) ReadAfter(_ context.Context, _ string, after uint64) (sdk.CommitReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var commits []sdk.Commit
	for _, commit := range s.stored.Commits {
		if commit.CommitSeq > after {
			commits = append(commits, commit)
		}
	}
	return &consumerCloseReader{commits: commits}, nil
}

func (s *consumerCloseStore) Close() error {
	s.closes.Add(1)
	return s.closeErr
}

func consumerCloseFixture(t *testing.T, calls *atomic.Int32) (sdk.WorkflowOptions, *consumerCloseStore) {
	t.Helper()
	d := sdk.WorkflowDefinition{Name: "consumer-close", Version: "v1", Source: "consumer", FormatVersion: sdk.WorkflowFormatV1, Resumable: true,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Nodes:       []sdk.WorkflowNode{{ID: "s", Type: "start"}, {ID: "t", Type: "tool", Tool: "echo"}, {ID: "e", Type: "end", Inputs: map[string]sdk.WorkflowValue{"result": {Ref: &sdk.WorkflowRef{Node: "t", Field: "result"}}}}},
		Edges:       []sdk.WorkflowEdge{{From: "s", To: "t"}, {From: "t", To: "e"}}}
	opts := sdk.WorkflowOptions{Workspace: t.TempDir(), StateRoot: t.TempDir(), RunID: "consumer-close", Definition: d, Principal: "local", GenerationFingerprint: "consumer-close-v1",
		Tools: []sdk.ToolDefinition{{Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: sdk.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
			calls.Add(1)
			return "ok", nil
		}}}}
	seed, err := sdk.CreateWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(filepath.Join(opts.StateRoot, "workflow-runs", opts.RunID, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var header sdk.Header
	if err := json.NewDecoder(file).Decode(&header); err != nil {
		t.Fatal(err)
	}
	store := &consumerCloseStore{stored: sdk.StoredSession{Header: header}}
	opts.Store = store
	return opts, store
}

func consumerRequireSafeClose(t *testing.T, err error) {
	t.Helper()
	pe, ok := sdk.AsError(err)
	if !ok || pe.Code != sdk.CodeStorageUnavailable {
		t.Errorf("sdk.AsError direct=%t, want storage_unavailable", ok)
		return
	}
	if pe.Message != "workflow close failed" || pe.Details != nil || len(pe.Refs) != 0 || strings.Contains(err.Error(), "consumer-private-close-marker") {
		t.Error("public SDK Close exposed the private cause")
	}
}

func TestWorkflowConsumerCloseErrorBoundary(t *testing.T) {
	for _, fault := range []string{"terminal-receipt", "raw-backend", "product-backend"} {
		t.Run(fault, func(t *testing.T) {
			var calls atomic.Int32
			opts, store := consumerCloseFixture(t, &calls)
			store.closeErr = &os.PathError{Op: "close", Path: "consumer-private-close-marker/location", Err: os.ErrPermission}
			if fault == "terminal-receipt" {
				store.failed = make(chan struct{})
			} else if fault == "product-backend" {
				store.closeErr = sdk.NewError(sdk.CodeStorageUnavailable, "consumer-private-close-marker/backend-detail")
			}
			w, err := sdk.CreateWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close(context.Background()) })
			if store.appends.Load() != 1 || store.closes.Load() != 0 || calls.Load() != 0 {
				t.Fatal("injected consumer fixture was not initialized once without execution")
			}
			if fault == "terminal-receipt" {
				receipt, err := w.SubmitInput(t.Context(), sdk.WorkflowInputCommand{Input: json.RawMessage(`{}`), Principal: "local", IdempotencyKey: "input"})
				if err != nil || receipt.AcceptedCommit != 2 {
					t.Fatal("real input was not accepted before terminal receipt fault")
				}
				select {
				case <-store.failed:
				case <-time.After(5 * time.Second):
					t.Fatal("real terminal Append was not reached")
				}
				// Snapshot takes the owner's mutex after the final Append. Actual
				// segment exit occurs before execute releases that same mutex.
				snap, err := w.Snapshot(t.Context())
				if err != nil || snap.State != "running" || snap.ExecutionStopped || snap.Usage.ToolExecutions != 1 || calls.Load() != 1 || store.faults.Load() != 1 {
					t.Fatal("lost terminal receipt changed committed state or invocation counts")
				}
			}
			before := store.appends.Load()
			err = w.Close(t.Context())
			consumerRequireSafeClose(t, err)
			if again := w.Close(t.Context()); again != err || store.closes.Load() != 1 || store.appends.Load() != before {
				t.Error("SDK repeated Close changed error identity or backend counts")
			}
			t.Logf("append=%d terminalReceiptFault=%d backendClose=%d actualTool=%d", store.appends.Load(), store.faults.Load(), store.closes.Load(), calls.Load())
		})
	}
}

func TestWorkflowConsumerCloseErrorBoundaryActualExit(t *testing.T) {
	var calls atomic.Int32
	opts, store := consumerCloseFixture(t, &calls)
	entered, cancelled, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	opts.Tools[0].Run = func(ctx context.Context, _ json.RawMessage) (string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-gate
		return "", ctx.Err()
	}
	store.closeErr = errors.New("consumer-private-close-marker/backend")
	w, err := sdk.CreateWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close(context.Background()) })
	if _, err := w.SubmitInput(t.Context(), sdk.WorkflowInputCommand{Input: json.RawMessage(`{}`), Principal: "local"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer tool did not enter")
	}
	wait, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err = w.Close(wait)
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("consumer tool did not acknowledge cancellation")
	}
	snap, snapshotErr := w.Snapshot(t.Context())
	if err != context.DeadlineExceeded || snapshotErr != nil || snap.ExecutionStopped || snap.State != "pausing" || snap.Usage.ToolExecutions != 1 || store.closes.Load() != 0 || calls.Load() != 1 {
		t.Fatal("public Close deadline released backend before actual exit")
	}
	release()
	err = w.Close(t.Context())
	consumerRequireSafeClose(t, err)
	if again := w.Close(t.Context()); again != err || store.closes.Load() != 1 || calls.Load() != 1 {
		t.Error("public Close repeated backend Close or the non-cooperative tool")
	}
	t.Logf("deadline backendClose=0; after real release backendClose=%d actualTool=%d", store.closes.Load(), calls.Load())
}
