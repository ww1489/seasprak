package sessions

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestCancelPausedApprovalRejectsLateAnswerAndResume(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	response := answerApproval(t, f, "allowed-once")
	if err := f.s.Cancel(t.Context(), f.input.TraceID); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	response.IdempotencyKey, response.ExpectedRevision = "late-answer", before.LastSeq
	_, err := f.s.RespondInteraction(t.Context(), response)
	requireSessionCode(t, err, product.CodeStateConflict)
	_, err = f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	requireSessionCode(t, err, product.CodeIncompatibleResume)
	if !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 || f.model.Calls() != 1 || before.Traces[f.input.TraceID].State != "cancelled" || before.Traces[f.input.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("late approval revived or consumed cancelled work")
	}
	snap, err := f.s.Snapshot(t.Context())
	if err != nil || snap.Interactions[response.InteractionID].State != "cancelled" || snap.Approvals[snap.Interactions[response.InteractionID].ApprovalID].State != "cancelled" {
		t.Fatalf("cancelled approval still appears usable: %+v %+v %v", snap.Interactions, snap.Approvals, err)
	}
}

func TestCancelAcceptedBeforeResumedApprovalHookExitsPreventsClaim(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var hooks atomic.Int32
	f := waitingApprovalSession(t, func(context.Context, agent.FrozenExecution) error {
		if hooks.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	})
	answerApproval(t, f, "allowed-once")
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed hook did not enter")
	}
	base, expire := context.WithCancelCause(t.Context())
	defer expire(context.Canceled)
	result := make(chan error, 1)
	go func() { result <- f.s.Cancel(policyWaitDeadline{base}, f.input.TraceID) }()
	var cancelled <-chan struct{}
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		cancelled = rt.active.ctx.Done()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	<-cancelled
	expire(context.DeadlineExceeded)
	if err := <-result; err != context.DeadlineExceeded {
		t.Fatalf("caller wait=%v", err)
	}
	before := f.manager.View()
	if before.Traces[f.input.TraceID].ExecutionStopped || f.runs.Load() != 0 || before.Traces[f.input.TraceID].Usage.ToolExecutions != 0 || len(before.ApprovalClaims) != 0 {
		t.Fatal("cancellation pretended the blocked hook had exited")
	}
	close(release)
	waitResumeCondition(t, func() bool { return f.manager.View().Traces[f.input.TraceID].State == "cancelled" })
	v := f.manager.View()
	if !v.Traces[f.input.TraceID].ExecutionStopped || f.runs.Load() != 0 || f.model.Calls() != 1 || v.Traces[f.input.TraceID].Usage.ToolExecutions != 0 || len(v.ApprovalClaims) != 0 {
		t.Fatal("cancelled approved call started late")
	}
}

func TestApprovalCurrentPolicyAndExpiryRejectBeforeResume(t *testing.T) {
	for _, gate := range []string{"policy", "expiry"} {
		t.Run(gate, func(t *testing.T) {
			f := waitingApprovalSession(t, nil)
			response := answerApproval(t, f, "allowed-once")
			if gate == "policy" {
				if err := f.s.rt.setExecutionPolicy(t.Context(), f.manager.View().ExecutionPolicy.Revision, agent.ResolvedPolicy{ApprovalPolicy: "never"}); err != nil {
					t.Fatal(err)
				}
			} else {
				expiry := approvalSnapshot(t, f.s).Interactions[response.InteractionID].ExpiresAt
				clock := &manualActivityClock{now: expiry}
				if err := f.s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = clock; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			before := f.manager.View()
			_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 || f.model.Calls() != 1 {
				t.Fatal("invalid permission consumed checkpoint or executed work")
			}
		})
	}
}

func requireSessionCode(t *testing.T, err error, code string) {
	t.Helper()
	pe, ok := product.AsError(err)
	if !ok || pe.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
