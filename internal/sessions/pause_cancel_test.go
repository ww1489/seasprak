package sessions

import (
	"context"
	"encoding/json"
	"errors"
	goruntime "runtime"
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

func TestCancelOverridesAcceptedPauseWithoutCheckpoint(t *testing.T) {
	backend, err := memory.Open("cancel-pause", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "cancel-pause")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "not reached"})
	s, err := Start(Options{SessionID: "cancel-pause", Profile: ProfileMemory, Store: backend, Model: model,
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
			entered <- ctx
			<-release
			return "", ctx.Err()
		}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}, ResourceScheduler: tools.NewResourceScheduler()}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	receipt, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var toolCtx context.Context
	select {
	case toolCtx = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not enter")
	}
	paused := make(chan error, 1)
	go func() { _, err := s.Pause(context.Background(), receipt.TraceID); paused <- err }()
	deadline := time.After(5 * time.Second)
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		default:
			goruntime.Gosched()
		}
	}
	cancelled := make(chan error, 1)
	go func() { cancelled <- s.Cancel(context.Background(), receipt.TraceID) }()
	select {
	case <-toolCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not reach tool context")
	}
	view := manager.View()
	if tr := view.Traces[receipt.TraceID]; tr.State != "cancelling" || tr.ExecutionStopped || tr.Settled || len(view.Checkpoints) != 0 {
		t.Fatalf("Pause published before cancelled tool exit: %+v", tr)
	}
	select {
	case err := <-cancelled:
		t.Fatalf("Cancel returned before actual tool exit: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-paused:
		if err == nil {
			t.Fatal("cancelled Pause reported successful checkpoint")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pause did not return after Cancel")
	}
	select {
	case err := <-cancelled:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Cancel did not finish")
	}
	view = manager.View()
	if tr := view.Traces[receipt.TraceID]; tr.State != "cancelled" || !tr.ExecutionStopped || !tr.Settled || len(view.Checkpoints) != 0 || model.Calls() != 1 {
		t.Fatalf("Cancel lost priority over Pause: trace=%+v checkpoint=%d model=%d", tr, len(view.Checkpoints), model.Calls())
	}
}
