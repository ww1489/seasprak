package codeagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

func TestActivityExpiryPreservesFailureReasonAcrossReopen(t *testing.T) {
	s, manager, clock, model := activitySession(t, 3*time.Second, nil)
	input := activitySubmit(t, s)
	modelCtx := activityStarted(t, model)
	frame := activityFrame(t, s)
	clock.advance(time.Second)
	awaitActivitySignal(t, modelCtx.Done())
	if tr := manager.View().Traces[input.TraceID]; tr.ExecutionStopped || tr.State != "running" || model.calls.Load() != 1 {
		t.Fatal("expired reservation pretended the model exited")
	}
	model.release <- struct{}{}
	activityWait(t, frame)
	tr := manager.View().Traces[input.TraceID]
	if tr.State != "failed" || !tr.ExecutionStopped || !strings.Contains(tr.Error, product.CodeBudgetExhausted) || !strings.Contains(tr.Error, "activity reservation expired") || tr.Activity.Settled != time.Second || tr.Activity.Reserved != 0 || tr.Usage.TransportRequests != 1 || tr.Usage.ToolExecutions != 0 || model.calls.Load() != 1 {
		t.Fatalf("activity failure lost its reason or changed execution: %+v calls=%d", tr, model.calls.Load())
	}
	reopened, err := state.NewManager(s.rt.opts.Store, "activity")
	if err != nil {
		t.Fatal(err)
	}
	if saved := reopened.View().Traces[input.TraceID]; saved.Error != tr.Error || saved.State != "failed" || !saved.ExecutionStopped || model.calls.Load() != 1 {
		t.Fatal("reopen lost the failure reason or restarted execution")
	}
}

func TestExecutionFinalizationPreservesMixedErrorsAndSuppressesOnlyCancellation(t *testing.T) {
	budget := activityExhausted()
	for _, tc := range []struct {
		name string
		err  error
		keep bool
	}{
		{"cancel", context.Canceled, false},
		{"wrapped-cancel", fmt.Errorf("worker stopped: %w", context.Canceled), false},
		{"joined-cancel", errors.Join(context.Canceled, fmt.Errorf("worker stopped: %w", context.Canceled)), false},
		{"budget", budget, true},
		{"cancel-and-budget", errors.Join(context.Canceled, budget), true},
		{"wrapped-mixed", fmt.Errorf("worker failed: %w", errors.Join(context.Canceled, budget)), true},
		{"cancel-and-deadline", errors.Join(context.Canceled, context.DeadlineExceeded), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, err := memory.Open(tc.name, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			manager, err := state.NewManager(backend, tc.name)
			if err != nil {
				t.Fatal(err)
			}
			target := agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"}
			input, err := manager.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hello"}`)}, target)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.SetTraceState(t.Context(), input.TraceID, "running", false); err != nil {
				t.Fatal(err)
			}
			if err := manager.SetTraceState(t.Context(), input.TraceID, "cancelling", false); err != nil {
				t.Fatal(err)
			}
			tr := manager.View().Traces[input.TraceID]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			frame := &execution{scope: agent.ExecutionScope{SessionID: tc.name, TraceID: tr.ID, InvocationID: tr.InvocationID, ExecutionID: "segment", Generation: "gen"}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
			rt := &runtime{manager: manager, active: frame}
			rt.segmentFinished(frame, tc.err)
			got := manager.View().Traces[input.TraceID]
			want := ""
			if tc.keep {
				want = tc.err.Error()
			}
			if got.State != "cancelled" || !got.ExecutionStopped || !got.Settled || got.Error != want || got.Usage.ToolExecutions != 0 {
				t.Fatalf("finalization changed state or lost failure: %+v want_error=%q", got, want)
			}
		})
	}
}
