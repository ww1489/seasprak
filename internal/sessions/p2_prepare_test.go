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

func TestP2PrepareSessionFrozenAppendFailureStopsBeforeClaim(t *testing.T) {
	backend, err := memory.Open("prepare-fault", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	faults := &p2AttemptFaultStore{Store: backend, kind: "frozen_execution", failed: make(chan struct{})}
	manager, err := state.NewManager(faults, "prepare-fault")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var starts atomic.Int32
	model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}})
	opts := Options{SessionID: "prepare-fault", Profile: ProfileMemory, Store: faults, Model: model,
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { starts.Add(1); return "written", nil }}},
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"write"}`)})
	if err != nil {
		t.Fatal(err)
	}
	frame := activityFrame(t, s)
	close(gate)
	<-faults.failed
	activityWait(t, frame)
	view := manager.View()
	if starts.Load() != 0 || manager.Fault() == nil || len(view.Calls) != 1 || len(view.FrozenExecutions) != 0 || view.Traces[frame.scope.TraceID].Usage.ToolExecutions != 0 || view.LastSeq != faults.rejected.ExpectedPreviousSeq {
		t.Fatalf("starts=%d fault=%v calls=%d frozen=%d revision=%d", starts.Load(), manager.Fault(), len(view.Calls), len(view.FrozenExecutions), view.LastSeq)
	}
}
