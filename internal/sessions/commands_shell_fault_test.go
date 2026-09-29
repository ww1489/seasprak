package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type shellHistoryFaultStore struct {
	store.Store
	phase  string
	lost   bool
	writes atomic.Int32
}

func (s *shellHistoryFaultStore) Close() error { return nil }
func (s *shellHistoryFaultStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	phase := s.phase
	if phase == "" {
		phase = "host_command_result"
	}
	for _, entry := range commit.ControlRecords {
		if entry.Type != phase {
			continue
		}
		s.writes.Add(1)
		if s.lost {
			if _, err := s.Store.Append(ctx, id, expected, commit); err != nil {
				return store.CommitReceipt{}, err
			}
		}
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "injected command history failure")
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestP2CommandShellHistoryFailureDoesNotRetry(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{true: "lost-ack", false: "rejected"}[lost], func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			faults := &shellHistoryFaultStore{Store: backend, lost: lost}
			opts := Options{SessionID: id, Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Store: faults, Model: testkit.NewFake()}
			manager, err := state.NewManager(faults, id)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "shell-history-test")
			if err != nil {
				t.Fatal(err)
			}
			request := CommandRequest{Command: shellScript(t, "echo run >>runs.txt; printf 'retained output'; exit 7", "echo run>>runs.txt & echo retained output & exit /b 7")}
			out, err := s.ExecuteCommand(t.Context(), request)
			requireSessionCode(t, err, product.CodeStorageUnavailable)
			if !out.Started || !out.Terminated || out.ExitCode != 7 || !strings.Contains(out.Output, "retained output") || shellRuns(t, opts.Workspace) != 1 || faults.writes.Load() != 1 {
				t.Fatalf("history failure changed execution: %+v writes=%d", out, faults.writes.Load())
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager, err = state.NewManager(faults, id)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Start(opts, manager, "shell-history-test")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			wantMessages := 0
			if lost {
				wantMessages = 1
			}
			if len(approvalSnapshot(t, reopened).Messages) != wantMessages || len(manager.View().Messages) != 0 || shellRuns(t, opts.Workspace) != 1 || faults.writes.Load() != 1 {
				t.Fatal("replay retried command or fabricated saved history")
			}
		})
	}
}

func TestP2CommandShellStartFailureAndPreCancellation(t *testing.T) {
	s, opts, _ := newShellSession(t, "memory")
	out, err := s.ExecuteCommand(t.Context(), CommandRequest{Command: "echo must-not-run", Cwd: filepath.Join(opts.Workspace, "absent")})
	requireSessionCode(t, err, product.CodeResourceUnavailable)
	if out.Started || out.Terminated || out.ExitCode != -1 {
		t.Fatalf("start failure fabricated termination: %+v", out)
	}
	before := s.rt.manager.View().LastSeq
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = s.ExecuteCommand(ctx, CommandRequest{Command: "echo must-not-run"})
	if !errors.Is(err, context.Canceled) || s.rt.manager.View().LastSeq != before {
		t.Fatal("pre-cancelled command executed or wrote history")
	}
}

func TestP2CommandShellRejectsInputOriginSpoofing(t *testing.T) {
	s, _, model := newShellSession(t, "memory")
	before := s.rt.manager.View().LastSeq
	_, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "command", Content: json.RawMessage(`{"command":"echo must-not-run","origin":"user","user":true}`)})
	requireSessionCode(t, err, product.CodeUnsupportedCapability)
	if s.rt.manager.View().LastSeq != before || model.Calls() != 0 {
		t.Fatal("untrusted command input acquired host execution")
	}
}

func TestP2CommandShellRelativeCwdAndConfiguredTimeout(t *testing.T) {
	s, opts, _ := newShellSession(t, "memory")
	child := filepath.Join(opts.Workspace, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	request := CommandRequest{Cwd: "child", Command: shellScript(t, "echo run >>runs.txt", "echo run>>runs.txt")}
	out, err := s.ExecuteCommand(t.Context(), request)
	if err != nil || out.ExitCode != 0 || shellRuns(t, child) != 1 {
		t.Fatalf("relative cwd=%+v err=%v", out, err)
	}
	// The immutable session limit is set before admitting another command.
	if err := s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Limits.ToolTimeout = 50 * time.Millisecond; return nil }); err != nil {
		t.Fatal(err)
	}
	out, err = s.ExecuteCommand(t.Context(), CommandRequest{Command: shellScript(t, "while :; do :; done", "for /L %i in (1,1,2147483647) do @rem")})
	if err != nil || !out.TimedOut || !out.Terminated || out.Duration > 5*time.Second {
		t.Fatalf("configured timeout ignored: %+v err=%v", out, err)
	}
}

func TestP2CommandShellConcurrentExplicitCalls(t *testing.T) {
	s, opts, _ := newShellSession(t, "memory")
	// Distinct files avoid imposing a filesystem append atomicity requirement.
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			command := shellScript(t, "echo run >", "echo run >") + string(rune('a'+i)) + ".txt"
			out, err := s.ExecuteCommand(t.Context(), CommandRequest{Command: command})
			if err != nil || out.ExitCode != 0 || !out.Terminated {
				t.Errorf("concurrent explicit command=%+v err=%v", out, err)
			}
		})
	}
	wg.Wait()
	for i := range 4 {
		if _, err := os.Stat(filepath.Join(opts.Workspace, string(rune('a'+i))+".txt")); err != nil {
			t.Fatal(err)
		}
	}
	view := s.rt.manager.View()
	if len(view.Messages) != 4 {
		t.Fatal("concurrent explicit calls were lost")
	}
	display := approvalSnapshot(t, s).Messages
	var last uint64
	for i, msg := range view.Messages {
		result := view.HostCommands[msg.ID]
		if result.CommitSeq <= last || display[i].ID != msg.ID || view.HostCommandConsumptions[msg.ID] == 0 {
			t.Fatal("concurrent completion order or unique consumption changed")
		}
		last = result.CommitSeq
	}
}

func TestP2CommandShellExplicitShellExitAndOutput(t *testing.T) {
	s, _, _ := newShellSession(t, "memory")
	shell, command := "sh", "printf '中文😀\\n'; printf 'stderr\\n' >&2; exit 9"
	if goruntime.GOOS == "windows" {
		shell, command = "powershell", "[Console]::WriteLine('中文😀'); [Console]::Error.WriteLine('stderr'); exit 9"
	}
	out, err := s.ExecuteCommand(t.Context(), CommandRequest{Shell: shell, Command: command})
	if err != nil || !out.Terminated || out.ExitCode != 9 || !strings.Contains(out.Output, "中文😀") || !strings.Contains(out.Output, "stderr") {
		t.Fatalf("explicit shell result=%+v err=%v", out, err)
	}
	if goruntime.GOOS == "windows" {
		out, err = s.ExecuteCommand(t.Context(), CommandRequest{Shell: shell, Command: "cmd /c exit 9"})
		if err != nil || out.ExitCode != 9 || !out.Terminated {
			t.Fatalf("PowerShell native exit code changed: %+v err=%v", out, err)
		}
		out, err = s.ExecuteCommand(t.Context(), CommandRequest{Shell: shell, Command: "Get-Item -LiteralPath 'missing-fixture-file'"})
		if err != nil || out.ExitCode == 0 || !out.Terminated {
			t.Fatalf("PowerShell failure hidden: %+v err=%v", out, err)
		}
		out, err = s.ExecuteCommand(t.Context(), CommandRequest{Shell: shell, Command: "cmd /c exit 0; Write-Error 'synthetic-review'"})
		if err != nil || out.ExitCode == 0 || !out.Terminated {
			t.Fatalf("PowerShell mixed-command failure hidden: %+v err=%v", out, err)
		}
	}
}
