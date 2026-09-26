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

type approvalSessionFixture struct {
	s       *AgentSession
	manager *state.Manager
	model   versionedPauseModel
	input   agent.InputReceipt
	runs    *atomic.Int32
}

func startApprovalSession(t *testing.T, decisionHook func(context.Context, agent.FrozenExecution) error, decorators ...func(store.Store) store.Store) approvalSessionFixture {
	t.Helper()
	id := agent.MustID()
	memoryStore, err := memory.Open(id, store.Header{})
	var backend store.Store = memoryStore
	if err != nil {
		t.Fatal(err)
	}
	for _, decorate := range decorators {
		backend = decorate(backend)
	}
	manager, err := state.NewManager(backend, id)
	if err != nil {
		t.Fatal(err)
	}
	model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
	runs := &atomic.Int32{}
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "approved result", nil
	}}
	if decisionHook != nil {
		def.BeforeCall = []func(context.Context, agent.FrozenExecution) error{decisionHook}
	}
	opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "approval-bundle-v1", Store: backend, Model: model, Tools: []tools.Definition{def}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return approvalSessionFixture{s: s, manager: manager, model: model, input: input, runs: runs}
}

func waitingApprovalSession(t *testing.T, decisionHook func(context.Context, agent.FrozenExecution) error) approvalSessionFixture {
	t.Helper()
	f := startApprovalSession(t, decisionHook)
	waitResumeCondition(t, func() bool {
		tr := f.manager.View().Traces[f.input.TraceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	if tr := f.manager.View().Traces[f.input.TraceID]; tr.State != "paused" || !tr.ExecutionStopped || tr.CheckpointID == "" || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatalf("fixture did not safely wait: %+v", tr)
	}
	return f
}

func answerApproval(t *testing.T, f approvalSessionFixture, decision string) InteractionResponse {
	t.Helper()
	for id := range f.manager.View().Interactions {
		response := InteractionResponse{InteractionID: id, Decision: decision, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "answer"}
		if _, err := f.s.RespondInteraction(t.Context(), response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	t.Fatal("approval interaction missing")
	return InteractionResponse{}
}

func TestApprovalPartialAnswersKeepUnansweredOriginalCallsWaiting(t *testing.T) {
	for _, kind := range []string{"invokable", "enhanced-invokable"} {
		t.Run(kind, func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := state.NewManager(backend, id)
			if err != nil {
				t.Fatal(err)
			}
			var counts [3]atomic.Int32
			labels := []string{"a", "b", "c"}
			model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "a", Name: "work", Arguments: `{"label":"a"}`}, {CallID: "b", Name: "work", Arguments: `{"label":"b"}`}, {CallID: "c", Name: "work", Arguments: `{"label":"c"}`}}}, testkit.Step{Text: "finished"})}
			opts := Options{SessionID: id, Workspace: t.TempDir(), Principal: "host-user", Profile: ProfileMemory, GenerationFingerprint: "approval-bundle-v1", Store: backend, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", ToolInterface: kind, Schema: json.RawMessage(`{"type":"object","required":["label"]}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(_ context.Context, args json.RawMessage) (string, error) {
				var input struct{ Label string }
				if err := json.Unmarshal(args, &input); err != nil {
					return "", err
				}
				for index, label := range labels {
					if input.Label == label {
						counts[index].Add(1)
					}
				}
				return input.Label, nil
			}}}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				tr := manager.View().Traces[input.TraceID]
				return tr.State == "paused" || terminal(tr.State)
			})
			original := manager.View()
			if len(original.Interactions) != 3 || original.Traces[input.TraceID].State != "paused" {
				t.Fatalf("batch did not ask three original approvals: %+v", original.Traces[input.TraceID])
			}
			for index, label := range labels {
				v := manager.View()
				var interactionID string
				for id, in := range v.Interactions {
					if v.Calls[in.CallID].Call.ProviderCallID == label {
						interactionID = id
					}
				}
				if interactionID == "" {
					t.Fatal("original interaction lost")
				}
				if _, err := s.RespondInteraction(t.Context(), InteractionResponse{InteractionID: interactionID, Decision: "allowed-once", ExpectedRevision: v.LastSeq, IdempotencyKey: label}); err != nil {
					t.Fatalf("answer %s: %v", label, err)
				}
				if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: manager.View().LastSeq}); err != nil {
					t.Fatalf("resume %s: %v", label, err)
				}
				var resumed *execution
				if err := s.rt.do(t.Context(), func(rt *runtime) error { resumed = rt.active; return nil }); err != nil {
					t.Fatal(err)
				}
				waitResumeCondition(t, func() bool {
					tr := manager.View().Traces[input.TraceID]
					return tr.State == "paused" || terminal(tr.State)
				})
				v = manager.View()
				wanted := "paused"
				if index == 2 {
					wanted = "completed"
				}
				if v.Traces[input.TraceID].State != wanted || v.Traces[input.TraceID].Usage.ToolExecutions != index+1 {
					if resumed != nil && resumed.activity != nil {
						resumed.activity.mu.Lock()
						t.Logf("activity cause=%v record=%+v", resumed.activity.err, resumed.activity.record)
						resumed.activity.mu.Unlock()
					}
					t.Fatalf("partial %s trace=%+v counts=%d/%d/%d", label, v.Traces[input.TraceID], counts[0].Load(), counts[1].Load(), counts[2].Load())
				}
				for n := range counts {
					want := int32(0)
					if n <= index {
						want = 1
					}
					if counts[n].Load() != want {
						t.Fatalf("after %s counts=%d/%d/%d", label, counts[0].Load(), counts[1].Load(), counts[2].Load())
					}
				}
				for id, old := range original.Calls {
					if v.Calls[id].Call != old.Call || v.Calls[id].Scope != old.Scope {
						t.Fatal("partial resume rewrote an original call identity")
					}
				}
			}
			if model.Calls() != 2 {
				t.Fatal("partial resumes replayed the original model")
			}
		})
	}
}
