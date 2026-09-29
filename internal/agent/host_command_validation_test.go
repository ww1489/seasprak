package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestHostShellCommandRejectsInvalidFacts(t *testing.T) {
	for _, result := range []string{
		`{}`, `null`,
		`{"output":"","exitCode":0,"terminated":true}`,
		`{"output":"","exitCode":0,"started":true}`,
		`{"output":"","started":true,"terminated":true}`,
		`{"exitCode":0,"started":true,"terminated":true}`,
		`{"output":"","exitCode":0,"started":false,"terminated":true}`,
		`{"output":"","exitCode":0,"started":false,"terminated":false}`,
		`{"output":"","exitCode":-2,"started":true,"terminated":true}`,
		`{"output":"","exitCode":0,"started":true,"terminated":true,"duration":-1}`,
		`{"output":"","exitCode":0,"started":true,"terminated":true,"cancelled":true,"timedOut":true}`,
		`{"output":null,"exitCode":0,"started":true,"terminated":true}`,
		`{"output":"","exitCode":0,"started":"true","terminated":true}`,
	} {
		t.Run(result, func(t *testing.T) {
			cmd := agent.CommandMessage{Name: "shell", Content: json.RawMessage(`{"command":"echo hi"}`), Result: json.RawMessage(result)}
			if _, _, err := agent.DecodeHostShellCommand(cmd); err == nil {
				t.Fatal("malformed execution facts accepted")
			}
			if _, err := agent.ConvertToLLM([]agent.AgentMessage{{Kind: agent.KindCommand, Command: &cmd}}); err == nil {
				t.Fatal("malformed execution facts entered model context")
			}
		})
	}
}
