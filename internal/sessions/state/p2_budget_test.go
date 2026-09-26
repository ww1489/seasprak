package state_test

import (
	"context"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"sync"
	"sync/atomic"
	"testing"
)

func budgetCall(t *testing.T) (*state.Manager, *failingStore, agent.ToolRecord) {
	t.Helper()
	m, s := fixture(t)
	r := accept(t, m, "budget", `{"text":"hi"}`)
	ctx := context.Background()
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	scope := agent.ExecutionScope{SessionID: "session", TraceID: r.TraceID, InvocationID: m.View().Traces[r.TraceID].InvocationID, TurnID: "turn", ExecutionID: "segment"}
	if err := m.SaveTurn(ctx, agent.TurnRecord{ID: "turn", TraceID: r.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "test", Arguments: `{}`}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "session", TraceID: r.TraceID, TurnID: "turn", InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "test", Arguments: `{}`})}}}
	if err := m.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	return m, s, call
}

func TestP2BudgetClaimSingleCommitAndConcurrentWinner(t *testing.T) {
	m, s, call := budgetCall(t)
	before := m.View().LastSeq
	usage := m.View().Traces[call.Scope.TraceID].Usage
	usage.ToolExecutions++
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.ClaimTool(context.Background(), call.Call, usage) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || m.View().LastSeq != before+1 {
		t.Fatalf("wins=%d commits=%d", wins.Load(), m.View().LastSeq-before)
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	v := reopened.View()
	if !v.Calls[call.Call.CallID].Claimed || v.Traces[call.Scope.TraceID].Usage.ToolExecutions != 1 {
		t.Fatal("claim and budget not recovered together")
	}
}

func TestP2BudgetClaimCommitFailureLeavesNeitherFact(t *testing.T) {
	m, s, call := budgetCall(t)
	before := m.View().LastSeq
	usage := m.View().Traces[call.Scope.TraceID].Usage
	usage.ToolExecutions++
	s.fail = true
	if err := m.ClaimTool(context.Background(), call.Call, usage); err == nil {
		t.Fatal("expected failure")
	}
	v := m.View()
	if v.LastSeq != before || v.Calls[call.Call.CallID].Claimed || v.Traces[call.Scope.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("failed claim leaked state")
	}
}

func TestP2BudgetLogicalCallAndPhysicalCountReopen(t *testing.T) {
	m, s := fixture(t)
	r := accept(t, m, "model", `{"text":"hi"}`)
	if err := m.SetTraceState(context.Background(), r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	b := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	b.SetPersist(func(u agent.Usage) error { return m.SaveTraceBudget(context.Background(), r.TraceID, u) })
	before := m.View().LastSeq
	if err := b.BeginTurnID("logical"); err != nil {
		t.Fatal(err)
	}
	if m.View().LastSeq != before+1 || m.View().Turns["logical"].ID != "logical" {
		t.Fatal("logical call and occupancy were not committed together")
	}
	for i := 0; i < 3; i++ {
		if err := b.OccupyModel(); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := state.NewManager(s, "session")
	if err != nil {
		t.Fatal(err)
	}
	beforeReject := reopened.View().LastSeq
	refunded := reopened.View().Traces[r.TraceID].Usage
	refunded.ModelRequests = 0
	if err := reopened.SaveTraceBudget(context.Background(), r.TraceID, refunded); err == nil || reopened.View().LastSeq != beforeReject {
		t.Fatal("durable model request usage could be refunded")
	}
	if reopened.View().Turns["logical"].TransportRequests != 3 {
		t.Fatal("physical counts lost")
	}
	restored := agent.NewBudget(config.Limits{LogicalModelRequests: 3})
	restored.Restore(reopened.View().Traces[r.TraceID].Usage)
	if err := restored.BeginTurnID("logical"); err != nil {
		t.Fatal(err)
	}
	if err := restored.OccupyModel(); err == nil {
		t.Fatal("reopen refunded physical requests")
	}
	if restored.Snapshot().LogicalModelCalls != 1 || restored.Snapshot().TransportRequests != 3 {
		t.Fatal("recovery charged twice or refunded")
	}
}
