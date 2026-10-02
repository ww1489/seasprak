package codeagent

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/testkit"
)

// The child acknowledges actual process start and waits on a socket. Lifecycle
// ordering is controlled by this handshake and model gates, never by sleeps.
func blockedContextShell(t *testing.T, s *AgentSession, exclude bool) (release func(), finish func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second))
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if goruntime.GOOS == "windows" {
		quoted = "\"" + binary + "\""
	}
	request := CommandRequest{ExcludeFromContext: exclude, Command: shellScript(t, quoted, quoted) + " -test.run=^TestP2CommandCheckpointChild$ -- checkpoint-shell-child " + listener.Addr().String(), Timeout: 25 * time.Second}
	type result struct {
		out CommandResult
		err error
	}
	done := make(chan result, 1)
	go func() { out, err := s.ExecuteCommand(t.Context(), request); done <- result{out, err} }()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	var started [1]byte
	if _, err := conn.Read(started[:]); err != nil {
		t.Fatal(err)
	}
	if shellRuns(t, s.rt.opts.Workspace) != 1 {
		t.Fatal("shell has not started")
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := conn.Write([]byte{1}); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	finish = func() {
		t.Helper()
		select {
		case r := <-done:
			if r.err != nil || !r.out.Started || !r.out.Terminated || r.out.ExitCode != 0 {
				t.Fatalf("shell did not complete: %+v %v", r.out, r.err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("shell did not exit")
		}
	}
	return release, finish
}

func TestP2CommandOrdinaryCheckpointDefersContext(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "include", true: "exclude"}[exclude], func(t *testing.T) {
			gate := make(chan struct{})
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "work-call", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}}
			var runs atomic.Int32
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, GenerationFingerprint: "ordinary-command-checkpoint-v1", Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "tool result", nil }}}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			release, finish := blockedContextShell(t, s, exclude)
			paused := make(chan error, 1)
			go func() { _, err := s.Pause(t.Context(), first.TraceID); paused <- err }()
			waitResumeCondition(t, func() bool {
				for _, op := range s.rt.manager.View().Operations {
					if op.Kind == "pause" {
						return true
					}
				}
				return false
			})
			if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
			close(gate)
			select {
			case err := <-paused:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ordinary pause did not complete")
			}
			before := s.rt.manager.View()
			cp := before.Checkpoints[before.Traces[first.TraceID].CheckpointID]
			if before.Traces[first.TraceID].State != "paused" || cp.ID == "" || len(cp.ApprovalTargets) != 0 || len(cp.InteractionIDs) != 0 || runs.Load() != 0 {
				t.Fatal("fixture is not an ordinary checkpoint")
			}
			release()
			finish()
			after := s.rt.manager.View()
			if after.LeafID != before.LeafID || !reflect.DeepEqual(after.Messages, before.Messages) || !reflect.DeepEqual(after.Checkpoints, before.Checkpoints) || len(after.HostCommandConsumptions) != 0 {
				t.Fatal("shell completion changed ordinary checkpoint history")
			}
			visible := approvalSnapshot(t, s).Messages
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close(context.Background()) })
			if !reflect.DeepEqual(visible, approvalSnapshot(t, reopened).Messages) || model.Calls() != 1 || runs.Load() != 0 || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("open changed facts or reran work")
			}
			resume := ResumeCommand{TraceID: first.TraceID, ExpectedRevision: reopened.rt.manager.View().LastSeq, IdempotencyKey: "ordinary-resume"}
			receipt, err := reopened.Resume(t.Context(), resume)
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(reopened.rt.manager.View().Traces[first.TraceID].State) })
			_ = approvalSnapshot(t, reopened)
			history, err := agent.ConvertToLLM(reopened.rt.manager.View().Messages)
			if err != nil {
				t.Fatal(err)
			}
			requests := model.inputs()
			if len(history) < 3 || len(requests) != 2 || !reflect.DeepEqual(commandConversation(requests[1]), history[:3]) || runs.Load() != 1 {
				t.Fatal("ordinary Resume included pending shell or changed original tool exchange")
			}
			if retry, err := reopened.Resume(t.Context(), resume); err != nil || retry != receipt || model.Calls() != 2 || runs.Load() != 1 {
				t.Fatal("duplicate Resume repeated work", err)
			}
			next, err := reopened.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(reopened.rt.manager.View().Traces[next.TraceID].State) })
			requests = model.inputs()
			want := append(history, schema.UserAgenticMessage("next"))
			if len(requests) != 3 || !reflect.DeepEqual(commandConversation(requests[2]), want) || shellRuns(t, opts.Workspace) != 1 || runs.Load() != 1 {
				t.Fatal("next input lost ordered command context or repeated execution")
			}
			view := reopened.rt.manager.View()
			count := 0
			for _, msg := range view.Messages {
				if msg.Kind == agent.KindCommand {
					count++
				}
			}
			expected := 1
			if exclude {
				expected = 0
			}
			if count != expected || len(view.HostCommandConsumptions) != 1 {
				t.Fatal("ordinary resume command exclusion differs")
			}
		})
	}
}

func TestP2CommandCompletesDuringSecondIndependentTrace(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(map[bool]string{false: "include", true: "exclude"}[exclude], func(t *testing.T) {
			firstGate, secondGate := make(chan struct{}), make(chan struct{})
			model := &commandContextModel{versionedPauseModel: versionedPauseModel{testkit.NewFake(testkit.Step{Gate: firstGate, Text: "first done"}, testkit.Step{Gate: secondGate, Text: "second done"}, testkit.Step{Text: "third done"})}}
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Model: model}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			first, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			release, finish := blockedContextShell(t, s, exclude)
			second, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"second"}`)})
			if err != nil {
				t.Fatal(err)
			}
			close(firstGate)
			waitResumeCondition(t, func() bool { return model.Calls() == 2 })
			before := s.rt.manager.View()
			if before.Traces[first.TraceID].State != "completed" || before.Traces[second.TraceID].State != "running" || len(before.HostCommands) != 0 {
				t.Fatal("shell did not span two independent Traces")
			}
			expectedSecond, err := agent.ConvertToLLM(before.Messages)
			if err != nil {
				t.Fatal(err)
			}
			release()
			finish()
			after := s.rt.manager.View()
			if after.LeafID != before.LeafID || !reflect.DeepEqual(after.Messages, before.Messages) || len(after.HostCommands) != 1 || len(after.HostCommandConsumptions) != 0 || !reflect.DeepEqual(commandConversation(model.inputs()[1]), expectedSecond) {
				t.Fatal("late shell result changed second active Trace")
			}
			close(secondGate)
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[second.TraceID].State) })
			_ = approvalSnapshot(t, s)
			history, err := agent.ConvertToLLM(s.rt.manager.View().Messages)
			if err != nil {
				t.Fatal(err)
			}
			third, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"third"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(s.rt.manager.View().Traces[third.TraceID].State) })
			requests := model.inputs()
			if len(requests) != 3 || !reflect.DeepEqual(commandConversation(requests[2]), append(history, schema.UserAgenticMessage("third"))) || shellRuns(t, opts.Workspace) != 1 {
				t.Fatal("third input lost ordered late result or repeated shell")
			}
			view := s.rt.manager.View()
			count := 0
			for _, msg := range view.Messages {
				if msg.Kind == agent.KindCommand {
					count++
				}
			}
			expected := 1
			if exclude {
				expected = 0
			}
			if count != expected || len(view.HostCommandConsumptions) != 1 {
				t.Fatal("late command exclusion or consumption differs")
			}
		})
	}
}
