package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2CommandWaitsForFinalTraceBeforeQueuedInput(t *testing.T) {
	for _, fault := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "consume-rejected"}[fault], func(t *testing.T) {
			firstGate, followGate := make(chan struct{}), make(chan struct{})
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Gate: firstGate, Text: "first done"}, testkit.Step{Gate: followGate, Text: "follow done"}, testkit.Step{Text: "queued done"})}}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			if fault {
				if err := s.rt.do(t.Context(), func(rt *runtime) error {
					manager, err := state.NewManager(&shellHistoryFaultStore{Store: rt.opts.Store, phase: "host_command_consumed"}, opts.SessionID)
					if err != nil {
						return err
					}
					rt.manager = manager
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			request := CommandRequest{Command: shellScript(t, "echo run >>runs.txt; printf 'pending'", "echo run>>runs.txt & echo pending")}
			out, err := s.ExecuteCommand(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "follow_up", TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"follow"}`)}); err != nil {
				t.Fatal(err)
			}
			next, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"independent"}`)})
			if err != nil {
				t.Fatal(err)
			}
			close(firstGate)
			waitResumeCondition(t, func() bool { return model.Calls() == 2 })
			// A continuation on the original Trace must receive no pending shell data.
			v := s.rt.manager.View()
			requests := model.inputs()
			if len(v.HostCommandConsumptions) != 0 || len(requests) != 2 {
				t.Fatal("same Trace consumed pending context")
			}
			// Compare against persisted projection to retain the assistant's finish metadata.
			wantFollow, err := agent.ConvertToLLM(v.Messages)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(commandConversation(requests[1]), wantFollow) {
				t.Fatal("continuation request differs from original history")
			}
			close(followGate)
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[first.TraceID].State) })
			_ = approvalSnapshot(t, s)
			if fault {
				v = s.rt.manager.View()
				if s.rt.manager.Fault() == nil || model.Calls() != 2 || v.Traces[next.TraceID].State != "queued" || len(v.HostCommandConsumptions) != 0 {
					t.Fatal("consume failure started queued model or published history")
				}
			} else {
				waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[next.TraceID].State) })
				requests = model.inputs()
				if len(requests) != 3 {
					t.Fatal("queued model invocation differs")
				}
				// Original Trace's final response precedes shell context, then next prompt.
				want := append([]*schema.AgenticMessage(nil), wantFollow...)
				finalProjection, err := agent.ConvertToLLM(s.rt.manager.View().Messages)
				if err != nil {
					t.Fatal(err)
				}
				want = append(want, finalProjection[len(wantFollow)], schema.UserAgenticMessage("Ran `"+request.Command+"`\n```\n"+out.Output+"\n```"), schema.UserAgenticMessage("independent"))
				if !reflect.DeepEqual(commandConversation(requests[2]), want) {
					t.Fatal("queued input did not follow complete command context exactly once")
				}
			}
			if shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("shell repeated")
			}
		})
	}
}
