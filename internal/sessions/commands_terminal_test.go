package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2CommandFailureAndCloseKeepActualResult(t *testing.T) {
	for _, closePending := range []bool{false, true} {
		t.Run(map[bool]string{false: "failed-trace", true: "close-pending"}[closePending], func(t *testing.T) {
			gate := make(chan struct{})
			model := versionedPauseModel{testkit.NewFake(testkit.Step{Gate: gate, Err: product.NewError(product.CodeInvalidArgument, "fixture model failure")})}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			in, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"original"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			out, err := s.ExecuteCommand(t.Context(), CommandRequest{Command: shellScript(t, "echo run >>runs.txt; printf 'kept'", "echo run>>runs.txt & echo kept")})
			if err != nil {
				t.Fatal(err)
			}
			before := s.rt.manager.View()
			if len(before.HostCommands) != 1 || len(before.HostCommandConsumptions) != 0 {
				t.Fatal("active model consumed shell")
			}
			if !closePending {
				close(gate)
				waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[in.TraceID].State) })
				_ = approvalSnapshot(t, s)
				if s.rt.manager.View().Traces[in.TraceID].State != "failed" || len(s.rt.manager.View().HostCommandConsumptions) != 1 {
					t.Fatal("failed Trace lost pending context")
				}
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			view := opened.rt.manager.View()
			if !reflect.DeepEqual(view.HostCommands, before.HostCommands) || shellRuns(t, opts.Workspace) != 1 || model.Calls() != 1 {
				t.Fatal("close/reopen lost facts or repeated work")
			}
			if closePending {
				if view.Traces[in.TraceID].State != "paused" || len(view.HostCommandConsumptions) != 0 || !reflect.DeepEqual(view.Messages, before.Messages) {
					t.Fatal("close consumed paused history")
				}
				if err := opened.Cancel(t.Context(), in.TraceID); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := approvalSnapshot(t, opened)
			count := 0
			for _, msg := range snapshot.Messages {
				if msg.Kind == agent.KindCommand {
					count++
					var saved CommandResult
					if json.Unmarshal(msg.Command.Result, &saved) != nil || saved != out {
						t.Fatal("terminal/reopen changed result")
					}
				}
			}
			if count != 1 || len(opened.rt.manager.View().HostCommandConsumptions) != 1 || shellRuns(t, opts.Workspace) != 1 || model.Calls() != 1 {
				t.Fatal("terminal command missing or repeated")
			}
		})
	}
}
