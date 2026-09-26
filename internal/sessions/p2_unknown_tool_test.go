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

func TestP2OriginUnknownToolHasDeniedObservation(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonempty_inventory", true: "empty_inventory"}[empty], func(t *testing.T) {
			testP2UnknownTool(t, empty)
		})
	}
}

func testP2UnknownTool(t *testing.T, empty bool) {
	backend, err := memory.Open("unknown-tool", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "unknown-tool")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "unknown", Name: "not_registered", Arguments: `{}`}}}, testkit.Step{Text: "done"})
	var runs atomic.Int32
	opts := Options{SessionID: "unknown-tool", Profile: ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{{Name: "registered", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "ran", nil
	}}}}
	if empty {
		opts.Tools = nil
	}
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
	view := manager.View()
	if runs.Load() != 0 || len(view.Calls) != 1 || len(view.FrozenExecutions) != 0 || view.Traces[receipt.TraceID].Usage.ToolExecutions != 0 {
		t.Fatal("unknown tool executed, claimed budget, or changed accepted identity")
	}
	for _, call := range view.Calls {
		if call.Claimed || call.Observation == nil || call.Observation.Status != "denied" || call.Observation.Executed || call.Observation.SideEffect != "none" {
			t.Fatalf("unknown tool did not retain a denied, unstarted observation: %+v", call.Observation)
		}
	}
	results := 0
	for _, message := range view.Messages {
		if message.Kind == agent.KindToolResult {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("denied call has %d paired results", results)
	}
	for _, event := range view.Events {
		if event.Type == "tool.started" {
			t.Fatal("unknown tool published a started event")
		}
	}
}
