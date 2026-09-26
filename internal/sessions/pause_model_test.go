package sessions_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestPauseAfterModelKeepsUnfinishedTurnAndDoesNotRunTool(t *testing.T) {
	backend, err := memory.Open("pause-model-tool", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "pause-model-tool")
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	defer func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}()
	var runs atomic.Int32
	model := testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "not reached"})
	s, err := sessions.Start(sessions.Options{SessionID: "pause-model-tool", Profile: sessions.ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ok", nil }}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, s)
	receipt := submit(t, s)
	deadline := time.After(3 * time.Second)
	for model.Calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("model not entered")
		case <-time.After(time.Millisecond):
		}
	}
	result := make(chan error, 1)
	go func() { _, err := s.Pause(context.Background(), receipt.TraceID); result <- err }()
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		case <-time.After(time.Millisecond):
		}
	}
	if len(manager.View().Checkpoints) != 0 || runs.Load() != 0 || manager.View().Traces[receipt.TraceID].Settled {
		t.Fatal("checkpoint or tool appeared before model exited")
	}
	time.Sleep(80 * time.Millisecond)
	close(gate)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("pause: %v trace=%+v", err, manager.View().Traces[receipt.TraceID])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause not completed")
	}
	view := manager.View()
	if view.Traces[receipt.TraceID].State != "paused" || view.Traces[receipt.TraceID].Settled || len(view.Checkpoints) != 1 || runs.Load() != 0 || model.Calls() != 1 {
		t.Fatalf("pause state=%+v checkpoints=%d tools=%d models=%d", view.Traces[receipt.TraceID], len(view.Checkpoints), runs.Load(), model.Calls())
	}
	if len(view.Turns) != 1 {
		t.Fatalf("turns=%v", view.Turns)
	}
	for _, turn := range view.Turns {
		if turn.Ended {
			t.Fatalf("pause ended unfinished turn: %+v", turn)
		}
	}
	for _, event := range view.Events {
		if event.Type == "trace.settled" {
			t.Fatal("pause published trace.settled")
		}
	}
}
