package codeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

func shellLogText(mode string) string {
	switch mode {
	case "short":
		return "HEAD 中文😀 fixture-private-value TAIL"
	case "line-limit":
		return "HEAD fixture-private-value\n" + strings.Repeat("中文😀\n", 2100) + "TAIL"
	case "bytes":
		return "HEAD fixture-private-value " + strings.Repeat("中文😀", 6000) + " TAIL"
	default:
		return "HEAD fixture-private-value\n" + strings.Repeat("中文😀 fixed fixture output\n", 2100) + "TAIL"
	}
}

// The real host shell launches the test binary, which emits deterministic UTF-8
// without relying on platform-specific echo encodings or installed scripting tools.
func TestP2CommandLogChild(t *testing.T) {
	args := os.Args
	if len(args) < 4 || args[len(args)-3] != "shell-log-child" {
		return
	}
	f, err := os.OpenFile("runs.txt", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(91)
	}
	_, err = f.WriteString("run\n")
	if closeErr := f.Close(); err != nil || closeErr != nil {
		os.Exit(92)
	}
	fmt.Print(shellLogText(args[len(args)-2]))
	code, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		os.Exit(93)
	}
	os.Exit(code)
}

func shellLogRequest(t *testing.T, mode string, exit int) CommandRequest {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if goruntime.GOOS == "windows" {
		quoted = "\"" + binary + "\""
	}
	// Also verifies availability of the platform shell.
	command := shellScript(t, quoted, quoted) + " -test.run=^TestP2CommandLogChild$ -- shell-log-child " + mode + " " + strconv.Itoa(exit)
	return CommandRequest{Command: command, Timeout: 15 * time.Second}
}

type shellLogStore struct {
	*fixture.Memory
	fail   bool
	saves  atomic.Int32
	inputs []agent.OutputArtifactInput
}

func (s *shellLogStore) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	s.saves.Add(1)
	s.inputs = append(s.inputs, in)
	if s.fail {
		return agent.ArtifactRef{}, errors.New("private storage diagnostics")
	}
	return s.Memory.SaveOutput(ctx, in)
}

func shellLogRedactor(_ context.Context, text string) (string, error) {
	return strings.ReplaceAll(text, "fixture-private-value", "[redacted]"), nil
}

// Decode through the public wire fields so the initial red test exercises the
// missing runtime behavior rather than failing to compile on new struct fields.
type shellLogResult struct {
	Output    string            `json:"output"`
	Truncated bool              `json:"truncated"`
	Artifact  agent.ArtifactRef `json:"artifact"`
	LogError  string            `json:"logError"`
}

func decodeShellLog(t *testing.T, out CommandResult) shellLogResult {
	t.Helper()
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var got shellLogResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestP2CommandLogs(t *testing.T) {
	for _, tc := range []struct {
		name, mode, redaction string
		fail, noStore         bool
		exit                  int
	}{
		{name: "long-lines", mode: "lines"},
		{name: "line-limit-only", mode: "line-limit"},
		{name: "long-single-line", mode: "bytes"},
		{name: "save-failed-nonzero", mode: "lines", fail: true, exit: 7},
		{name: "redactor-failed-nonzero", mode: "lines", redaction: "error", exit: 9},
		{name: "redactor-panicked", mode: "lines", redaction: "panic"},
		{name: "redactor-missing", mode: "lines", redaction: "missing"},
		{name: "store-missing", mode: "lines", noStore: true},
		{name: "short", mode: "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifacts := &shellLogStore{Memory: fixture.NewMemory(), fail: tc.fail}
			ops := tools.Operations{Artifacts: artifacts, OutputRedactor: shellLogRedactor}
			switch tc.redaction {
			case "error":
				ops.OutputRedactor = func(context.Context, string) (string, error) { return "", errors.New("private redactor diagnostics") }
			case "panic":
				ops.OutputRedactor = func(context.Context, string) (string, error) { panic("private redactor diagnostics") }
			case "missing":
				ops.OutputRedactor = nil
			}
			if tc.noStore {
				ops.Artifacts = nil
			}
			model := testkit.NewFake()
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model, Operations: ops, ResourceEnvironment: "model-container-not-host"}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			for i := 1; i <= 2; i++ {
				out, err := s.ExecuteCommand(t.Context(), shellLogRequest(t, tc.mode, tc.exit))
				if err != nil || !out.Started || !out.Terminated || out.ExitCode != tc.exit || out.TimedOut || out.Cancelled || out.OutputIncomplete || out.Duration <= 0 || shellRuns(t, opts.Workspace) != i {
					t.Fatalf("execution facts changed: exit=%d started=%v terminated=%v err=%v", out.ExitCode, out.Started, out.Terminated, err)
				}
				got := decodeShellLog(t, out)
				if got.Truncated != (tc.mode != "short") || len(got.Output) > 50<<10 || strings.Count(got.Output, "\n")+1 > 2000 || !utf8.ValidString(got.Output) || strings.Contains(got.Output, "fixture-private-value") {
					t.Errorf("unsafe or unbounded output: truncated=%v bytes=%d", got.Truncated, len(got.Output))
				}
				wantSaves := i
				if tc.redaction != "" || tc.noStore || tc.mode == "short" {
					wantSaves = 0
				}
				if artifacts.saves.Load() != int32(wantSaves) || artifacts.Calls("save") != 0 {
					t.Errorf("saves=%d want=%d legacy=%d", artifacts.saves.Load(), wantSaves, artifacts.Calls("save"))
				}
				failedLog := tc.fail || tc.noStore || tc.redaction != ""
				if failedLog {
					if !strings.HasPrefix(got.LogError, product.CodeResourceUnavailable+":") || got.Artifact != (agent.ArtifactRef{}) {
						t.Error("log failure hidden or reference published")
					}
				} else if got.LogError != "" {
					t.Error("unexpected log error")
				}
				if tc.redaction == "" && (!strings.HasPrefix(got.Output, "HEAD") || !strings.HasSuffix(got.Output, "TAIL")) {
					t.Error("head/tail preview lost")
				}
				view := s.rt.manager.View()
				if len(view.Messages) != i || len(view.Calls) != 0 || len(view.ToolProjections) != 0 || len(view.Approvals) != 0 || len(view.ApprovalClaims) != 0 || len(view.Traces) != 0 || len(view.Turns) != 0 || len(view.Operations) != 0 || model.Calls() != 0 {
					t.Fatal("shell entered model execution or lost history")
				}
				msg := view.Messages[i-1]
				var saved CommandResult
				if msg.Kind != agent.KindCommand || msg.Scope.ToolCallID != "" || json.Unmarshal(msg.Command.Result, &saved) != nil || saved != out {
					t.Error("single history entry lost actual result")
				}
				if tc.mode != "short" && !failedLog {
					binding := agent.OutputArtifactBinding{SessionID: opts.SessionID, Environment: "host", CallID: msg.ID}
					if got.Artifact.ID == "" || artifacts.inputs[i-1].Binding != binding {
						t.Error("missing artifact or incorrect host/history binding")
						continue
					}
					reader, err := artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: binding, Ref: got.Artifact})
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(reader)
					_ = reader.Close()
					want, _ := shellLogRedactor(t.Context(), shellLogText(tc.mode))
					if err != nil || string(body) != want {
						t.Error("full redacted output unreadable")
					}
					binding.CallID = agent.MustID()
					reader, err = artifacts.OpenOutput(t.Context(), agent.OutputArtifactRead{Binding: binding, Ref: got.Artifact})
					requireSessionCode(t, err, product.CodePermissionDenied)
					if reader != nil {
						t.Error("wrong command binding returned content")
					}
				} else if got.Artifact != (agent.ArtifactRef{}) {
					t.Error("short/failed log published artifact")
				}
			}
			before := artifacts.saves.Load()
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			if shellRuns(t, opts.Workspace) != 2 || artifacts.saves.Load() != before || len(reopened.rt.manager.View().Messages) != 2 {
				t.Error("opening history repeated command or log save")
			}
		})
	}
}

func TestP2CommandLogsShortWithoutRedactor(t *testing.T) {
	artifacts := &shellLogStore{Memory: fixture.NewMemory()}
	s, err := CreateAgentSession(t.Context(), Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: "memory", Profile: ProfileMemory, Model: testkit.NewFake(), Operations: tools.Operations{Artifacts: artifacts}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	out, err := s.ExecuteCommand(t.Context(), shellLogRequest(t, "short", 0))
	if err != nil || out.Output != shellLogText("short") || out.Truncated || out.OutputIncomplete || out.Artifact != (agent.ArtifactRef{}) || out.LogError != "" || artifacts.saves.Load() != 0 || artifacts.Calls("save") != 0 {
		t.Fatal("trusted short output was changed or saved", err)
	}
}

func TestP2CommandLogsHistoryFailure(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(strconv.FormatBool(lost), func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			faults := &shellHistoryFaultStore{Store: backend, lost: lost}
			artifacts := &shellLogStore{Memory: fixture.NewMemory()}
			opts := Options{SessionID: id, Workspace: t.TempDir(), Profile: ProfileMemory, StateRoot: "memory", Store: faults, Model: testkit.NewFake(), Operations: tools.Operations{Artifacts: artifacts, OutputRedactor: shellLogRedactor}}
			manager, err := state.NewManager(faults, id)
			if err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "shell-log-history-test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			out, err := s.ExecuteCommand(t.Context(), shellLogRequest(t, "lines", 7))
			requireSessionCode(t, err, product.CodeStorageUnavailable)
			got := decodeShellLog(t, out)
			if !out.Started || !out.Terminated || out.ExitCode != 7 || !got.Truncated || got.Artifact != (agent.ArtifactRef{}) || got.LogError == "" || artifacts.saves.Load() != 1 || faults.writes.Load() != 1 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("failed history lost facts, leaked reference or retried execution")
			}
			if !strings.HasPrefix(out.Output, "HEAD") || !strings.HasSuffix(out.Output, "TAIL") || strings.Contains(out.Output, "fixture-private-value") {
				t.Fatal("failed history lost safe preview")
			}
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			manager, err = state.NewManager(faults, id)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := Start(opts, manager, "shell-log-history-test")
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close(context.Background())
			want := 0
			if lost {
				want = 1
			}
			if len(approvalSnapshot(t, reopened).Messages) != want || len(manager.View().Messages) != 0 || artifacts.saves.Load() != 1 || faults.writes.Load() != 1 || shellRuns(t, opts.Workspace) != 1 {
				t.Error("history replay repeated execution or storage")
			}
			if _, err := os.Stat(filepath.Join(opts.Workspace, "runs.txt")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
