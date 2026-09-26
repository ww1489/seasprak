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

func TestP2FrozenSessionKeepsOriginalCallAndFinalExecution(t *testing.T) {
	backend, err := memory.Open("frozen-session", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "frozen-session")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{"n":1}`}}}, testkit.Step{Text: "done"})
	var starts atomic.Int32
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		PrepareArguments: []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"n":2}`), nil
		}},
		Execution: tools.ExecutionDescription{Effect: "read", Resources: []agent.ExecutionResource{{Identity: "file"}}},
		BeforeCall: []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, frozen agent.FrozenExecution) error {
			if string(frozen.FinalArguments) != `{"n":2}` {
				t.Error("hook saw original instead of final arguments")
			}
			frozen.FinalArguments[5] = '9'
			return nil
		}},
		Run: func(_ context.Context, arguments json.RawMessage) (string, error) {
			starts.Add(1)
			if string(arguments) != `{"n":2}` {
				t.Errorf("Run received modified arguments: %s", arguments)
			}
			return "ok", nil
		},
	}
	opts := Options{SessionID: "frozen-session", Profile: ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{def}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	frame := activityFrame(t, s)
	close(gate)
	activityWait(t, frame)
	v := manager.View()
	if starts.Load() != 1 || v.Traces[receipt.TraceID].State != "completed" || len(v.Calls) != 1 || len(v.FrozenExecutions) != 1 {
		t.Fatalf("starts=%d trace=%+v calls=%d frozen=%d", starts.Load(), v.Traces[receipt.TraceID], len(v.Calls), len(v.FrozenExecutions))
	}
	for id, call := range v.Calls {
		frozen := v.FrozenExecutions["execution:"+id]
		if call.Call.Arguments != `{"n":1}` || call.Call.ProviderCallID != "provider" || !call.Claimed || call.Observation == nil || !call.Observation.Executed ||
			string(frozen.FinalArguments) != `{"n":2}` || frozen.FinalArgumentsHash != toolArgumentHash([]byte(`{"n":2}`)) || frozen.OriginalArgumentsHash != toolArgumentHash([]byte(`{"n":1}`)) || frozen.PolicyRef != v.ExecutionPolicy.Ref {
			t.Fatalf("accepted call or frozen execution changed: call=%+v frozen=%+v", call, frozen)
		}
		if digest, err := frozen.Digest(); err != nil || digest != frozen.Hash {
			t.Fatal("persisted descriptor hash mismatch", err)
		}
	}
	replayed, err := state.NewManager(backend, "frozen-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed.View().FrozenExecutions) != 1 || len(replayed.View().Calls) != 1 || starts.Load() != 1 {
		t.Fatal("reopen lost frozen/accepted identity or reran tool")
	}
}
