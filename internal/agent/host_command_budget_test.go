package agent_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestHostShellFinalModelTextBudget(t *testing.T) {
	for _, tc := range []struct {
		name, command, output, artifact string
		exit                            int
		cancel, timeout, incomplete     bool
	}{
		{name: "full-output", command: "echo full", output: strings.Repeat("x", 50<<10)},
		{name: "long-command", command: strings.Repeat("echo x;", 12000), output: "tail"},
		{name: "utf8-lines", command: strings.Repeat("命令😀😀\n", 2100), output: strings.Repeat("输出😀\n", 2000), exit: 7, incomplete: true},
		{name: "full-lines", command: "echo full", output: strings.Repeat("x\n", 1999) + "last", exit: 9},
		{name: "cancel", command: strings.Repeat("命令", 20000), output: strings.Repeat("输出😀", 8000), cancel: true, incomplete: true, artifact: "saved-log"},
		{name: "timeout", command: "slow", output: strings.Repeat("x", 50<<10), timeout: true, incomplete: true, artifact: "saved-log"},
		{name: "huge-reference", command: "echo hi", output: "output", artifact: "unique-reference-" + strings.Repeat("id", 40<<10), exit: 7, incomplete: true},
		{name: "multiline-reference", command: "echo hi", output: "output", artifact: "unique-reference-" + strings.Repeat("id\n", 2500), exit: 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := agent.HostShellResult{Output: tc.output, Started: true, Terminated: true, ExitCode: tc.exit, Cancelled: tc.cancel, TimedOut: tc.timeout, OutputIncomplete: tc.incomplete}
			if tc.artifact != "" {
				result.Truncated = true
				result.Artifact = agent.ArtifactRef{ID: tc.artifact, Available: true}
			}
			content, _ := json.Marshal(map[string]string{"command": tc.command})
			body, _ := json.Marshal(result)
			msg := agent.AgentMessage{ID: "shell", Kind: agent.KindCommand, Status: agent.StatusComplete, Command: &agent.CommandMessage{Name: "shell", Content: content, Result: body}}
			original, _ := json.Marshal(msg)
			got, err := agent.ConvertToLLM([]agent.AgentMessage{msg})
			if err != nil || len(got) != 1 {
				t.Fatal("projection failed", err)
			}
			text := got[0].ContentBlocks[0].UserInputText.Text
			if len(text) > 50<<10 || strings.Count(text, "\n")+1 > 2000 || !utf8.ValidString(text) {
				t.Errorf("final model text exceeds budget: bytes=%d lines=%d", len(text), strings.Count(text, "\n")+1)
			}
			if !strings.Contains(text, "truncated") || !strings.Contains(text, "limit") || !strings.Contains(text, "head/tail") || !strings.Contains(text, "partial lines") || !strings.Contains(text, "session history") {
				t.Error("model truncation lacks its limit, retained fragments, partial-line warning or original-result location")
			}
			if tc.name == "full-output" && !strings.Contains(text, "byte limit") || tc.name == "full-lines" && !strings.Contains(text, "line limit") || tc.name == "utf8-lines" && !strings.Contains(text, "byte and line limits") {
				t.Error("model truncation does not identify the exceeded dimension")
			}
			for required, needed := range map[string]bool{"(command cancelled)": tc.cancel, "(command timed out)": tc.timeout, "[Output collection incomplete.]": tc.incomplete, "Command exited with code 7": tc.exit == 7, "Command exited with code 9": tc.exit == 9} {
				if needed && !strings.Contains(text, required) {
					t.Errorf("lost execution status %q", required)
				}
			}
			if strings.HasPrefix(tc.artifact, "unique-reference-") {
				if strings.Contains(text, "unique-reference-") || !strings.Contains(text, "reference omitted from model display") || strings.Contains(text, "Full output unavailable") {
					t.Error("oversized reference was cut, or omission misrepresented storage")
				}
			} else if tc.artifact != "" && !strings.Contains(text, "Full output artifact: "+tc.artifact+"]") {
				t.Error("fitting reference lost")
			}
			after, _ := json.Marshal(msg)
			if !reflect.DeepEqual(original, after) {
				t.Fatal("projection mutated original result")
			}
			var replay agent.AgentMessage
			if err := json.Unmarshal(original, &replay); err != nil {
				t.Fatal(err)
			}
			recovered, err := agent.ConvertToLLM([]agent.AgentMessage{replay})
			if err != nil || !reflect.DeepEqual(got, recovered) {
				t.Fatal("recovery projection differs", err)
			}
			msg.Command.ExcludeFromContext = true
			if excluded, err := agent.ConvertToLLM([]agent.AgentMessage{msg}); err != nil || len(excluded) != 0 {
				t.Fatal("excluded command entered context")
			}
		})
	}
}
