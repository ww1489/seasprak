package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

func TestP2CommandConsumptionFailurePreservesResult(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejected", true: "lost-ack"}[lost], func(t *testing.T) {
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Text: "done"})}}
			artifacts := &shellLogStore{Memory: fixture.NewMemory()}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Operations: tools.Operations{Artifacts: artifacts, OutputRedactor: shellLogRedactor}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			// Replace only the journal writer in the idle mailbox; keep the real disk
			// store/header/manifest so close and public Open exercise actual recovery.
			var faults *shellHistoryFaultStore
			if err := s.rt.do(t.Context(), func(rt *runtime) error {
				faults = &shellHistoryFaultStore{Store: rt.opts.Store, phase: "host_command_consumed", lost: lost}
				manager, err := state.NewManager(faults, opts.SessionID)
				if err != nil {
					return err
				}
				rt.manager = manager
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			request := shellLogRequest(t, "lines", 7)
			out, err := s.ExecuteCommand(t.Context(), request)
			requireSessionCode(t, err, product.CodeStorageUnavailable)
			if !out.Started || !out.Terminated || out.ExitCode != 7 || out.Artifact.ID == "" || !out.Artifact.Available || out.LogError != "" || artifacts.saves.Load() != 1 || shellRuns(t, opts.Workspace) != 1 || faults.writes.Load() != 1 {
				t.Fatal("consumption failure lost durable output or repeated work")
			}
			view := s.rt.manager.View()
			snapshot := approvalSnapshot(t, s)
			if len(view.Messages) != 0 || len(view.HostCommands) != 1 || len(view.HostCommandConsumptions) != 0 || len(snapshot.Messages) != 1 {
				t.Fatal("failed consume published candidate or hid result")
			}
			_, err = s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"blocked"}`)})
			requireSessionCode(t, err, product.CodeStorageUnavailable)
			if model.Calls() != 0 || !reflect.DeepEqual(view, s.rt.manager.View()) {
				t.Fatal("faulted journal admitted model input")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close(context.Background()) })
			wantEntries := 0
			if lost {
				wantEntries = 1
			}
			before := reopened.rt.manager.View()
			if len(before.Messages) != wantEntries || len(before.HostCommandConsumptions) != wantEntries || len(approvalSnapshot(t, reopened).Messages) != 1 || model.Calls() != 0 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("reopen executed work or recovered partial consume")
			}
			input, err := reopened.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(reopened.rt.manager.View().Traces[input.TraceID].State) })
			requests := model.inputs()
			var command agent.AgentMessage
			for _, result := range before.HostCommands {
				command = result.Message
			}
			want, err := agent.ConvertToLLM([]agent.AgentMessage{command})
			if err != nil {
				t.Fatal(err)
			}
			text := want[0].ContentBlocks[0].UserInputText.Text
			if len(text) > 50<<10 || strings.Count(text, "\n")+1 > 2000 || !strings.Contains(text, "Command exited with code 7") || !strings.Contains(text, "Full output artifact: "+out.Artifact.ID+"]") {
				t.Fatal("recovered model representation lost bounds, status or reference")
			}
			want = append(want, schema.UserAgenticMessage("next"))
			if len(requests) != 1 || !reflect.DeepEqual(commandConversation(requests[0]), want) || artifacts.saves.Load() != 1 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("recovered command context changed, duplicated or reran")
			}
			after := reopened.rt.manager.View()
			count := 0
			for _, ev := range after.Events {
				if ev.Type == "message.finalized" {
					var msg agent.AgentMessage
					if json.Unmarshal(ev.Payload, &msg) == nil && msg.Kind == agent.KindCommand {
						count++
					}
				}
			}
			if count != 1 || len(after.HostCommandConsumptions) != 1 {
				t.Fatal("consumption emitted duplicate display event")
			}
		})
	}
}
