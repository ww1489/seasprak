package consumer_test

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ww1489/seasprak/sdk"
)

func TestSDKConsumerShellOutputLimitDoesNotCreateApproval(t *testing.T) {
	command := "i=0; while [ $i -lt 3000 ]; do echo fixture-output-0123456789; i=$((i+1)); done"
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "cmd.exe"
		command = "for /L %i in (1,1,3000) do @echo fixture-output-0123456789"
	}
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("required host shell unavailable: %v", err)
	}
	model := &fakeModel{}
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-shell-limit", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	result, err := s.ExecuteCommand(t.Context(), sdk.CommandRequest{Command: command, Timeout: 10 * time.Second})
	if err != nil || !result.Started || !result.Terminated || result.ExitCode != 0 || !result.Truncated || result.OutputIncomplete {
		t.Fatalf("shell output contract: result=%+v err=%v", result, err)
	}
	limits := sdk.DefaultOutputLimits()
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(result.Output, "\r\n", "\n"), "\n"), "\n")
	if len(result.Output) > limits.MaxBytes || len(lines) > limits.MaxLines || !utf8.ValidString(result.Output) {
		t.Fatalf("preview exceeds public limits: bytes=%d lines=%d", len(result.Output), len(lines))
	}
	if result.Artifact.Available || result.LogError == "" {
		t.Fatal("missing artifact backend must report unavailable full log without changing shell success")
	}
	snapshot, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Calls) != 0 || len(snapshot.Approvals) != 0 || len(snapshot.Interactions) != 0 || len(snapshot.FrozenExecutions) != 0 {
		t.Fatal("host shell entered model tool approval or execution pipeline")
	}
	for _, trace := range snapshot.Traces {
		if trace.Usage.ToolExecutions != 0 || trace.CheckpointID != "" {
			t.Fatal("host shell charged model budget or created a resumable checkpoint")
		}
	}
	model.mu.Lock()
	defer model.mu.Unlock()
	if model.calls != 0 {
		t.Fatal("host shell invoked model")
	}
}
