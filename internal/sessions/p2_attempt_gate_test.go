package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
)

type allowAll struct{}

func (allowAll) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionAllow, nil
}

func TestP2AttemptToolWrapperRequiresAcceptedAttempt(t *testing.T) {
	backend, err := memory.Open("gate", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	m, err := state.NewManager(backend, "gate")
	if err != nil {
		t.Fatal(err)
	}
	scope := agent.ExecutionScope{SessionID: "gate", TraceID: "trace", TurnID: "turn", InvocationID: "inv", ExecutionID: "execution", Generation: "gen"}
	if err := m.SaveTurn(t.Context(), agent.TurnRecord{ID: "turn", TraceID: "trace", InvocationID: "inv"}); err != nil {
		t.Fatal(err)
	}
	initial := state.ModelAttempt{ID: "attempt", ModelCallID: "turn", MessageID: "candidate", StreamID: "stream", Scope: scope, State: "started"}
	if err := m.SaveRecords(t.Context(), m.View().LastSeq, state.Records{ModelAttempts: []state.ModelAttempt{initial}}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "run", Arguments: `{}`, Generation: "gen"}}
	msg := assistantMessage(scope, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "run", Arguments: `{}`})}})
	// This is deliberately the compatibility entry, not attempt acceptance.
	if err := m.SaveAssistant(t.Context(), msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	rt := &runtime{manager: m, mailbox: make(chan command), done: make(chan struct{}), active: &execution{scope: scope, ctx: context.Background(), turnID: "turn"}}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case cmd := <-rt.mailbox:
				value, err := cmd.fn(rt)
				cmd.reply <- commandResult{value: value, err: err}
			case <-stop:
				return
			}
		}
	}()
	defer func() { close(stop); <-done }()
	var runs atomic.Int32
	exec, err := tools.NewExecutor("gen", []tools.Definition{{Name: "run", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ran", nil }}}, rt, allowAll{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	_, err = exec.Run(t.Context(), scope, "provider", "run", `{}`)
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || runs.Load() != 0 {
		t.Fatalf("unaccepted attempt gate: err=%v runs=%d", err, runs.Load())
	}
	if err := m.SaveAttemptResult(t.Context(), scope, "attempt", "failed", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	before := m.View().LastSeq
	payload, _ := json.Marshal(agent.ModelStreamSnapshot{AttemptID: "attempt", MessageID: "candidate", StreamID: "stream", ChunkSeq: 1})
	err = rt.CommitFact(t.Context(), scope, agent.Fact{Kind: "model_stream_snapshot", Payload: payload})
	pe, ok = product.AsError(err)
	if !ok || pe.Code != product.CodeStateConflict || m.View().LastSeq != before {
		t.Fatalf("late update changed ended attempt: %v", err)
	}
}
