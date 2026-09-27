package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"

	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

func commandApprovalFixture(t *testing.T, disk bool, change *atomic.Bool, hooks ...func(context.Context, agent.FrozenExecution) error) (*AgentSession, Options, *commandProcessProbe, state.OperationReceipt, string) {
	t.Helper()
	process := &commandProcessProbe{}
	def := builtinDefinitionForSession(t, "execute")
	def.Execution.RequestedGrantRef = "requires-approval"
	def.BeforeCall = hooks
	resolve := def.ResolveExecution
	if change != nil {
		def.ResolveExecution = func(ctx context.Context, raw json.RawMessage, d tools.ExecutionDescription) (tools.ExecutionDescription, error) {
			d, err := resolve(ctx, raw, d)
			if change.Load() {
				d.Argv = []string{"changed"}
			}
			return d, err
		}
	}
	root := "memory"
	if disk {
		root = t.TempDir()
	}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: root, Profile: ProfileMemory, Principal: "host", GenerationFingerprint: "command-approval-v1", Model: versionedPauseModel{testkit.NewFake()}, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
	if err != nil {
		t.Fatal(err)
	}
	trace := ""
	waitResumeCondition(t, func() bool {
		for id, tr := range s.rt.manager.View().Traces {
			trace = id
			if tr.State == "paused" || terminal(tr.State) {
				return true
			}
		}
		return false
	})
	if s.rt.manager.View().Traces[trace].State != "paused" {
		t.Fatal("command did not wait")
	}
	return s, opts, process, receipt, trace
}
func answerCommand(t *testing.T, s *AgentSession, decision string) InteractionResponse {
	t.Helper()
	v := s.rt.manager.View()
	for id := range v.Interactions {
		r := InteractionResponse{InteractionID: id, Decision: decision, ExpectedRevision: v.LastSeq, IdempotencyKey: "answer"}
		receipt, err := s.RespondInteraction(t.Context(), r)
		if err != nil {
			t.Fatal(err)
		}
		duplicate, err := s.RespondInteraction(t.Context(), r)
		if err != nil || duplicate != receipt {
			t.Fatalf("duplicate answer=%+v err=%v", duplicate, err)
		}
		return r
	}
	t.Fatal("no interaction")
	return InteractionResponse{}
}
func TestCommandApprovalCancelAndReject(t *testing.T) {
	for _, decision := range []string{"rejected", "cancelled", "cancel-before-answer", "cancel-after-answer"} {
		t.Run(decision, func(t *testing.T) {
			s, _, p, op, trace := commandApprovalFixture(t, false, nil)
			if decision == "rejected" || decision == "cancelled" {
				answerCommand(t, s, decision)
				_, err := s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if decision == "cancel-after-answer" {
					answerCommand(t, s, "allowed-once")
				}
				if err := s.Cancel(t.Context(), trace); err != nil {
					t.Fatal(err)
				}
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[trace].State) })
			v := s.rt.manager.View()
			if p.calls.Load() != 0 || !terminal(v.Operations[op.OperationID].State) || len(v.ApprovalClaims) != 0 || v.Traces[trace].Usage.ToolExecutions != 0 {
				t.Fatalf("cancel/reject backend=%d op=%s trace=%s", p.calls.Load(), v.Operations[op.OperationID].State, v.Traces[trace].State)
			}
			_, err := s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: v.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
		})
	}
}
func TestCommandApprovalDiskReopenAndDuplicateResume(t *testing.T) {
	for _, beforeAnswer := range []bool{true, false} {
		t.Run(map[bool]string{true: "before-answer", false: "after-answer"}[beforeAnswer], func(t *testing.T) {
			s, opts, p, op, trace := commandApprovalFixture(t, true, nil)
			if !beforeAnswer {
				answerCommand(t, s, "allowed-once")
			}
			before := s.rt.manager.View()
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			if p.calls.Load() != 0 || !reflect.DeepEqual(before, opened.rt.manager.View()) {
				t.Fatal("Open executed or changed waiting facts")
			}
			if beforeAnswer {
				answerCommand(t, opened, "allowed-once")
			}
			cmd := ResumeCommand{TraceID: trace, ExpectedRevision: opened.rt.manager.View().LastSeq, IdempotencyKey: "resume"}
			receipt, err := opened.Resume(t.Context(), cmd)
			if err != nil {
				t.Fatal(err)
			}
			duplicate, err := opened.Resume(t.Context(), cmd)
			if err != nil || duplicate != receipt {
				t.Fatalf("duplicate resume=%+v err=%v", duplicate, err)
			}
			waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[trace].State) })
			v := opened.rt.manager.View()
			if p.calls.Load() != 1 || v.Traces[trace].State != "completed" || v.Operations[op.OperationID].State != "completed" || len(v.ApprovalClaims) != 1 || v.Traces[trace].Usage.ToolExecutions != 1 {
				t.Fatalf("reopen backend=%d trace=%+v operation=%+v", p.calls.Load(), v.Traces[trace], v.Operations[op.OperationID])
			}
			if err := opened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			duplicate, err = reopened.Resume(t.Context(), cmd)
			if err != nil || duplicate != receipt || p.calls.Load() != 1 {
				t.Fatalf("reopen duplicate=%+v err=%v count=%d", duplicate, err, p.calls.Load())
			}
		})
	}
}
func TestCommandApprovalChangedDescriptionDoesNotExecute(t *testing.T) {
	var changed atomic.Bool
	s, _, p, op, trace := commandApprovalFixture(t, false, &changed)
	answerCommand(t, s, "allowed-once")
	changed.Store(true)
	_, err := s.Resume(t.Context(), ResumeCommand{TraceID: trace, ExpectedRevision: s.rt.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[trace].State) })
	v := s.rt.manager.View()
	if p.calls.Load() != 0 || len(v.ApprovalClaims) != 0 || v.Operations[op.OperationID].State != "failed" || v.Operations[op.OperationID].ErrorRef != product.CodeStateConflict {
		t.Fatalf("changed command ran: backend=%d op=%s", p.calls.Load(), v.Operations[op.OperationID].State)
	}
}

type commandApprovalProcess struct {
	commandProcessProbe
	started chan agent.AuthorizedProcess
}

func (p *commandApprovalProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, sink agent.ProgressSink) (agent.ProcessObservation, error) {
	out, err := p.commandProcessProbe.Execute(ctx, req, sink)
	p.started <- req
	return out, err
}

func TestCommandApprovalWaitAndExplicitResume(t *testing.T) {
	process := &commandApprovalProcess{started: make(chan agent.AuthorizedProcess, 1)}
	def := builtinDefinitionForSession(t, "execute")
	def.Execution.RequestedGrantRef = "requires-approval"
	model := versionedPauseModel{testkit.NewFake()}
	s, err := CreateAgentSession(t.Context(), Options{Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Principal: "host", GenerationFingerprint: "command-approval-v1", Model: model, Tools: []tools.Definition{def}, Operations: tools.Operations{Process: process}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
	if err != nil {
		t.Fatal(err)
	}
	var traceID string
	waitResumeCondition(t, func() bool {
		for id, tr := range s.rt.manager.View().Traces {
			traceID = id
			if tr.State == "paused" || terminal(tr.State) {
				return true
			}
		}
		return false
	})
	v := s.rt.manager.View()
	tr := v.Traces[traceID]
	if tr.State != "paused" || tr.Settled || !tr.ExecutionStopped || terminal(v.Operations[receipt.OperationID].State) || process.calls.Load() != 0 || model.Calls() != 0 || len(v.Turns) != 0 || len(v.Checkpoints) != 0 {
		t.Fatalf("direct approval must wait without a result: state=%s settled=%v stopped=%v operation=%s backend=%d model=%d turns=%d checkpoints=%d", tr.State, tr.Settled, tr.ExecutionStopped, v.Operations[receipt.OperationID].State, process.calls.Load(), model.Calls(), len(v.Turns), len(v.Checkpoints))
	}
	original := v.Calls[receipt.OperationID]
	frozen := v.FrozenExecutions["execution:"+receipt.OperationID]
	if original.Claimed || original.Observation != nil || frozen.Origin != "direct" || tr.Usage.ToolExecutions != 0 || tr.Activity.Reserved != 0 {
		t.Fatal("waiting manufactured execution or retained active time reservation")
	}
	if len(v.Interactions) != 1 {
		t.Fatalf("interactions=%d", len(v.Interactions))
	}
	for _, in := range v.Interactions {
		_, err = s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: in.ID, Decision: "allowed-once", ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "decision"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if process.calls.Load() != 0 || s.rt.manager.View().Traces[traceID].State != "paused" {
		t.Fatal("decision started backend")
	}
	_, err = s.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[traceID].State) })
	v = s.rt.manager.View()
	var executed agent.AuthorizedProcess
	select {
	case executed = <-process.started:
	default:
		t.Fatal("resumed command never reached process backend")
	}
	if !reflect.DeepEqual(executed.Authorization.Frozen, frozen) || !reflect.DeepEqual(executed.Argv, []string{"echo", "hi"}) || executed.Cwd != "workspace" || v.Calls[receipt.OperationID].Scope != original.Scope || v.Traces[traceID].ExecutionID == original.Scope.ExecutionID {
		t.Fatal("resume changed the original call or frozen backend request")
	}
	for _, input := range v.Inputs {
		if input.State != "consumed" {
			t.Fatal("command input not consumed")
		}
	}
	if len(v.Messages) != 1 || v.Messages[0].Kind != agent.KindCommand {
		t.Fatal("command created non-command messages")
	}
	if process.calls.Load() != 1 || model.Calls() != 0 || v.Traces[traceID].State != "completed" || v.Operations[receipt.OperationID].State != "completed" || len(v.Turns) != 0 || len(v.Checkpoints) != 0 {
		t.Fatalf("resume: backend=%d model=%d trace=%+v operation=%+v", process.calls.Load(), model.Calls(), v.Traces[traceID], v.Operations[receipt.OperationID])
	}
	request := atomicCommandRequest()
	request.IdempotencyKey = "another-command"
	next, err := s.ExecuteCommand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		view := s.rt.manager.View()
		for id, tr := range view.Traces {
			if id != traceID && (tr.State == "paused" || terminal(tr.State)) {
				return true
			}
		}
		return false
	})
	v = s.rt.manager.View()
	if process.calls.Load() != 1 || v.Operations[next.OperationID].State != "running" || len(v.Approvals) != 2 || len(v.ApprovalClaims) != 1 {
		t.Fatal("original approval authorized a different command")
	}
}
