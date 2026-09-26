package sessions_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type unendedPauseProcess struct {
	entered chan struct{}
	release chan struct{}
	starts  atomic.Int32
}

func (p *unendedPauseProcess) Execute(ctx context.Context, request agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.starts.Add(1)
	close(p.entered)
	<-p.release
	return agent.ProcessObservation{Started: true, Terminated: false, Content: "returned without exiting", SideEffect: "none"}, nil
}
func (*unendedPauseProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: false}, nil
}

func TestPauseRejectsUnterminatedProcessEvenAfterToolReturns(t *testing.T) {
	backend, err := memory.Open("pause-unterminated", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "pause-unterminated")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	process := &unendedPauseProcess{entered: make(chan struct{}), release: release}
	model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "not reached"})
	var callbacks atomic.Int32
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"fixture"}}, Run: func(context.Context, json.RawMessage) (string, error) { callbacks.Add(1); return "unexpected", nil }}
	s, err := sessions.Start(sessions.Options{SessionID: "pause-unterminated", Profile: sessions.ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{def}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}, Operations: tools.Operations{Process: process}, ResourceScheduler: tools.NewResourceScheduler()}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, s)
	receipt := submit(t, s)
	select {
	case <-process.entered:
	case <-time.After(3 * time.Second):
		t.Fatalf("process not entered: trace=%+v calls=%+v model=%d", manager.View().Traces[receipt.TraceID], manager.View().Calls, model.Calls())
	}
	result := make(chan error, 1)
	go func() { _, err := s.Pause(context.Background(), receipt.TraceID); result <- err }()
	deadline := time.After(3 * time.Second)
	for len(manager.View().Operations) == 0 {
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		case <-time.After(time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("unterminated process was pausable")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not exit")
	}
	view := manager.View()
	if len(view.Checkpoints) != 0 || view.Traces[receipt.TraceID].State == "paused" || process.starts.Load() != 1 || callbacks.Load() != 0 || model.Calls() != 1 {
		t.Fatalf("process mistaken for stopped: trace=%+v checkpoints=%d process=%d callbacks=%d model=%d", view.Traces[receipt.TraceID], len(view.Checkpoints), process.starts.Load(), callbacks.Load(), model.Calls())
	}
}
