package consumer_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"runtime"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerCommandContextModel struct {
	fakeModel
	requests chan []*schema.AgenticMessage
}

func (m *consumerCommandContextModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var copy []*schema.AgenticMessage
	if err := json.Unmarshal(body, &copy); err != nil {
		return nil, err
	}
	m.requests <- copy
	return m.fakeModel.Generate(ctx, in, opts...)
}

func TestConsumerCommandExclusionAndFullContext(t *testing.T) {
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "cmd"
	}
	if _, err := exec.LookPath(shell); err != nil {
		t.Skip("platform shell unavailable")
	}
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "include", true: "exclude"}[exclude], func(t *testing.T) {
			model := &consumerCommandContextModel{requests: make(chan []*schema.AgenticMessage, 1)}
			s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "command-context", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory, Store: &memStore{}, Model: model})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			request := sdk.CommandRequest{Command: "echo consumer-result", ExcludeFromContext: exclude}
			out, err := s.ExecuteCommand(t.Context(), request)
			if err != nil || !out.Started || !out.Terminated || out.ExitCode != 0 {
				t.Fatal("shell failed", err)
			}
			snapshot, err := s.Snapshot(t.Context())
			if err != nil || len(snapshot.Messages) != 1 || snapshot.Messages[0].Command == nil || snapshot.Messages[0].Command.ExcludeFromContext != exclude {
				t.Fatal("SDK lost visible result or exclusion flag", err)
			}
			var saved sdk.CommandResult
			if json.Unmarshal(snapshot.Messages[0].Command.Result, &saved) != nil || saved != out {
				t.Fatal("SDK visible execution result changed")
			}
			if _, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case in := <-model.requests:
				var got []*schema.AgenticMessage
				for _, msg := range in {
					if msg.Role != schema.AgenticRoleTypeSystem {
						got = append(got, msg)
					}
				}
				want := []*schema.AgenticMessage{schema.UserAgenticMessage("next")}
				if !exclude {
					want = append([]*schema.AgenticMessage{schema.UserAgenticMessage("Ran `echo consumer-result`\n```\n" + out.Output + "\n```")}, want...)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatal("public SDK model request lost command/output order or exclusion")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("model was not invoked")
			}
			// Close waits for the captured invocation to exit.
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			model.mu.Lock()
			calls := model.calls
			model.mu.Unlock()
			if calls != 1 {
				t.Fatal("model repeated")
			}
		})
	}
}
