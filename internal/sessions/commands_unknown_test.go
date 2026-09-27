package sessions

import (
	"context"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

type commandUnknownProcess struct{ *commandProcessProbe }

func (p commandUnknownProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, sink agent.ProgressSink) (agent.ProcessObservation, error) {
	out, err := p.commandProcessProbe.Execute(ctx, req, sink)
	out.SideEffect = "unknown"
	return out, err
}
func TestExecuteCommandUnknownBlocksNewAcceptance(t *testing.T) {
	s, _, process := newAtomicCommandSession(t, "")
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Operations.Process = commandUnknownProcess{process}; return nil }); err != nil {
		t.Fatal(err)
	}
	receipt, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
	if err != nil {
		t.Fatal(err)
	}
	waitAtomicCommand(t, s, receipt.OperationID)
	before := s.rt.manager.View()
	if !before.HasUnresolvedEffects() {
		t.Fatal("unknown effect disappeared")
	}
	request := atomicCommandRequest()
	request.IdempotencyKey = "new"
	_, err = s.ExecuteCommand(t.Context(), request)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
		t.Fatalf("unknown accepted new command: %v", err)
	}
	after := s.rt.manager.View()
	if len(after.Operations) != 1 || len(after.Inputs) != 1 || process.calls.Load() != 1 {
		t.Fatalf("unknown advanced work: operations=%d inputs=%d runs=%d", len(after.Operations), len(after.Inputs), process.calls.Load())
	}
	duplicate, err := s.ExecuteCommand(t.Context(), atomicCommandRequest())
	if err != nil || duplicate != receipt {
		t.Fatalf("unknown must retain original receipt: %+v %v", duplicate, err)
	}
	holdID := tools.ResourceHoldID(s.rt.opts.SessionID, receipt.OperationID)
	if !s.rt.resourceScheduler().HasHold(holdID) {
		t.Fatal("unknown command lost its resource hold")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !s.rt.resourceScheduler().HasHold(holdID) {
		t.Fatal("closing the fixture released an unknown resource hold")
	}
}
