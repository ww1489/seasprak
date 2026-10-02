package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	store "github.com/ww1489/seasprak/internal/storage"
)

// Persist the real operation, then withhold its Append response. This isolates
// acceptance from delivery to Pause without depending on an 80ms scheduler gap.
type pauseAcceptanceStore struct {
	store.Store
	store.CheckpointBlobs
	accepted chan state.Operation
	release  <-chan struct{}
}

func (s *pauseAcceptanceStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, c store.Commit) (store.CommitReceipt, error) {
	receipt, err := s.Store.Append(ctx, id, expected, c)
	if err != nil {
		return receipt, err
	}
	for _, r := range c.ControlRecords {
		if r.Type != "operation" {
			continue
		}
		var op state.Operation
		if err := json.Unmarshal(r.Payload, &op); err != nil {
			return store.CommitReceipt{}, err
		}
		if op.Kind == "pause" && op.State == "accepted" {
			s.accepted <- op
			<-s.release
		}
	}
	return receipt, nil
}

// pauseWaitContext observes the direct Done call in Pause's post-receipt
// select. Calls from runtime.call, storage, and context helpers do not qualify.
// Unlike a Done-call counter, extra internal context checks cannot move this
// boundary. If Pause's function boundary changes, this test must be re-audited.
type pauseWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *pauseWaitContext) Done() <-chan struct{} {
	pc, _, _, ok := goruntime.Caller(1)
	if ok && goruntime.FuncForPC(pc).Name() == "github.com/ww1489/seasprak/internal/codeagent.(*AgentSession).Pause" {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}
func (c *pauseWaitContext) Err() error {
	if c.Context.Err() != nil {
		return context.Cause(c.Context)
	}
	return nil
}

func TestPauseCancellationAcceptanceBoundary(t *testing.T) {
	for _, phase := range []string{"before_call", "persisted_before_reply"} {
		t.Run(phase, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var backend *pauseAcceptanceStore
			s, manager, _, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
				backend = &pauseAcceptanceStore{Store: st, CheckpointBlobs: st.(store.CheckpointBlobs), accepted: make(chan state.Operation, 1), release: release}
				return backend
			})
			defer unblock() // Always unblock Append before activitySession cleanup.
			r := activitySubmit(t, s)
			modelCtx := activityStarted(t, model)
			frame := activityFrame(t, s)
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &pauseWaitContext{Context: base, waiting: make(chan struct{})}
			type result struct {
				receipt state.OperationReceipt
				err     error
			}
			returned := make(chan result, 1)
			if phase == "before_call" {
				cancel()
			}
			go func() {
				receipt, err := s.Pause(ctx, r.TraceID)
				returned <- result{receipt, err}
			}()
			var accepted state.Operation
			if phase == "persisted_before_reply" {
				select {
				case accepted = <-backend.accepted:
				case <-time.After(time.Second):
					t.Fatal("Pause did not reach durable acceptance")
				}
				select {
				case <-ctx.waiting:
					t.Fatal("stored acceptance was mistaken for receipt delivery")
				default:
				}
				cancel()
			}
			select {
			case got := <-returned:
				if !errors.Is(got.err, context.Canceled) || got.receipt != (state.OperationReceipt{}) {
					t.Fatalf("cancellation before reply: receipt=%+v err=%v", got.receipt, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("Pause did not return caller cancellation while Append response was blocked")
			}
			unblock()
			if err := s.rt.do(t.Context(), func(rt *runtime) error {
				if rt.active != frame || frame.ctx.Err() != nil {
					t.Error("caller cancellation changed active execution")
				}
				if phase == "persisted_before_reply" && frame.pauseID != accepted.Receipt.OperationID {
					t.Error("accepted pause was rolled back after caller cancellation")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			v := manager.View()
			if phase == "before_call" {
				if len(v.Operations) != 0 {
					t.Fatal("pre-cancelled request accepted an operation")
				}
			} else {
				status, err := manager.GetOperation(accepted.Receipt.OperationID)
				if err != nil || status.State != "accepted" || status.OperationReceipt != accepted.Receipt || len(v.Operations) != 1 {
					t.Fatalf("durable operation lost after cancellation: status=%+v err=%v", status, err)
				}
			}
			if modelCtx.Err() != nil || model.calls.Load() != 1 || v.Traces[r.TraceID].ExecutionStopped || len(v.Checkpoints) != 0 {
				t.Fatal("caller cancellation claimed execution exit or checkpoint")
			}
		})
	}
}

func TestPauseWaitCancellationAfterReceiptDelivery(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			s, manager, clock, model := activitySession(t, 3*time.Second, nil)
			var toolCalls atomic.Int32
			model.inner = testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}})
			if err := s.rt.do(t.Context(), func(rt *runtime) error {
				rt.opts.Tools = []tools.Definition{{Name: "work", Version: "1", Schema: []byte(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
					toolCalls.Add(1)
					return "unexpected", nil
				}}}
				rt.opts.ToolInfos = []*schema.ToolInfo{testkit.ToolInfo("work", "")}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r := activitySubmit(t, s)
			modelCtx := activityStarted(t, model)
			frame := activityFrame(t, s)
			base, expire := context.WithCancelCause(t.Context())
			defer expire(context.Canceled)
			ctx := &pauseWaitContext{Context: base, waiting: make(chan struct{})}
			type result struct {
				receipt state.OperationReceipt
				err     error
			}
			returned := make(chan result, 1)
			go func() {
				receipt, err := s.Pause(ctx, r.TraceID)
				returned <- result{receipt, err}
			}()
			awaitActivitySignal(t, ctx.waiting)
			// Evaluating this select occurs only after rt.call returned its
			// accepted receipt. The still-blocked model rules out the exit case.
			expire(cause)
			var accepted state.OperationReceipt
			select {
			case got := <-returned:
				if !errors.Is(got.err, cause) || got.receipt.OperationID == "" || got.receipt.State != "accepted" || got.receipt.Target != r.TraceID {
					t.Fatalf("post-receipt cancellation: receipt=%+v err=%v", got.receipt, got.err)
				}
				accepted = got.receipt
			case <-time.After(time.Second):
				t.Fatal("Pause did not return waiting cancellation")
			}
			status, err := manager.GetOperation(accepted.OperationID)
			v := manager.View()
			if err != nil || status.State != "accepted" || status.OperationReceipt != accepted || modelCtx.Err() != nil || model.calls.Load() != 1 || v.Traces[r.TraceID].ExecutionStopped || len(v.Checkpoints) != 0 || v.Traces[r.TraceID].Usage.TransportRequests != 1 {
				t.Fatalf("wait cancellation changed execution facts: status=%+v err=%v", status, err)
			}
			select {
			case <-frame.done:
				t.Fatal("wait cancellation pretended the model exited")
			default:
			}
			clock.advance(250 * time.Millisecond)
			model.release <- struct{}{}
			activityWait(t, frame)
			v = manager.View()
			tr := v.Traces[r.TraceID]
			status, err = manager.GetOperation(accepted.OperationID)
			if err != nil || status.State != "completed" || tr.State != "paused" || !tr.ExecutionStopped || tr.Activity.Reserved != 0 || tr.Activity.Settled != 250*time.Millisecond || tr.Usage.TransportRequests != 1 || model.calls.Load() != 1 || toolCalls.Load() != 0 || tr.Usage.ToolExecutions != 0 || len(v.Checkpoints) != 1 {
				t.Fatalf("actual exit did not complete accepted pause: trace=%+v status=%+v err=%v", tr, status, err)
			}
		})
	}
}
