package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

// A blocked real model request proves shell completion is admitted during
// execution, not just before a queued prompt or after an approval pause.
func TestP2CommandFactWhileModelRunning(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "include", true: "exclude"}[exclude], func(t *testing.T) {
			gate := make(chan struct{})
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Gate: gate, Text: "done"})}}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"running input"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			before := s.rt.manager.View()
			snapshotBefore := approvalSnapshot(t, s)
			script := shellScript(t, "echo run >>runs.txt; printf 'parallel-result'", "echo run>>runs.txt & echo parallel-result")
			raw, _ := json.Marshal(map[string]any{"command": script, "excludeFromContext": exclude})
			var request CommandRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatal(err)
			}
			out, err := s.ExecuteCommand(t.Context(), request)
			if err != nil || !out.Started || !out.Terminated || out.ExitCode != 0 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatalf("shell did not complete alongside blocked model: %+v %v", out, err)
			}
			after := s.rt.manager.View()
			if terminal(after.Traces[input.TraceID].State) || model.Calls() != 1 || len(after.Calls) != 0 {
				t.Fatal("model gate or host/model execution separation failed")
			}
			if after.LeafID != before.LeafID || !reflect.DeepEqual(after.Messages, before.Messages) {
				t.Error("completed parallel shell changed active model history")
			}
			snapshot := approvalSnapshot(t, s)
			if len(snapshot.Messages) != len(snapshotBefore.Messages)+1 {
				t.Fatal("completed shell is missing or duplicated in Snapshot")
			}
			visible := snapshot.Messages[len(snapshot.Messages)-1]
			var saved CommandResult
			if visible.Command == nil || json.Unmarshal(visible.Command.Result, &saved) != nil || saved != out {
				t.Fatal("Snapshot lost actual shell result")
			}
			fact, ok := after.HostCommands[visible.ID]
			if !ok || fact.ExcludeFromContext != exclude || !reflect.DeepEqual(agent.PublicMessage(fact.Message), visible) {
				t.Error("completed parallel shell has no matching durable pending fact")
			}
			replayed, err := state.NewManager(s.rt.opts.Store, opts.SessionID)
			if err != nil || !reflect.DeepEqual(replayed.View().HostCommands, after.HostCommands) {
				t.Fatal("pending fact changed on journal replay", err)
			}
			inputs := model.inputs()
			var conversation []*schema.AgenticMessage
			if len(inputs) != 1 {
				t.Fatal("unexpected model invocation count")
			}
			for _, msg := range inputs[0] {
				if msg.Role != schema.AgenticRoleTypeSystem {
					conversation = append(conversation, msg)
				}
			}
			if !reflect.DeepEqual(conversation, []*schema.AgenticMessage{schema.UserAgenticMessage("running input")}) {
				t.Error("parallel command leaked into the already-started model request")
			}
			// Cancellation settles the model without releasing its gate. A reopen
			// must preserve the command, not turn the result into an execution queue.
			if err := s.Cancel(t.Context(), input.TraceID); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			reopenedSnapshot := approvalSnapshot(t, opened)
			count := 0
			for _, msg := range reopenedSnapshot.Messages {
				if msg.ID == visible.ID {
					count++
					if !reflect.DeepEqual(msg, visible) {
						t.Error("command result changed after cancel and disk reopen")
					}
				}
			}
			if count != 1 || shellRuns(t, opts.Workspace) != 1 || model.Calls() != 1 || opened.rt.manager.View().Traces[input.TraceID].State != "cancelled" {
				t.Fatal("cancel/reopen lost command history, duplicated it, or restarted work")
			}
		})
	}
}
