package codeagent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func shellScript(t *testing.T, unix, windows string) string {
	t.Helper()
	name := "/bin/sh"
	if goruntime.GOOS == "windows" {
		name = "cmd.exe"
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("required host shell unavailable: %v", err)
	}
	if goruntime.GOOS == "windows" {
		return windows
	}
	return unix
}

func newShellSession(t *testing.T, root string) (*AgentSession, Options, *testkit.FakeModel) {
	t.Helper()
	model := testkit.NewFake()
	opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: root, Profile: ProfileMemory, Model: model,
		Policy: &agent.ResolvedPolicy{ApprovalPolicy: "never", SandboxMode: "read-only"},
		Limits: config.Limits{ActivityBudget: time.Nanosecond, TraceToolCalls: 1},
		Tools:  []tools.Definition{builtinDefinitionForSession(t, "execute")}}
	s, err := CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s, opts, model
}

func shellRuns(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "runs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(data)))
}

func TestP2CommandShellExplicitCallsAndHistory(t *testing.T) {
	s, opts, model := newShellSession(t, t.TempDir())
	request := CommandRequest{Command: shellScript(t, "echo run >>runs.txt; printf 'raw output\\n'", "echo run>>runs.txt & echo raw output")}
	for range 2 {
		result, err := s.ExecuteCommand(t.Context(), request)
		if err != nil || !result.Started || !result.Terminated || result.ExitCode != 0 || !strings.Contains(result.Output, "raw output") {
			t.Fatalf("actual shell result=%+v err=%v", result, err)
		}
	}
	if shellRuns(t, opts.Workspace) != 2 {
		t.Fatal("explicit repeated command was deduplicated")
	}
	v := s.rt.manager.View()
	if len(v.Calls) != 0 || len(v.Approvals) != 0 || len(v.ApprovalClaims) != 0 || len(v.FrozenExecutions) != 0 || len(v.Turns) != 0 || len(v.Traces) != 0 || len(v.Inputs) != 0 || len(v.Operations) != 0 || model.Calls() != 0 {
		t.Fatal("user shell entered controlled model execution")
	}
	if len(v.Messages) != 2 {
		t.Fatalf("command history=%d", len(v.Messages))
	}
	for _, msg := range v.Messages {
		if msg.Kind != agent.KindCommand || msg.Standard != nil || msg.Scope.TurnID != "" || msg.Scope.ToolCallID != "" {
			t.Fatal("shell manufactured model tool messages")
		}
		var saved CommandResult
		if err := json.Unmarshal(msg.Command.Result, &saved); err != nil || !saved.Terminated || saved.ExitCode != 0 {
			t.Fatalf("saved result=%+v err=%v", saved, err)
		}
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	if len(opened.rt.manager.View().Messages) != 2 || shellRuns(t, opts.Workspace) != 2 {
		t.Fatal("Open replayed user shell")
	}
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: v.Messages[0].ID, ExpectedRevision: opened.rt.manager.View().LastSeq})
	requireSessionCode(t, err, product.CodeNotFound)
	if shellRuns(t, opts.Workspace) != 2 {
		t.Fatal("Resume executed user shell history")
	}
}

func TestP2CommandShellCwdAndNonzeroExit(t *testing.T) {
	s, _, _ := newShellSession(t, "memory")
	outside := t.TempDir()
	result, err := s.ExecuteCommand(t.Context(), CommandRequest{Cwd: outside, Command: shellScript(t, "echo run >>runs.txt; printf 'before exit\\n'; exit 7", "echo run>>runs.txt & echo before exit & exit /b 7")})
	if err != nil || !result.Started || !result.Terminated || result.ExitCode != 7 || !strings.Contains(result.Output, "before exit") || shellRuns(t, outside) != 1 {
		t.Fatalf("outside workspace result=%+v err=%v", result, err)
	}
}

func TestP2CommandShellTimeoutAndCancellation(t *testing.T) {
	for _, cause := range []string{"timeout", "cancel", "close"} {
		t.Run(cause, func(t *testing.T) {
			s, opts, _ := newShellSession(t, "memory")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := CommandRequest{Command: shellScript(t, "echo started >started.txt; while :; do :; done", "echo started>started.txt & for /L %i in (1,1,2147483647) do @rem"), Timeout: 10 * time.Second}
			if cause == "timeout" {
				request.Timeout = 500 * time.Millisecond
			}
			done := make(chan CommandResult, 1)
			failures := make(chan error, 1)
			go func() { out, err := s.ExecuteCommand(ctx, request); done <- out; failures <- err }()
			waitResumeCondition(t, func() bool { _, err := os.Stat(filepath.Join(opts.Workspace, "started.txt")); return err == nil })
			if cause == "cancel" {
				cancel()
			}
			if cause == "close" {
				if err := s.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case out := <-done:
				if err := <-failures; err != nil {
					t.Fatalf("execution result lost: %v", err)
				}
				if !out.Started || !out.Terminated || out.ExitCode == 0 || out.TimedOut != (cause == "timeout") || out.Cancelled != (cause != "timeout") {
					t.Fatalf("termination was not observed: %+v", out)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shell did not exit after stop request")
			}
			if len(s.rt.manager.View().Messages) != 1 {
				t.Fatal("stopped command history missing")
			}
		})
	}
}

func TestP2CommandShellReadOnlyAndInvalidRequests(t *testing.T) {
	s, opts, _ := newShellSession(t, t.TempDir())
	for _, request := range []CommandRequest{{}, {Command: "echo hi", Timeout: -1}, {Command: "echo hi", Shell: "missing-shell"}} {
		_, err := s.ExecuteCommand(t.Context(), request)
		if err == nil {
			t.Fatal("invalid shell request accepted")
		}
	}
	if len(s.rt.manager.View().Messages) != 0 {
		t.Fatal("invalid input created command history")
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	opts.ReadOnly = true
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	before := opened.rt.manager.View().LastSeq
	_, err = opened.ExecuteCommand(t.Context(), CommandRequest{Command: "echo rejected"})
	requireSessionCode(t, err, product.CodePermissionDenied)
	if opened.rt.manager.View().LastSeq != before {
		t.Fatal("read-only execution wrote history")
	}
}
