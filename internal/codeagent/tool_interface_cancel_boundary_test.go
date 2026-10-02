package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
)

func TestToolInterfaceSessionCancelWhileResourceWaiting(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				scheduler := tools.NewResourceScheduler()
				workspace := t.TempDir()
				request := tools.ResourceRequest{Environment: "memory", Workspace: workspace, Effect: "write", Resources: []agent.ExecutionResource{{Identity: "shared"}}}
				lease, err := scheduler.Acquire(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				var runs atomic.Int32
				model := outputModel()
				def := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "write", Resources: request.Resources}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "unexpected", nil }}
				s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: workspace, StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}, ResourceScheduler: scheduler})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close(context.Background())
				receipt := submitOutput(t, s)
				// Wait is a synchronization barrier, not elapsed-time guessing. There
				// are no model/hook/backend gates in this fixture. Once all workers
				// are durably blocked after freezing, the pending tool can only be
				// waiting inside Acquire for the conflicting lease held above.
				synctest.Wait()
				before := s.rt.manager.View()
				trace := before.Traces[receipt.TraceID]
				if trace.State != "running" || trace.ExecutionStopped || trace.Settled || runs.Load() != 0 || model.Calls() != 1 || trace.Usage.ToolExecutions != 0 || len(before.FrozenExecutions) != 1 || len(before.Calls) != 1 {
					t.Fatalf("resource wait not reached: trace=%+v runs=%d model=%d frozen=%d calls=%d", trace, runs.Load(), model.Calls(), len(before.FrozenExecutions), len(before.Calls))
				}
				for _, call := range before.Calls {
					if call.Claimed || call.Observation != nil {
						t.Fatalf("resource waiter already claimed: %+v", call)
					}
				}
				frame := activityFrame(t, s)
				if err := s.Cancel(t.Context(), receipt.TraceID); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if !errors.Is(frame.ctx.Err(), context.Canceled) {
					t.Fatalf("execution error=%v", frame.ctx.Err())
				}
				// Acquire returns cancellation without an observation; the normal
				// interrupted-turn finalizer records this unclaimed call as skipped.
				assertSyncCancelledWithoutClaim(t, s, receipt.TraceID, runs.Load(), model.Calls(), "skipped")
				if frame.budget.Snapshot().ToolExecutions != 0 {
					t.Fatal("cancelled resource waiter consumed in-memory budget")
				}
				// Cancellation must remove the waiter, rather than dispatch it when
				// the resource becomes free later.
				lease.Release()
				synctest.Wait()
				probe, err := scheduler.Acquire(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				probe.Release()
				if runs.Load() != 0 {
					t.Fatal("resource release started a cancelled waiter")
				}
			})
		})
	}
}

func TestToolInterfaceSessionCancelAfterAuthorizationBeforeAtomicClaim(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			model := outputModel()
			def := tools.Definition{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "read"}, BeforeCall: []func(context.Context, agent.FrozenExecution) error{func(context.Context, agent.FrozenExecution) error { close(entered); <-release; return nil }}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "unexpected", nil }}
			s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: model, Tools: []tools.Definition{def}})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			// Keep unrelated activity renewal commands out of the controlled
			// mailbox sequence; no production timeout or budget is disabled.
			if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = newManualActivityClock(); return nil }); err != nil {
				t.Fatal(err)
			}
			receipt := submitOutput(t, s)
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("hook not reached")
			}
			frame := activityFrame(t, s)
			authorizationReady := make(chan struct{})
			barrierDone := make(chan error, 1)
			go func() {
				barrierDone <- s.rt.do(context.Background(), func(rt *runtime) error {
					// Run on the real mailbox owner. Hold the authorization response
					// until Cancel has entered the queue, then let the executor enqueue
					// its claim. Dispatch the actual closures in their FIFO order.
					close(release)
					authorize, err := nextSyncBoundaryCommand(rt)
					if err != nil {
						return err
					}
					allowed := commandResult{err: authorize.ctx.Err()}
					if allowed.err == nil {
						allowed.value, allowed.err = authorize.fn(rt)
					}
					if allowed.err != nil || allowed.value != agent.DecisionAllow {
						authorize.reply <- allowed
						return fmt.Errorf("real authorization did not allow: %+v", allowed)
					}
					close(authorizationReady)
					cancel, err := nextSyncBoundaryCommand(rt)
					authorize.reply <- allowed
					if err != nil {
						return err
					}
					cancelDispatched := false
					defer func() {
						if !cancelDispatched {
							dispatchSyncBoundaryCommand(rt, cancel)
						}
					}()
					claim, err := nextSyncBoundaryCommand(rt)
					if err != nil {
						return err
					}
					// CommitFact submits tool_intent with WithoutCancel. Cancel was
					// enqueued before Authorize returned, hence before this claim.
					if claim.ctx.Done() != nil {
						claim.reply <- commandResult{err: fmt.Errorf("unexpected cancellable claim command")}
						return fmt.Errorf("expected uncancellable intent envelope")
					}
					pending := rt.manager.View()
					if len(pending.Calls) != 1 || pending.Traces[receipt.TraceID].Usage.ToolExecutions != 0 {
						claim.reply <- commandResult{err: fmt.Errorf("claim was already visible")}
						return fmt.Errorf("pending claim changed durable budget")
					}
					for _, call := range pending.Calls {
						if call.Claimed || call.Observation != nil {
							claim.reply <- commandResult{err: fmt.Errorf("claim was already visible")}
							return fmt.Errorf("claim boundary was passed: %+v", call)
						}
					}
					cancelled := dispatchSyncBoundaryCommand(rt, cancel)
					cancelDispatched = true
					if cancelled.err != nil || cancelled.value != frame.done {
						claim.reply <- commandResult{err: fmt.Errorf("cancel failed")}
						return fmt.Errorf("real cancellation did not accept: %+v", cancelled)
					}
					trace := rt.manager.View().Traces[receipt.TraceID]
					if trace.State != "cancelling" || trace.ExecutionStopped || trace.Settled {
						claim.reply <- commandResult{err: fmt.Errorf("premature stop")}
						return fmt.Errorf("stopped before pending claim returned: %+v", trace)
					}
					rejected := dispatchSyncBoundaryCommand(rt, claim)
					if !errors.Is(rejected.err, context.Canceled) {
						return fmt.Errorf("claim error=%v want context.Canceled", rejected.err)
					}
					return nil
				})
			}()
			select {
			case <-authorizationReady:
			case err := <-barrierDone:
				t.Fatalf("claim boundary not reached: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("claim boundary timed out")
			}
			if runs.Load() != 0 {
				t.Fatal("backend ran before atomic claim")
			}
			if err := s.Cancel(t.Context(), receipt.TraceID); err != nil {
				t.Fatal(err)
			}
			if err := <-barrierDone; err != nil {
				t.Fatal(err)
			}
			assertSyncCancelledWithoutClaim(t, s, receipt.TraceID, runs.Load(), model.Calls(), "cancelled")
			if frame.budget.Snapshot().ToolExecutions != 0 {
				t.Fatal("rejected claim consumed in-memory budget")
			}
		})
	}
}

func nextSyncBoundaryCommand(rt *runtime) (command, error) {
	select {
	case cmd := <-rt.mailbox:
		return cmd, nil
	case <-time.After(10 * time.Second):
		return command{}, fmt.Errorf("expected mailbox command did not arrive")
	}
}

func dispatchSyncBoundaryCommand(rt *runtime, cmd command) commandResult {
	result := commandResult{err: cmd.ctx.Err()}
	if result.err == nil {
		result.value, result.err = cmd.fn(rt)
	}
	rt.publishCommitted()
	cmd.reply <- result
	return result
}

func assertSyncCancelledWithoutClaim(t *testing.T, s *AgentSession, traceID string, runs int32, modelCalls int, wantObservation string) {
	t.Helper()
	view := s.rt.manager.View()
	trace := view.Traces[traceID]
	if trace.State != "cancelled" || !trace.ExecutionStopped || !trace.Settled || trace.Usage.ToolExecutions != 0 || runs != 0 || modelCalls != 1 || len(view.Calls) != 1 || len(view.FrozenExecutions) != 1 {
		t.Fatalf("unsafe cancelled boundary: trace=%+v runs=%d model=%d calls=%d frozen=%d", trace, runs, modelCalls, len(view.Calls), len(view.FrozenExecutions))
	}
	if trace.Error != "" {
		t.Fatalf("ordinary cancellation became a trace failure: %q", trace.Error)
	}
	for _, call := range view.Calls {
		if call.Claimed || call.Observation == nil || call.Observation.Executed || call.Observation.SideEffect != "none" || call.Observation.Status != wantObservation {
			t.Fatalf("cancelled pre-claim result=%+v observation=%+v", call, call.Observation)
		}
	}
}
