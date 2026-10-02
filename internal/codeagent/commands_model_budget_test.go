package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2CommandBoundedRecoveryKeepsSnapshotFacts(t *testing.T) {
	model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Text: "done"})}}
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	// Seed an immutable historical result, including a legacy oversized reference.
	// No shell is executed by this replay/projection fixture.
	command := strings.Repeat("历史命令😀\n", 2200)
	result := CommandResult{Output: "HEAD " + strings.Repeat("输出😀", 8000) + " TAIL", Started: true, Terminated: true, ExitCode: 7, Truncated: true, OutputIncomplete: true, Artifact: agent.ArtifactRef{ID: "saved-identity-" + strings.Repeat("x", 60<<10), Available: true}}
	content, _ := json.Marshal(CommandRequest{Command: command})
	body, _ := json.Marshal(result)
	msg := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCommand, Status: agent.StatusComplete, Scope: agent.MessageScope{SessionID: opts.SessionID}, Source: agent.SourceRef{Kind: agent.SourceHuman, Description: "host shell"}, Command: &agent.CommandMessage{Name: "shell", Content: content, Result: body}}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		if err := rt.manager.SaveHostCommand(t.Context(), msg, false); err != nil {
			return err
		}
		return rt.flushHostCommands(t.Context())
	}); err != nil {
		t.Fatal(err)
	}
	visible := approvalSnapshot(t, s).Messages
	if len(visible) != 1 || !reflect.DeepEqual(visible[0], agent.PublicMessage(msg)) {
		t.Fatal("saving shrank Snapshot facts")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	// This exercises replay and projection, not activity timing. Large historical
	// facts plus race instrumentation can exhaust the real one-second lease
	// before the model starts; activity tests cover expiry with this same clock.
	if err := reopened.rt.do(t.Context(), func(rt *runtime) error { rt.clock = newManualActivityClock(); return nil }); err != nil {
		t.Fatal(err)
	}
	before := reopened.rt.manager.View()
	if !reflect.DeepEqual(visible, approvalSnapshot(t, reopened).Messages) || model.Calls() != 0 {
		t.Fatal("reopen changed snapshot or ran model")
	}
	input, err := reopened.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(reopened.rt.manager.View().Traces[input.TraceID].State) })
	requests := model.inputs()
	trace := reopened.rt.manager.View().Traces[input.TraceID]
	if trace.State != "completed" || len(requests) != 1 || model.Calls() != 1 {
		t.Fatalf("model execution differs: requests=%d calls=%d trace=%+v", len(requests), model.Calls(), trace)
	}
	conversation := commandConversation(requests[0])
	if len(conversation) != 2 {
		t.Fatal("model context message count differs")
	}
	text := conversation[0].ContentBlocks[0].UserInputText.Text
	if len(text) > 50<<10 || strings.Count(text, "\n")+1 > 2000 || !utf8.ValidString(text) || !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") || !strings.Contains(text, "Command exited with code 7") || !strings.Contains(text, "[Output collection incomplete.]") || !strings.Contains(text, "reference omitted from model display") || strings.Contains(text, "saved-identity-") {
		t.Fatal("recovered actual model input lost bounds, status, preview or truthful omission")
	}
	after := reopened.rt.manager.View()
	snapshot := approvalSnapshot(t, reopened)
	if !reflect.DeepEqual(before.HostCommands, after.HostCommands) || !reflect.DeepEqual(snapshot.Messages[0], visible[0]) {
		t.Fatal("model rendering modified persistent facts or Snapshot")
	}
}
