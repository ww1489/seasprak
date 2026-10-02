package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
)

// Faults follow the real append/close, so a lost receipt is distinct from a
// rejected write. The wrapper never supplies an artificial runtime state.
type closeBoundaryStore struct {
	storage.Store
	appends  atomic.Int32
	closes   atomic.Int32
	faults   atomic.Int32
	after    func(storage.Commit) error
	closeErr error
}

func (s *closeBoundaryStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, commit storage.Commit) (storage.CommitReceipt, error) {
	s.appends.Add(1)
	receipt, err := s.Store.Append(ctx, id, expected, commit)
	if err == nil && s.after != nil {
		if err = s.after(commit); err != nil {
			s.faults.Add(1)
			return storage.CommitReceipt{}, err
		}
	}
	return receipt, err
}

func (s *closeBoundaryStore) Close() error {
	s.closes.Add(1)
	return errors.Join(s.Store.Close(), s.closeErr)
}

func requireSafeWorkflowClose(t *testing.T, err error) {
	t.Helper()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStorageUnavailable {
		t.Errorf("direct AsError=%t, want storage_unavailable", ok)
		return
	}
	if pe.Message != "workflow close failed" || pe.Details != nil || len(pe.Refs) != 0 || strings.Contains(err.Error(), "private-close-marker") {
		t.Error("Close exposed a private cause instead of its fixed public shape")
	}
}

func TestWorkflowCloseErrorBoundaryCommitReceiptAndBackend(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	base := injectStore(t, &opts)
	private := &os.PathError{Op: "close", Path: "private-close-marker/location", Err: os.ErrPermission}
	observed := &closeBoundaryStore{Store: base.Store, closeErr: private, after: func(commit storage.Commit) error {
		for _, rec := range commit.ControlRecords {
			var node nodeRecord
			if rec.Type == "workflow_node" && json.Unmarshal(rec.Payload, &node) == nil && node.Node.NodeID == "t" && node.Node.State == "completed" {
				return errors.New("private-close-marker/lost-node-receipt")
			}
		}
		return nil
	}}
	opts.Store = observed
	w := newWorkflow(t, opts)
	submit(t, w)
	waitExited(t, w)
	w.mu.Lock()
	broken, active := w.broken, w.active
	w.mu.Unlock()
	requireCode(t, broken, product.CodeStorageUnavailable)
	snap, err := w.Snapshot(t.Context())
	if err != nil || active != nil || snap.ExecutionStopped || snap.State == "completed" || calls.Load() != 1 || snap.Usage.ToolExecutions != 1 || observed.faults.Load() != 1 || observed.appends.Load() < 3 || observed.closes.Load() != 0 {
		t.Fatal("receipt fault did not follow one real tool and actual segment exit")
	}
	before := observed.appends.Load()
	err = w.Close(t.Context())
	requireSafeWorkflowClose(t, err)
	if again := w.Close(t.Context()); again != err || observed.closes.Load() != 1 || observed.appends.Load() != before || calls.Load() != 1 {
		t.Error("repeated Close changed its result, repeated backend Close or executed work")
	}
	if !errors.Is(w.closeCause, private) || !errors.Is(w.closeCause, broken) {
		t.Error("private Close cause lost the backend or broken commit failure")
	}
	t.Logf("actual tool=%d receiptFault=%d append=%d backendClose=%d", calls.Load(), observed.faults.Load(), observed.appends.Load(), observed.closes.Load())
}

func TestWorkflowCloseErrorBoundaryBackendOnly(t *testing.T) {
	for _, kind := range []string{"raw", "product", "cancel", "deadline", "cancel-priority", "success"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			opts := testOptions(t, toolOnly(), nil, &calls)
			base := injectStore(t, &opts)
			private := &os.PathError{Op: "close", Path: "private-close-marker/location", Err: os.ErrPermission}
			var cause error
			switch kind {
			case "raw":
				cause = private
			case "product":
				cause = product.NewError(product.CodeStorageUnavailable, "private-close-marker/product-detail")
			case "cancel":
				cause = errors.Join(private, context.Canceled)
			case "deadline":
				cause = errors.Join(private, context.DeadlineExceeded)
			case "cancel-priority":
				cause = errors.Join(context.DeadlineExceeded, private, context.Canceled)
			}
			observed := &closeBoundaryStore{Store: base.Store, closeErr: cause}
			opts.Store = observed
			w := newWorkflow(t, opts)
			if w.broken != nil || observed.appends.Load() != 1 {
				t.Fatal("backend-only fixture already broken or did not initialize")
			}
			err := w.Close(t.Context())
			switch kind {
			case "raw", "product":
				requireSafeWorkflowClose(t, err)
			case "cancel", "cancel-priority":
				if err != context.Canceled {
					t.Error("Close lost direct context cancellation or its priority")
				}
			case "deadline":
				if err != context.DeadlineExceeded {
					t.Error("Close lost direct context deadline")
				}
			case "success":
				if err != nil {
					t.Error("successful Close returned an error")
				}
			}
			if cause == nil && w.closeCause != nil || cause != nil && !errors.Is(w.closeCause, cause) {
				t.Error("private Close cause did not preserve the entire backend failure")
			}
			if again := w.Close(t.Context()); again != err || observed.closes.Load() != 1 || observed.appends.Load() != 1 || calls.Load() != 0 {
				t.Error("Close result/ownership is not stable")
			}
			t.Logf("backendClose=%d append=%d tool=%d broken=false", observed.closes.Load(), observed.appends.Load(), calls.Load())
		})
	}
}

func TestWorkflowCloseErrorBoundaryWaitsForActualExit(t *testing.T) {
	var calls atomic.Int32
	entered, cancelled, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	opts := testOptions(t, toolOnly(), nil, &calls)
	opts.Tools[0].Run = func(ctx context.Context, _ json.RawMessage) (string, error) {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-gate
		return "", ctx.Err()
	}
	base := injectStore(t, &opts)
	observed := &closeBoundaryStore{Store: base.Store, closeErr: errors.New("private-close-marker/backend"), after: func(commit storage.Commit) error {
		for _, rec := range commit.ControlRecords {
			var run runRecord
			if rec.Type == "workflow_run" && json.Unmarshal(rec.Payload, &run) == nil && run.State == "pausing" {
				return errors.New("private-close-marker/stop-commit-receipt")
			}
		}
		return nil
	}}
	opts.Store = observed
	w := newWorkflow(t, opts)
	submit(t, w)
	migrationWaitSignal(t, entered, "tool entry")
	w.mu.Lock()
	frame := w.active
	w.mu.Unlock()
	wait, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err := w.Close(wait)
	cancel()
	migrationWaitSignal(t, cancelled, "cancellation acknowledgement")
	if err != context.DeadlineExceeded || observed.closes.Load() != 0 || calls.Load() != 1 {
		t.Fatal("wait deadline displaced actual execution ownership")
	}
	select {
	case <-frame.done:
		t.Fatal("blocked non-cooperative tool already reported exit")
	default:
	}
	w.mu.Lock()
	stopErr := w.closeCause
	w.mu.Unlock()
	requireCode(t, stopErr, product.CodeStorageUnavailable)
	if observed.faults.Load() != 1 {
		t.Fatal("Close did not reach the real stop commit before cancellation")
	}
	release()
	err = w.Close(t.Context())
	requireSafeWorkflowClose(t, err)
	if !errors.Is(w.closeCause, stopErr) || !errors.Is(w.closeCause, observed.closeErr) || !errors.Is(w.closeCause, w.broken) {
		t.Error("private Close cause lost stop commit, backend Close or broken failure")
	}
	select {
	case <-frame.done:
	default:
		t.Error("backend closed before the actual segment exit")
	}
	if again := w.Close(t.Context()); again != err || observed.closes.Load() != 1 || calls.Load() != 1 {
		t.Error("actual exit repeated Close or the tool")
	}
	t.Logf("deadline backendClose=0; after release backendClose=%d tool=%d", observed.closes.Load(), calls.Load())
}
