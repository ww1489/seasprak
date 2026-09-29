package agent_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
)

func TestHostShellCommandProjection(t *testing.T) {
	for _, tc := range []struct {
		name, result, want string
		exclude            bool
	}{
		{"output", `{"output":"hello","exitCode":0,"started":true,"terminated":true}`, "Ran `echo hello`\n```\nhello\n```", false},
		{"empty", `{"output":"","exitCode":0,"started":true,"terminated":true}`, "Ran `echo hello`\n(no output)", false},
		{"nonzero", `{"output":"bad","exitCode":7,"started":true,"terminated":true}`, "Ran `echo hello`\n```\nbad\n```\n\nCommand exited with code 7", false},
		{"cancel", `{"output":"","exitCode":-1,"started":true,"terminated":true,"cancelled":true}`, "Ran `echo hello`\n(no output)\n\n(command cancelled)", false},
		{"exclude", `{"output":"secret from model","exitCode":0,"started":true,"terminated":true}`, "", true},
		{"timeout", `{"output":"","exitCode":-1,"started":true,"terminated":true,"timedOut":true}`, "Ran `echo hello`\n(no output)\n\n(command timed out)", false},
		{"artifact", `{"output":"preview","exitCode":0,"started":true,"terminated":true,"truncated":true,"artifact":{"id":"log-1","available":true}}`, "Ran `echo hello`\n```\npreview\n```\n\n[Output truncated. Full output artifact: log-1]", false},
		{"unavailable", `{"output":"preview","exitCode":0,"started":true,"terminated":true,"truncated":true,"artifact":{"id":"log-1","available":false}}`, "Ran `echo hello`\n```\npreview\n```\n\n[Output truncated. Full output unavailable.]", false},
		{"incomplete", `{"output":"partial","exitCode":0,"started":true,"terminated":true,"outputIncomplete":true}`, "Ran `echo hello`\n```\npartial\n```\n\n[Output collection incomplete.]", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"name": "shell", "content": json.RawMessage(`{"command":"echo hello"}`), "result": json.RawMessage(tc.result), "excludeFromContext": tc.exclude})
			var cmd agent.CommandMessage
			if err := json.Unmarshal(raw, &cmd); err != nil {
				t.Fatal(err)
			}
			got, err := agent.ConvertToLLM([]agent.AgentMessage{{ID: "shell", Kind: agent.KindCommand, Status: agent.StatusComplete, Command: &cmd}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.exclude {
				if len(got) != 0 {
					t.Fatal("excluded command entered context")
				}
				return
			}
			if !reflect.DeepEqual(got, []*schema.AgenticMessage{schema.UserAgenticMessage(tc.want)}) {
				t.Fatalf("command projection differs: %+v", got)
			}
		})
	}
	legacy := agent.AgentMessage{ID: "legacy", Kind: agent.KindCommand, Command: &agent.CommandMessage{Name: "registered-tool", Content: json.RawMessage(`{"n":1}`)}}
	got, err := agent.ConvertToLLM([]agent.AgentMessage{legacy})
	if err != nil || !reflect.DeepEqual(got, []*schema.AgenticMessage{schema.UserAgenticMessage("registered-tool")}) {
		t.Fatal("legacy non-shell projection changed", err)
	}
}
