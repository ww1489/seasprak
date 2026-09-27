package sessions

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type commandAtomicStore struct {
	store.Store
	fail     string
	rejected chan store.Commit
}

func (s *commandAtomicStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	reject := false
	for _, r := range c.ControlRecords {
		if s.fail == "accept" && r.Type == "input" {
			reject = true
		}
		if s.fail == "result" && r.Type == "operation" {
			var op state.Operation
			if err := json.Unmarshal(r.Payload, &op); err != nil {
				return store.CommitReceipt{}, err
			}
			if op.State == "completed" {
				reject = true
			}
		}
	}
	if s.fail == "lost_accept" || s.fail == "lost_result" {
		matched := false
		for _, r := range c.ControlRecords {
			if s.fail == "lost_accept" && r.Type == "input" {
				matched = true
			}
			if s.fail == "lost_result" && r.Type == "operation" {
				var op state.Operation
				if err := json.Unmarshal(r.Payload, &op); err != nil {
					return store.CommitReceipt{}, err
				}
				if op.State == "completed" {
					matched = true
				}
			}
		}
		if matched {
			if _, err := s.Store.Append(ctx, id, expected, c); err != nil {
				return store.CommitReceipt{}, err
			}
			s.rejected <- c
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected lost append acknowledgement")
		}
	}
	if reject {
		s.rejected <- c
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected direct command append failure")
	}
	return s.Store.Append(ctx, id, expected, c)
}

// Keep the journal available for deterministic reopen tests; this does not
// simulate durable storage across an operating-system restart.
func (s *commandAtomicStore) Close() error { return nil }

func newAtomicCommandSession(t *testing.T, fail string) (*AgentSession, *commandAtomicStore, *commandProcessProbe) {
	t.Helper()
	backend, err := memory.Open("atomic-command", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	faults := &commandAtomicStore{Store: backend, fail: fail, rejected: make(chan store.Commit, 1)}
	process := &commandProcessProbe{}
	// Unknown effects intentionally retain resource holds after Close. Isolate
	// fixtures while preserving this workspace and scheduler in reopen options.
	opts := Options{SessionID: "atomic-command", Workspace: t.TempDir(), ResourceScheduler: tools.NewResourceScheduler(), Store: faults, Profile: ProfileMemory, Model: testkit.NewFake(), Tools: []tools.Definition{builtinDefinitionForSession(t, "execute")}, Operations: tools.Operations{Process: process}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(faults, opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "command-generation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()); _ = backend.Close() })
	return s, faults, process
}
func atomicCommandRequest() CommandRequest {
	return CommandRequest{Name: "execute", Arguments: json.RawMessage(`{"argv":["echo","hi"],"cwd":"workspace"}`), IdempotencyKey: "once"}
}
func waitAtomicCommand(t *testing.T, s *AgentSession, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if terminal(snap.Operations[id].State) {
			return snap
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("command did not reach terminal state")
	return Snapshot{}
}
func TestExecuteCommandAtomicIdempotencyAndConsumption(t *testing.T) {
	s, _, process := newAtomicCommandSession(t, "")
	request := atomicCommandRequest()
	receipt, err := s.ExecuteCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	snap := waitAtomicCommand(t, s, receipt.OperationID)
	duplicate, err := s.ExecuteCommand(t.Context(), request)
	if err != nil || duplicate != receipt {
		t.Errorf("same request lost receipt: %+v %v", duplicate, err)
	}
	request.Arguments = json.RawMessage(`{"argv":["echo","different"],"cwd":"workspace"}`)
	_, err = s.ExecuteCommand(t.Context(), request)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIdempotencyConflict {
		t.Errorf("different content: %v", err)
	}
	for _, in := range snap.Inputs {
		if in.State != "consumed" {
			t.Errorf("command input left %s", in.State)
		}
	}
	if process.calls.Load() != 1 || len(snap.Operations) != 1 {
		t.Fatalf("calls=%d operations=%d", process.calls.Load(), len(snap.Operations))
	}
}
func TestExecuteCommandLostCommitAcknowledgementReopensWithoutExecution(t *testing.T) {
	for _, stage := range []string{"lost_accept", "lost_result"} {
		t.Run(stage, func(t *testing.T) {
			s, faults, process := newAtomicCommandSession(t, stage)
			_, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
			if stage == "lost_accept" {
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
					t.Fatalf("accept error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case <-faults.rejected:
			case <-time.After(3 * time.Second):
				t.Fatal("lost acknowledgement not reached")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(faults.Store, s.rt.opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			// Disable the one simulated crash after the old writer has stopped.
			faults.fail = ""
			opened, err := Start(s.rt.opts, manager, s.rt.generation)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			view := manager.View()
			if len(view.Operations) != 1 || len(view.Inputs) != 1 || len(view.Traces) != 1 {
				t.Fatalf("incomplete durable command: operations=%d inputs=%d traces=%d", len(view.Operations), len(view.Inputs), len(view.Traces))
			}
			var original state.Operation
			for _, op := range view.Operations {
				original = op
			}
			retry, err := opened.ExecuteCommand(t.Context(), atomicCommandRequest())
			if err != nil || retry != original.Receipt {
				t.Fatalf("retry did not return original receipt: %+v %v", retry, err)
			}
			expectedRuns := int32(0)
			for _, in := range view.Inputs {
				if stage == "lost_accept" {
					if in.State != "pending" || !view.Traces[in.TraceID].Hold {
						t.Fatalf("reopened queue not held: %+v", in)
					}
				} else {
					expectedRuns = 1
					if in.State != "consumed" || original.State != "completed" || original.ResultRef == "" {
						t.Fatalf("partial terminal: input=%+v operation=%+v", in, original)
					}
				}
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if process.calls.Load() != expectedRuns {
				t.Fatalf("reopen/retry executed command: %d", process.calls.Load())
			}
		})
	}
}

func TestExecuteCommandConcurrentRetriesExecuteOnce(t *testing.T) {
	s, faults, process := newAtomicCommandSession(t, "")
	var wg sync.WaitGroup
	receipts := make(chan state.OperationReceipt, 8)
	for range 8 {
		wg.Go(func() {
			receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
			if err != nil {
				t.Errorf("concurrent retry: %v", err)
				return
			}
			receipts <- receipt
		})
	}
	wg.Wait()
	close(receipts)
	var first state.OperationReceipt
	for receipt := range receipts {
		if first.OperationID == "" {
			first = receipt
		}
		if receipt != first {
			t.Fatalf("different retry receipts: %+v %+v", first, receipt)
		}
	}
	if first.OperationID == "" {
		t.Fatal("no accepted command")
	}
	snap := waitAtomicCommand(t, s, first.OperationID)
	if process.calls.Load() != 1 || len(snap.Operations) != 1 || len(snap.Inputs) != 1 {
		t.Fatalf("duplicate execution: calls=%d operations=%d inputs=%d", process.calls.Load(), len(snap.Operations), len(snap.Inputs))
	}
	stored, err := faults.Load(t.Context(), s.rt.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var atomicResults int
	for _, commit := range stored.Commits {
		for _, entry := range commit.Entries {
			var msg agent.AgentMessage
			if err := json.Unmarshal(entry.Payload, &msg); err != nil {
				t.Fatal(err)
			}
			if msg.Kind != agent.KindCommand {
				continue
			}
			var terminalOperation, consumedInput bool
			for _, control := range commit.ControlRecords {
				if control.Type == "operation" {
					var op state.Operation
					if err := json.Unmarshal(control.Payload, &op); err != nil {
						t.Fatal(err)
					}
					terminalOperation = op.State == "completed" && op.ResultRef == msg.ID
				}
				if control.Type == "input" {
					var in state.InputState
					if err := json.Unmarshal(control.Payload, &in); err != nil {
						t.Fatal(err)
					}
					consumedInput = in.State == "consumed" && in.ID == msg.Scope.InputID
				}
			}
			if !terminalOperation || !consumedInput {
				t.Fatal("command result lacks atomic operation/input terminal")
			}
			atomicResults++
		}
	}
	if atomicResults != 1 {
		t.Fatalf("result commits=%d", atomicResults)
	}
}

func TestExecuteCommandAtomicAppendFailures(t *testing.T) {
	for _, stage := range []string{"accept", "result"} {
		t.Run(stage, func(t *testing.T) {
			s, faults, process := newAtomicCommandSession(t, stage)
			receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
			if stage == "accept" {
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
					t.Fatalf("accept error: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case <-faults.rejected:
			case <-time.After(3 * time.Second):
				t.Fatal("append fault not reached")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			view := s.rt.manager.View()
			if stage == "accept" {
				if len(view.Operations) != 0 || len(view.Inputs) != 0 || len(view.Traces) != 0 || process.calls.Load() != 0 {
					t.Errorf("partial acceptance: operations=%d inputs=%d traces=%d runs=%d", len(view.Operations), len(view.Inputs), len(view.Traces), process.calls.Load())
				}
			} else {
				if terminal(view.Operations[receipt.OperationID].State) {
					t.Error("failed append exposed terminal operation")
				}
				for _, msg := range view.Messages {
					if msg.Kind == agent.KindCommand {
						t.Error("failed terminal append exposed command result")
					}
				}
				if process.calls.Load() != 1 {
					t.Errorf("runs=%d", process.calls.Load())
				}
			}
			// Rebuild through the normal startup path. Neither persisted effects nor
			// an incomplete command may be automatically dispatched again.
			manager, err := state.NewManager(faults.Store, s.rt.opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			opts := s.rt.opts
			opts.Store = faults
			opened, err := Start(opts, manager, s.rt.generation)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			if _, err := opened.Snapshot(t.Context()); err != nil {
				t.Fatal(err)
			}
			expected := int32(0)
			if stage == "result" {
				expected = 1
			}
			if process.calls.Load() != expected {
				t.Errorf("reopen ran command: %d", process.calls.Load())
			}
		})
	}
}
