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

func TestPauseWaitsForToolExitBeforeAssociatingCheckpoint(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			backend, err := memory.Open("pause-tool", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, "pause-tool")
			if err != nil {
				t.Fatal(err)
			}
			model := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "not reached"})
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var runs atomic.Int32
			s, err := sessions.Start(sessions.Options{SessionID: "pause-tool", Profile: sessions.ProfileMemory, Model: model, Store: backend, Tools: []tools.Definition{{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
				runs.Add(1)
				close(entered)
				<-release
				return "ok", nil
			}}}, ToolInfos: []*schema.ToolInfo{testkit.ToolInfo("work", "work")}}, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer closeSession(t, s)
			receipt := submit(t, s)
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("tool did not start")
			}
			done := make(chan error, 1)
			go func() { _, err := s.Pause(context.Background(), receipt.TraceID); done <- err }()
			deadline := time.After(3 * time.Second)
			for {
				v := manager.View()
				if len(v.Operations) > 0 {
					if v.Traces[receipt.TraceID].State != "running" || v.Traces[receipt.TraceID].Settled || len(v.Checkpoints) > 0 {
						t.Fatalf("pause settled before tool exit: %+v", v.Traces[receipt.TraceID])
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("pause operation not accepted")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case err := <-done:
				t.Fatalf("pause returned before tool exit: %v", err)
			default:
			}
			time.Sleep(20 * time.Millisecond)
			close(release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("pause: %v trace=%+v", err, manager.View().Traces[receipt.TraceID])
				}
			case <-time.After(5 * time.Second):
				t.Fatal("pause did not complete")
			}
			v := manager.View()
			if v.Traces[receipt.TraceID].State != "paused" || v.Traces[receipt.TraceID].Settled || !v.Traces[receipt.TraceID].ExecutionStopped || len(v.Checkpoints) != 1 || runs.Load() != 1 || model.Calls() != 1 {
				t.Fatalf("unsafe pause: trace=%+v checkpoints=%d tools=%d models=%d", v.Traces[receipt.TraceID], len(v.Checkpoints), runs.Load(), model.Calls())
			}
			for _, cp := range v.Checkpoints {
				for _, operation := range v.Operations {
					if operation.State != "completed" || operation.ResultRef != cp.ID {
						t.Fatalf("pause operation and checkpoint diverged: %+v checkpoint=%+v", operation, cp)
					}
				}
			}
		})
	}
}
