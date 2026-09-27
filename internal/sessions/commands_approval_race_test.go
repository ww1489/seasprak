package sessions

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestCommandApprovalConcurrentResumeAndCancelBeforeClaim(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var hooks atomic.Int32
	s, _, p, op, trace := commandApprovalFixture(t, false, nil, func(context.Context, agent.FrozenExecution) error {
		if hooks.Add(1) == 2 {
			close(entered)
			<-release
		}
		return nil
	})
	answerCommand(t, s, "allowed-once")
	cmd := ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "concurrent-resume"}
	type result struct {
		receipt state.OperationReceipt
		err     error
	}
	replies := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { r, e := s.Resume(t.Context(), cmd); replies <- result{r, e} }()
	}
	a, b := <-replies, <-replies
	if a.err != nil || b.err != nil || a.receipt != b.receipt {
		t.Fatalf("concurrent resume errors=%v/%v", a.err, b.err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume hook was not reached")
	}
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.Cancel(t.Context(), trace) }()
	waitResumeCondition(t, func() bool { return s.rt.manager.View().Traces[trace].State == "cancelling" })
	v := s.rt.manager.View()
	if v.Traces[trace].ExecutionStopped || terminal(v.Operations[op.OperationID].State) || p.calls.Load() != 0 || len(v.ApprovalClaims) != 0 {
		t.Fatal("cancellation claimed execution or fabricated exit")
	}
	once.Do(func() { close(release) })
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	v = s.rt.manager.View()
	if v.Traces[trace].State != "cancelled" || !v.Traces[trace].ExecutionStopped || v.Operations[op.OperationID].State != "cancelled" || p.calls.Load() != 0 || len(v.ApprovalClaims) != 0 {
		t.Fatalf("cancelled resume backend=%d operation=%s trace=%+v", p.calls.Load(), v.Operations[op.OperationID].State, v.Traces[trace])
	}
}

func TestCommandApprovalPermissionExpiryAndBindingGates(t *testing.T) {
	for _, gate := range []string{"unanswered", "policy", "expiry", "build", "environment", "history"} {
		t.Run(gate, func(t *testing.T) {
			s, _, p, _, trace := commandApprovalFixture(t, false, nil)
			if gate != "unanswered" {
				answerCommand(t, s, "allowed-once")
			}
			code := product.CodeIncompatibleResume
			switch gate {
			case "unanswered":
				code = product.CodeStateConflict
			case "policy":
				code = product.CodePermissionDenied
				if err := s.rt.setExecutionPolicy(t.Context(), s.rt.manager.View().ExecutionPolicy.Revision, agent.ResolvedPolicy{ApprovalPolicy: "never"}); err != nil {
					t.Fatal(err)
				}
			case "expiry":
				code = product.CodePermissionDenied
				v := s.rt.manager.View()
				for _, in := range v.Interactions {
					if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.clock = &manualActivityClock{now: in.ExpiresAt}; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			case "build":
				if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.GenerationFingerprint = "changed"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "environment":
				if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.ResourceEnvironment = "changed"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "history":
				if err := s.rt.manager.AppendMessage(t.Context(), agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCommand, Status: agent.StatusComplete, Command: &agent.CommandMessage{Name: "other"}}); err != nil {
					t.Fatal(err)
				}
			}
			before := s.rt.manager.View()
			_, err := s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, code)
			after := s.rt.manager.View()
			if after.LastSeq != before.LastSeq || p.calls.Load() != 0 || len(after.ApprovalClaims) != 0 {
				t.Fatal("failed gate changed facts or executed")
			}
		})
	}
}

func TestCommandApprovalUnknownEffectNeverReexecutes(t *testing.T) {
	s, _, p, op, trace := commandApprovalFixture(t, false, nil)
	answerCommand(t, s, "allowed-once")
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Operations.Process = commandUnknownProcess{p}; return nil }); err != nil {
		t.Fatal(err)
	}
	cmd := ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "resume-unknown"}
	receipt, err := s.Resume(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[trace].State) })
	v := s.rt.manager.View()
	if p.calls.Load() != 1 || !v.HasUnresolvedEffects() || len(v.ApprovalClaims) != 1 || v.Traces[trace].Usage.ToolExecutions != 1 {
		t.Fatal("unknown command lost claim or effect")
	}
	duplicate, err := s.Resume(t.Context(), cmd)
	if err != nil || duplicate != receipt {
		t.Fatalf("unknown retry=%+v err=%v", duplicate, err)
	}
	request := atomicCommandRequest()
	request.IdempotencyKey = "new"
	_, err = s.ExecuteCommand(t.Context(), request)
	requireSessionCode(t, err, product.CodeReconciliationRequired)
	if p.calls.Load() != 1 || !v.Calls[op.OperationID].Claimed {
		t.Fatal("unknown action reexecuted")
	}
}
