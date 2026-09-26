package sessions

import (
	"context"
	"encoding/json"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestPauseKeepsAcceptedFollowUpPending(t *testing.T) {
	backend, err := memory.Open("pause-pending-input", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "pause-pending-input")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var runs atomic.Int32
	model := testkit.NewFake(
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}},
		testkit.Step{Text: "not reached"},
	)
	s, err := Start(Options{SessionID: "pause-pending-input", Profile: ProfileMemory, Store: backend, Model: model,
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
			runs.Add(1)
			close(entered)
			<-release
			return "ok", nil
		}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not start")
	}
	follow, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "follow_up", TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"later"}`)})
	if err != nil {
		t.Fatal(err)
	}
	pauseResult := make(chan error, 1)
	go func() { _, err := s.Pause(context.Background(), first.TraceID); pauseResult <- err }()
	deadline := time.After(5 * time.Second)
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause operation was not accepted")
		default:
			goruntime.Gosched()
		}
	}
	// The operation record becomes visible inside the mailbox callback. This
	// barrier ensures the callback has also requested Graceful Stop before exit.
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-pauseResult:
		if err != nil {
			t.Fatalf("accepted follow-up prevented safe pause: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not settle")
	}
	view := manager.View()
	if tr := view.Traces[first.TraceID]; tr == nil || tr.State != "paused" || tr.Settled || !tr.ExecutionStopped {
		t.Fatalf("trace was not safely paused: %+v", tr)
	}
	if in := view.Inputs[follow.InputID]; in == nil || in.State != "pending" || in.TraceID != first.TraceID {
		t.Fatalf("accepted follow-up lost: %+v", in)
	}
	if len(view.Checkpoints) != 1 || model.Calls() != 1 || runs.Load() != 1 {
		t.Fatalf("checkpoint=%d model=%d tool=%d", len(view.Checkpoints), model.Calls(), runs.Load())
	}
	for _, operation := range view.Operations {
		if operation.State != "completed" {
			t.Fatalf("pause operation is not complete: %+v", operation)
		}
	}
}
