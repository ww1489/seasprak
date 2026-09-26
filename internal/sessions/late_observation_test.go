package sessions

import (
	"context"
	"encoding/json"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"reflect"
	"testing"
)

func TestP2BudgetLateObservationAppendsEvidenceOnly(t *testing.T) {
	ctx := context.Background()
	st, err := memory.Open("late", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m, err := state.NewManager(st, "late")
	if err != nil {
		t.Fatal(err)
	}
	r, err := m.Accept(ctx, agent.InputCommand{Kind: "prompt", Content: []byte(`{"text":"hi"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err = m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	old := agent.ExecutionScope{SessionID: "late", TraceID: r.TraceID, InvocationID: m.View().Traces[r.TraceID].InvocationID, ExecutionID: "old", Generation: "gen", TurnID: "turn"}
	if err = m.SaveTurn(ctx, agent.TurnRecord{ID: "turn", TraceID: r.TraceID, InvocationID: old.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: old, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`}}
	msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: "late", TraceID: r.TraceID, InvocationID: old.InvocationID, TurnID: "turn"}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}}}
	if err = m.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	call.Claimed = true
	call.Observation = &agent.ToolObservation{Status: "outcome_unknown", Executed: true, SideEffect: "unknown"}
	if err = m.SaveCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	current := old
	current.ExecutionID = "new"
	current.TurnID = ""
	frameCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	frame := &execution{scope: current, ctx: frameCtx, turnID: "new-turn"}
	rt := &runtime{manager: m, active: frame, mailbox: make(chan command), done: make(chan struct{})}
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case cmd := <-rt.mailbox:
				v, e := cmd.fn(rt)
				cmd.reply <- commandResult{value: v, err: e}
			case <-done:
				return
			}
		}
	}()
	before := m.View()
	call.Observation = &agent.ToolObservation{Status: "succeeded", Executed: true, SideEffect: "confirmed", Content: "late result"}
	raw, _ := json.Marshal(call)
	fact := agent.Fact{Kind: "tool_observation", Payload: raw}
	if err = rt.CommitFact(ctx, old, fact); err != nil {
		t.Fatalf("late raw evidence was discarded: %v", err)
	}
	latest, err := m.LatestObservation("call")
	if err != nil {
		t.Fatal(err)
	}
	after := m.View()
	if latest.Version != 2 || latest.Observation != *call.Observation || after.LastSeq != before.LastSeq+1 || after.Calls["call"].Observation.Status != "outcome_unknown" || !after.HasUnresolvedEffects() {
		t.Fatalf("late observation changed original facts: %+v", latest)
	}
	if frame.turnID != "new-turn" || frame.ctx.Err() != nil || after.Traces[r.TraceID].ExecutionStopped || after.Traces[r.TraceID].State != "running" || after.Traces[r.TraceID].Usage != before.Traces[r.TraceID].Usage {
		t.Fatal("late result mutated current execution")
	}
	if err = rt.CommitFact(ctx, old, fact); err != nil || m.View().LastSeq != after.LastSeq {
		t.Fatal("duplicate late evidence appended twice", err)
	}
	forged := old
	forged.ExecutionID = "unrecorded"
	pe, ok := product.AsError(rt.CommitFact(ctx, forged, fact))
	if !ok || pe.Code != product.CodeStateConflict {
		t.Fatal("unrecorded execution supplied late evidence")
	}
	reopened, err := state.NewManager(st, "late")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.LatestObservation("call")
	if err != nil || !reflect.DeepEqual(saved, latest) {
		t.Fatal("late observation did not replay", err)
	}
}
