package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Capture real model inputs, rather than treating visible history as evidence
// that the model received command context.
type commandContextModel struct {
	versionedPauseModel
	mu       sync.Mutex
	requests [][]*schema.AgenticMessage
}

func (m *commandContextModel) Generate(ctx context.Context, messages []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	var copied []*schema.AgenticMessage
	if err := json.Unmarshal(raw, &copied); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.requests = append(m.requests, copied)
	m.mu.Unlock()
	return m.versionedPauseModel.Generate(ctx, messages, opts...)
}
func (m *commandContextModel) inputs() [][]*schema.AgenticMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]*schema.AgenticMessage(nil), m.requests...)
}

func commandConversation(messages []*schema.AgenticMessage) []*schema.AgenticMessage {
	var out []*schema.AgenticMessage
	for _, msg := range messages {
		if msg.Role != schema.AgenticRoleTypeSystem {
			copy := *msg
			copy.Extra = nil
			// Eino assigns per-execution message IDs; these are not conversation
			// content or stable persisted identities. Keep every other field.
			for key, value := range msg.Extra {
				if key != "_eino_msg_id" {
					if copy.Extra == nil {
						copy.Extra = make(map[string]any)
					}
					copy.Extra[key] = value
				}
			}
			out = append(out, &copy)
		}
	}
	return out
}

func TestP2CommandContextIncludesResultOrExcludes(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "include", true: "exclude"}[exclude], func(t *testing.T) {
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Text: "done"})}}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			// Wire decoding permits a behavioral red test before adding the public field.
			script := shellScript(t, "echo run >>runs.txt; printf 'context-result'", "echo run>>runs.txt & echo context-result")
			raw, _ := json.Marshal(map[string]any{"command": script, "excludeFromContext": exclude})
			var request CommandRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				t.Fatal(err)
			}
			out, err := s.ExecuteCommand(t.Context(), request)
			if err != nil || !out.Started || out.ExitCode != 0 {
				t.Fatalf("shell=%+v %v", out, err)
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			files := commandDiskImage(t, opts.StateRoot)
			readOnly := opts
			readOnly.ReadOnly = true
			reader, err := OpenAgentSession(t.Context(), readOnly)
			if err != nil {
				t.Fatal(err)
			}
			if len(approvalSnapshot(t, reader).Messages) != 1 || len(reader.rt.manager.View().HostCommandConsumptions) != 1 {
				t.Fatal("read-only open lost consumed result")
			}
			if err := reader.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(files, commandDiskImage(t, opts.StateRoot)) {
				t.Fatal("read-only consumed history changed disk files")
			}
			s, err = OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			if model.Calls() != 0 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("opening the persisted command executed work")
			}
			snapshot := approvalSnapshot(t, s)
			if len(snapshot.Messages) != 1 || snapshot.Messages[0].Command == nil {
				t.Fatal("disk reopen lost the visible command result")
			}
			var saved CommandResult
			if json.Unmarshal(snapshot.Messages[0].Command.Result, &saved) != nil || saved != out {
				t.Fatal("disk reopen changed the shell result")
			}
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next input"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[input.TraceID].State) })
			inputs := model.inputs()
			if len(inputs) != 1 || shellRuns(t, s.rt.opts.Workspace) != 1 {
				t.Fatal("model or shell repeated")
			}
			want := []*schema.AgenticMessage{schema.UserAgenticMessage("next input")}
			if !exclude {
				want = append([]*schema.AgenticMessage{schema.UserAgenticMessage("Ran `" + script + "`\n```\n" + out.Output + "\n```")}, want...)
			}
			var conversational []*schema.AgenticMessage
			for _, msg := range inputs[0] {
				if msg.Role != schema.AgenticRoleTypeSystem {
					conversational = append(conversational, msg)
				}
			}
			if !reflect.DeepEqual(conversational, want) {
				t.Errorf("model command content/order differs: got=%d messages want=%d", len(conversational), len(want))
			}
			before := s.rt.manager.View()
			replayed, err := state.NewManager(s.rt.opts.Store, s.rt.opts.SessionID)
			if err != nil || !reflect.DeepEqual(replayed.View().Messages, before.Messages) || shellRuns(t, s.rt.opts.Workspace) != 1 {
				t.Fatal("replay lost history or repeated shell", err)
			}
		})
	}
}
