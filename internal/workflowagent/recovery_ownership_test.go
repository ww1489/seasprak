package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"time"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowAcceptedModelResultReusedAfterDiskReopen(t *testing.T) {
	var effects atomic.Int32
	m := testkit.NewFake(testkit.Step{Text: "original"}, testkit.Step{Text: "must-not-run"})
	opts := testOptions(t, modelThenTool(), m, &effects)
	w := newWorkflow(t, opts)
	// Cancel the segment after its successful assistant fact is durably
	// accepted, before the node completion can use the cancelled context.
	w.store = &modelAcceptedStopStore{Store: w.store, owner: w}
	submit(t, w)
	paused := waitStopped(t, w)
	if paused.State != "paused" || m.Calls() != 1 || effects.Load() != 0 {
		t.Fatalf("wrong accepted-response stop: %s model=%d tool=%d", paused.State, m.Calls(), effects.Load())
	}
	var nodeID string
	for _, n := range paused.WorkflowNodes {
		if n.Kind == "model" {
			nodeID = n.ID
		}
	}
	if err := w.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close(context.Background())
	if m.Calls() != 1 || effects.Load() != 0 {
		t.Fatal("Open executed the accepted result")
	}
	_, err = reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, reopened)
	if final.State != "completed" || m.Calls() != 1 || effects.Load() != 1 || final.Usage.LogicalModelCalls != 1 || final.Usage.TransportRequests != 1 {
		t.Fatalf("accepted result was not reused: state=%s code=%s model=%d tool=%d usage=%+v", final.State, final.ErrorCode, m.Calls(), effects.Load(), final.Usage)
	}
	if final.WorkflowNodes[nodeID].State != "completed" || final.WorkflowNodes[nodeID].Result != "original" {
		t.Fatal("recovery replaced the stable logical node")
	}
}

func TestWorkflowInterruptedModelResumeRetainsLogicalCallAndOccupancy(t *testing.T) {
	for _, limit := range []int{1, 3} {
		t.Run(map[int]string{1: "exhausted", 3: "available"}[limit], func(t *testing.T) {
			var effects atomic.Int32
			gate := make(chan struct{})
			defer close(gate)
			m := testkit.NewFake(testkit.Step{Text: "interrupted", Gate: gate}, testkit.Step{Text: "recovered"})
			opts := testOptions(t, modelThenTool(), m, &effects)
			opts.Limits = config.Limits{LogicalModelRequests: limit}
			w := newWorkflow(t, opts)
			submit(t, w)
			end := time.Now().Add(2 * time.Second)
			for m.Calls() != 1 && time.Now().Before(end) {
				time.Sleep(time.Millisecond)
			}
			if m.Calls() != 1 {
				t.Fatal("model was never physically invoked")
			}
			if err := w.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenWorkflowAgent(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			before, _ := reopened.Snapshot(t.Context())
			if before.State != "paused" || before.Usage.LogicalModelCalls != 1 || before.Usage.TransportRequests != 1 || effects.Load() != 0 {
				t.Fatalf("interrupted occupancy: %+v", before.Usage)
			}
			var nodeID string
			for _, n := range before.WorkflowNodes {
				if n.Kind == "model" {
					nodeID = n.ID
				}
			}
			_, err = reopened.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
			if err != nil {
				t.Fatal(err)
			}
			final := waitStopped(t, reopened)
			if final.Usage.LogicalModelCalls != 1 || final.WorkflowNodes[nodeID].Usage.ModelCallID != nodeID {
				t.Fatal("resume allocated/refunded logical call")
			}
			if limit == 1 {
				if final.State != "failed" || final.ErrorCode != product.CodeBudgetExhausted || final.Usage.TransportRequests != 1 || m.Calls() != 1 || effects.Load() != 0 {
					t.Fatalf("refund or early request: state=%s code=%s calls=%d/%d", final.State, final.ErrorCode, m.Calls(), effects.Load())
				}
			} else if final.State != "completed" || final.Usage.TransportRequests != 2 || final.WorkflowNodes[nodeID].Usage.ModelRequests != 2 || m.Calls() != 2 || effects.Load() != 1 {
				t.Fatalf("recovery state=%s code=%s calls=%d/%d usage=%+v", final.State, final.ErrorCode, m.Calls(), effects.Load(), final.Usage)
			}
		})
	}
}

func TestWorkflowCancelWaitingApprovalClosesUnclaimedFact(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	_, err := w.Cancel(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := w.Snapshot(t.Context())
	if s.State != "cancelled" || !s.ExecutionStopped || effects.Load() != 0 {
		t.Fatalf("approval cancel state=%s effects=%d", s.State, effects.Load())
	}
	for _, q := range s.Interactions {
		if q.State != "cancelled" {
			t.Errorf("terminal unanswered approval stayed %s", q.State)
		}
	}
	for _, obs := range s.Observations {
		if obs.Status != "skipped" || obs.SideEffect != "none" || obs.Executed {
			t.Errorf("no-start proof lost: %+v", obs)
		}
	}
}

type modelAcceptedStopStore struct {
	storage.Store
	owner *WorkflowAgent
}

func (s *modelAcceptedStopStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	receipt, err := s.Store.Append(ctx, id, expected, c)
	if err == nil {
		for _, r := range c.ControlRecords {
			if r.Type == "workflow_model_attempt" {
				var attempt attemptRecord
				json.Unmarshal(r.Payload, &attempt)
				if attempt.Status == "complete" {
					s.owner.active.cancel()
				}
			}
		}
	}
	return receipt, err
}

type mutateAppendStore struct{ storage.Store }

func (s *mutateAppendStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, c storage.Commit) (storage.CommitReceipt, error) {
	receipt, err := s.Store.Append(ctx, id, expected, c)
	for i := range c.Events {
		for j := range c.Events[i].Payload {
			if c.Events[i].Payload[j] == 'r' {
				c.Events[i].Payload[j] = 'R'
			}
		}
	}
	return receipt, err
}

func TestWorkflowStoreCannotMutateCommittedVisibleEvents(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	w.store = &mutateAppendStore{Store: w.store}
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var original []string
	for _, c := range loaded.Commits {
		for _, ev := range c.Events {
			original = append(original, string(ev.Payload))
		}
	}
	if len(original) != len(w.state.Events) {
		t.Fatal("event history changed")
	}
	for i, ev := range w.state.Events {
		if string(ev.Payload) != original[i] {
			t.Errorf("backend mutated visible event %d: %s != %s", i, ev.Payload, original[i])
		}
	}
	if effects.Load() != 1 {
		t.Fatal("ownership probe repeated effects")
	}
}
