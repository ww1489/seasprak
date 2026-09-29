package consumer_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

func TestConsumerCommandFinalModelBudgetPreservesResult(t *testing.T) {
	shell, script := "sh", "echo consumer; exit 7"
	if runtime.GOOS == "windows" {
		shell, script = "cmd", "echo consumer & exit /b 7"
	}
	if _, err := exec.LookPath(shell); err != nil {
		t.Skip("platform shell unavailable")
	}
	for _, output := range []string{"HEAD" + strings.Repeat("界", 17064) + "TAIL", "HEAD\n" + strings.Repeat("界\n", 1998) + "TAIL"} {
		t.Run(map[bool]string{true: "bytes", false: "lines"}[len(output) == 50<<10], func(t *testing.T) {
			model := &consumerCommandContextModel{requests: make(chan []*schema.AgenticMessage, 1)}
			redactions := 0
			s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "bounded-command", Workspace: t.TempDir(), StateRoot: "memory", Profile: sdk.ProfileMemory, Store: &memStore{}, Model: model, Operations: sdk.Operations{OutputRedactor: func(context.Context, string) (string, error) { redactions++; return output, nil }}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			out, err := s.ExecuteCommand(t.Context(), sdk.CommandRequest{Command: script})
			if err != nil || !out.Started || !out.Terminated || out.ExitCode != 7 || out.Output != output || out.Truncated || redactions != 1 {
				t.Fatal("actual shell result or trusted preview changed", err)
			}
			before, err := s.Snapshot(t.Context())
			if err != nil || len(before.Messages) != 1 {
				t.Fatal("missing result", err)
			}
			var saved sdk.CommandResult
			if json.Unmarshal(before.Messages[0].Command.Result, &saved) != nil || saved != out {
				t.Fatal("Snapshot did not retain full result")
			}
			if _, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case in := <-model.requests:
				var texts []string
				for _, msg := range in {
					if msg.Role == schema.AgenticRoleTypeUser {
						texts = append(texts, msg.ContentBlocks[0].UserInputText.Text)
					}
				}
				if len(texts) != 2 || texts[1] != "next" {
					t.Fatal("model context ordering differs")
				}
				text := texts[0]
				if len(text) > 50<<10 || strings.Count(text, "\n")+1 > 2000 || !utf8.ValidString(text) || !strings.Contains(text, "HEAD") || !strings.Contains(text, "TAIL") || !strings.Contains(text, "Command exited with code 7") || !strings.Contains(text, "truncated") {
					t.Fatal("final SDK model text is unbounded or lost status/preview")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("model request missing")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			after, err := s.Snapshot(t.Context())
			if err != nil || !reflect.DeepEqual(before.Messages[0], after.Messages[0]) || redactions != 1 {
				t.Fatal("model rendering changed snapshot or reran shell", err)
			}
		})
	}
}
