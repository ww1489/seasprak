package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2OriginSameProviderIDInTwoInvocationsGetsDistinctCalls(t *testing.T) {
	backend, err := memory.Open("origin-identities", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "origin-identities")
	if err != nil {
		t.Fatal(err)
	}
	call := testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider-reused", Name: "work", Arguments: `{}`}}}
	model := testkit.NewFake(call, testkit.Step{Text: "first"}, call, testkit.Step{Text: "second"})
	var runs atomic.Int32
	opts := Options{SessionID: "origin-identities", Profile: ProfileMemory, Store: backend, Model: model,
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}},
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	for _, input := range []string{"first", "second"} {
		receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"` + input + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		waitSessionTrace(t, s, receipt.TraceID, "completed")
	}
	v := manager.View()
	if runs.Load() != 2 || len(v.Calls) != 2 || len(v.FrozenExecutions) != 2 {
		t.Fatalf("runs=%d calls=%d frozen=%d", runs.Load(), len(v.Calls), len(v.FrozenExecutions))
	}
	invocations := map[string]bool{}
	for id, record := range v.Calls {
		if record.Call.ProviderCallID != "provider-reused" || !record.Claimed || record.Observation == nil || record.Observation.Status != "succeeded" {
			t.Fatalf("call=%+v", record)
		}
		if v.FrozenExecutions["execution:"+id].Scope != record.Scope {
			t.Fatal("frozen scope lost invocation identity")
		}
		invocations[record.Scope.InvocationID] = true
	}
	if len(invocations) != 2 {
		t.Fatal("provider ID reused one product invocation")
	}
}
