package codeagent

import (
	"context"
	"encoding/json"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestP2CommandShellWindowsPowerShellResults(t *testing.T) {
	for _, shell := range []string{"powershell", "pwsh"} {
		t.Run(shell, func(t *testing.T) {
			if _, err := exec.LookPath(shell + ".exe"); err != nil {
				t.Skipf("required shell unavailable: %s", shell)
			}
			for _, tc := range []struct {
				name    string
				command string
				code    int
				output  []string
			}{
				{"explicit-nonzero", "[Console]::WriteLine('explicit-output'); exit 9", 9, []string{"explicit-output"}},
				{"native-nonzero", "[Console]::WriteLine('native-output'); cmd /c exit 9", 9, []string{"native-output"}},
				{"cmdlet-error", "Get-Item -LiteralPath 'missing-fixture-file'", 1, []string{"missing-fixture-file"}},
				{"terminating-cmdlet-error", "Write-Error 'terminating-fixture-error' -ErrorAction Stop", 1, []string{"terminating-fixture-error"}},
				{"native-success-then-cmdlet-error", "cmd /c exit 0; Write-Error 'mixed-fixture-error'", 1, []string{"mixed-fixture-error"}},
				{"success", "[Console]::WriteLine('success-output'); cmd /c exit 0", 0, []string{"success-output"}},
				{"utf8", "[Console]::WriteLine('中文😀'); [Console]::Error.WriteLine('错误😀')", 0, []string{"中文😀", "错误😀"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s, opts, model := newShellSession(t, t.TempDir())
					// ExecuteCommand owns the real process Start and Wait. The file
					// records actual execution, independently of stored history.
					request := CommandRequest{Shell: shell, Command: "[System.IO.File]::AppendAllText('runs.txt', \"run`n\"); " + tc.command}
					out, err := s.ExecuteCommand(t.Context(), request)
					if err != nil || !out.Started || !out.Terminated || out.ExitCode != tc.code || out.Cancelled || out.TimedOut || out.OutputIncomplete || out.Truncated {
						t.Fatalf("shell result=%+v err=%v; want exit=%d", out, err, tc.code)
					}
					if !utf8.ValidString(out.Output) {
						t.Fatalf("shell output is not UTF-8: %q", out.Output)
					}
					for _, want := range tc.output {
						if !strings.Contains(out.Output, want) {
							t.Fatalf("shell output=%q; missing %q", out.Output, want)
						}
					}
					check := func(session *AgentSession) {
						t.Helper()
						view := session.rt.manager.View()
						if len(view.Messages) != 1 || len(view.HostCommands) != 1 || shellRuns(t, opts.Workspace) != 1 || model.Calls() != 0 {
							t.Fatal("command was repeated, lost, or entered model execution")
						}
						msg := view.Messages[0]
						record, ok := view.HostCommands[msg.ID]
						if !ok || record.CommitSeq == 0 || view.HostCommandConsumptions[msg.ID] == 0 || msg.Command == nil || record.Message.Command == nil {
							t.Fatal("completed command history or consumption missing")
						}
						for _, body := range []json.RawMessage{msg.Command.Result, record.Message.Command.Result} {
							var saved CommandResult
							if err := json.Unmarshal(body, &saved); err != nil || !reflect.DeepEqual(saved, out) {
								t.Fatalf("persisted result=%+v differs from actual=%+v; err=%v", saved, out, err)
							}
						}
					}
					check(s)
					if err := s.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					reopened, err := OpenAgentSession(t.Context(), opts)
					if err != nil {
						t.Fatal(err)
					}
					defer reopened.Close(context.Background())
					check(reopened)
				})
			}
		})
	}
}
