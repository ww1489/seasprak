package sessions

import (
	"context"
	"errors"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"testing"
	"time"
)

func TestP2BudgetMailboxDoesNotAcquireBusyLedger(t *testing.T) {
	st, err := memory.Open("lock-order", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := state.NewManager(st, "lock-order")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	target := agent.TargetAgent{Name: "main", Generation: "gen"}
	r, err := m.AcceptWithLimits(ctx, agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)}, target, config.Limits{TraceLogicalModelCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Accept(ctx, agent.InputCommand{Kind: "follow_up", TargetTraceID: r.TraceID, Content: []byte(`{"text":"next"}`)}, target); err != nil {
		t.Fatal(err)
	}
	if err = m.SaveTraceBudget(ctx, r.TraceID, agent.Usage{LogicalModelCalls: 1}); err != nil {
		t.Fatal(err)
	}
	b := agent.NewBudget(config.Limits{TraceLogicalModelCalls: 1})
	entered, release, occupied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.SetPersist(func(agent.Usage) error { close(entered); <-release; return errors.New("blocked persistence failed") })
	go func() { defer close(occupied); _ = b.OccupyTool() }()
	<-entered
	frame := &execution{scope: agent.ExecutionScope{TraceID: r.TraceID, ExecutionID: "segment"}, ctx: ctx, cancel: cancel, done: make(chan struct{}), budget: b}
	rt := &runtime{manager: m, active: frame}
	finished := make(chan struct{})
	go func() { defer close(finished); rt.segmentFinished(frame, nil) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		close(release)
		<-occupied
		<-finished
		t.Fatal("mailbox waited for a ledger whose persistence can wait for the mailbox")
	}
	close(release)
	<-occupied
	if m.View().Traces[r.TraceID].State != "failed" || !m.View().Traces[r.TraceID].ExecutionStopped {
		t.Fatal("budget stop lost actual exit proof")
	}
}

func TestP2BudgetOldExecutionCannotCommit(t *testing.T) {
	st, err := memory.Open("segment", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	manager, err := state.NewManager(st, "segment")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := agent.ExecutionScope{SessionID: "segment", TraceID: "trace", InvocationID: "inv", ExecutionID: "new"}
	rt := &runtime{manager: manager, mailbox: make(chan command), done: make(chan struct{}), active: &execution{scope: current, ctx: ctx}}
	// Pump only execution-port requests, without unrelated scheduling.
	pumpDone := make(chan struct{})
	defer close(pumpDone)
	go func() {
		for {
			select {
			case cmd := <-rt.mailbox:
				value, err := cmd.fn(rt)
				cmd.reply <- commandResult{value: value, err: err}
			case <-pumpDone:
				return
			}
		}
	}()
	old := current
	old.ExecutionID = "old"
	callCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	err = rt.CommitFact(callCtx, old, agent.Fact{Kind: "probe", Payload: []byte(`{}`)})
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict {
		t.Fatalf("old segment committed: %v", err)
	}
	for name, invoke := range map[string]func() error{
		"lookup":   func() error { _, err := rt.LookupTool(callCtx, old, "provider"); return err },
		"prepare":  func() error { _, err := rt.PrepareNextTurn(callCtx, old); return err },
		"finish":   func() error { return rt.FinishTurn(callCtx, old, agent.TurnFact{}) },
		"steering": func() error { _, err := rt.TakeSteering(callCtx, old); return err },
	} {
		if pe, ok := product.AsError(invoke()); !ok || pe.Code != product.CodeStateConflict {
			t.Fatalf("%s accepted an old segment: %v", name, pe)
		}
	}
	stopOld, reason, err := rt.ShouldStop(callCtx, old)
	if err != nil || !stopOld || reason != "execution_stopped" {
		t.Fatalf("old segment did not stop: %v %s %v", stopOld, reason, err)
	}
	if manager.View().LastSeq != 0 {
		t.Fatal("old segment changed journal")
	}
}
