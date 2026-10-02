package codeagent

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

func assertApprovalCommandRejected(t *testing.T, f approvalSessionFixture) {
	t.Helper()
	before := f.manager.View()
	snapshot := approvalSnapshot(t, f.s)
	request := CommandRequest{Command: shellScript(t, "echo run >>runs.txt", "echo run>>runs.txt")}
	result, err := f.s.ExecuteCommand(t.Context(), request)
	requireSessionCode(t, err, product.CodeStateConflict)
	if result.Started || result.Terminated {
		t.Fatal("rejected command started a shell")
	}
	if _, err := os.Stat(filepath.Join(f.s.rt.opts.Workspace, "runs.txt")); !os.IsNotExist(err) {
		t.Fatal("rejected command produced a shell side effect", err)
	}
	if after := f.manager.View(); !reflect.DeepEqual(after, before) {
		t.Fatal("rejected command changed history, leaf, revision or execution state")
	}
	after := approvalSnapshot(t, f.s)
	if !reflect.DeepEqual(snapshot, after) || !after.Resume[f.input.TraceID].CanResume || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("rejected command changed approval, eligibility or invocation counts")
	}
}

func TestP2CommandShellPreservesApprovalCheckpoint(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	assertApprovalCommandRejected(t, f)
	answer := answerApproval(t, f, "allowed-once")
	assertApprovalCommandRejected(t, f) // An answer alone does not release the checkpoint.
	answered := approvalSnapshot(t, f.s)
	if answered.Interactions[answer.InteractionID].State != "allowed-once" {
		t.Fatal("approval answer was lost")
	}
	command := ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: answered.Revision, IdempotencyKey: "checkpoint-shell-resume"}
	receipt, err := f.s.Resume(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	final := f.manager.View()
	if final.Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 2 || f.runs.Load() != 1 || final.Traces[f.input.TraceID].Usage.ToolExecutions != 1 || len(final.Calls) != 1 {
		t.Fatalf("Resume lost or duplicated original work: state=%s models=%d tools=%d", final.Traces[f.input.TraceID].State, f.model.Calls(), f.runs.Load())
	}
	retry, err := f.s.Resume(t.Context(), command)
	if err != nil || retry != receipt || f.model.Calls() != 2 || f.runs.Load() != 1 {
		t.Fatal("idempotent Resume repeated work or changed its receipt")
	}
	assertCommandAllowedAfterApproval(t, f)
}

func assertCommandAllowedAfterApproval(t *testing.T, f approvalSessionFixture) {
	t.Helper()
	before := f.manager.View()
	result, err := f.s.ExecuteCommand(t.Context(), CommandRequest{Command: shellScript(t, "echo run >>runs.txt", "echo run>>runs.txt")})
	if err != nil || !result.Started || !result.Terminated || result.ExitCode != 0 || shellRuns(t, f.s.rt.opts.Workspace) != 1 {
		t.Fatalf("settled approval still blocks shell: %+v %v", result, err)
	}
	after := f.manager.View()
	if len(after.Messages) != len(before.Messages)+1 || after.LeafID == before.LeafID {
		t.Fatal("admitted shell no longer participates in model history")
	}
	message := after.Messages[len(after.Messages)-1]
	var saved CommandResult
	if message.Kind != agent.KindCommand || message.Command == nil || json.Unmarshal(message.Command.Result, &saved) != nil || saved != result {
		t.Fatal("admitted shell lost its durable result")
	}
	var request CommandRequest
	if err := json.Unmarshal(message.Command.Content, &request); err != nil {
		t.Fatal(err)
	}
	projected, err := agent.ConvertToLLM([]agent.AgentMessage{message})
	want := "Ran `" + request.Command + "`\n(no output)"
	if result.Output != "" {
		want = "Ran `" + request.Command + "`\n```\n" + result.Output + "\n```"
	}
	if err != nil || len(projected) != 1 || !reflect.DeepEqual(projected[0], schema.UserAgenticMessage(want)) {
		t.Fatal("admitted shell lost command/result context")
	}
}

func TestP2CommandShellApprovalCancelReleasesAdmission(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	assertApprovalCommandRejected(t, f)
	if err := f.s.Cancel(t.Context(), f.input.TraceID); err != nil {
		t.Fatal(err)
	}
	if f.manager.View().Traces[f.input.TraceID].State != "cancelled" || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("Cancel executed approval work or failed to settle the trace")
	}
	assertCommandAllowedAfterApproval(t, f)
}

// The child acknowledges its actual process start, then waits for the parent to
// release it. The socket is only a test barrier, never an execution backend.
func TestP2CommandCheckpointChild(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "checkpoint-shell-child" {
		return
	}
	conn, err := net.DialTimeout("tcp", os.Args[len(os.Args)-1], 10*time.Second)
	if err != nil {
		os.Exit(91)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	if err := os.WriteFile("runs.txt", []byte("run\n"), 0600); err != nil {
		os.Exit(92)
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		os.Exit(93)
	}
	var release [1]byte
	if _, err := conn.Read(release[:]); err != nil {
		os.Exit(94)
	}
	os.Exit(0)
}

func TestP2CommandShellStartedBeforeApprovalCheckpoint(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		for _, reopen := range []bool{false, true} {
			name := map[bool]string{false: "include", true: "exclude"}[exclude] + map[bool]string{false: "/memory", true: "/disk-reopen"}[reopen]
			t.Run(name, func(t *testing.T) { commandAcrossApprovalCheckpoint(t, exclude, reopen) })
		}
	}
}

func commandAcrossApprovalCheckpoint(t *testing.T, exclude, reopen bool) {
	allowApproval := make(chan struct{})
	var once sync.Once
	releaseApproval := func() { once.Do(func() { close(allowApproval) }) }
	hook := func(ctx context.Context, _ agent.FrozenExecution) error {
		select {
		case <-allowApproval:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var f approvalSessionFixture
	var diskOpts Options
	if reopen {
		f, diskOpts = startHostCommandApprovalDisk(t, hook)
	} else {
		f = startApprovalSession(t, hook)
	}
	defer releaseApproval()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(15 * time.Second))
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	if goruntime.GOOS == "windows" {
		quoted = "\"" + binary + "\""
	}
	request := CommandRequest{ExcludeFromContext: exclude, Command: shellScript(t, quoted, quoted) + " -test.run=^TestP2CommandCheckpointChild$ -- checkpoint-shell-child " + listener.Addr().String(), Timeout: 20 * time.Second}
	done := make(chan struct {
		result CommandResult
		err    error
	}, 1)
	go func() {
		result, err := f.s.ExecuteCommand(t.Context(), request)
		done <- struct {
			result CommandResult
			err    error
		}{result, err}
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var started [1]byte
	if _, err := conn.Read(started[:]); err != nil {
		t.Fatal(err)
	}
	if shellRuns(t, f.s.rt.opts.Workspace) != 1 {
		t.Fatal("shell did not actually start before approval")
	}
	releaseApproval()
	waitResumeCondition(t, func() bool { return f.manager.View().Traces[f.input.TraceID].State == "paused" })
	before := approvalSnapshot(t, f.s)
	beforeView := f.manager.View()
	if !before.Resume[f.input.TraceID].CanResume || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("approval checkpoint was not initially resumable")
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.err != nil || !out.result.Started || !out.result.Terminated || out.result.ExitCode != 0 {
			t.Fatalf("previously admitted shell lost its actual result: %+v %v", out.result, out.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shell history did not finish")
	}
	after := approvalSnapshot(t, f.s)
	afterView := f.manager.View()
	if afterView.LeafID != beforeView.LeafID || !reflect.DeepEqual(afterView.Messages, beforeView.Messages) || !reflect.DeepEqual(afterView.Checkpoints, beforeView.Checkpoints) {
		t.Error("pending command changed checkpoint or active model history")
	}
	replayed, replayErr := state.NewManager(f.s.rt.opts.Store, before.SessionID)
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	// Replaying the durable journal must preserve the visible result without
	// executing the process. Snapshot wiring is deliberately tested separately
	// from the model projection; pending commands must not appear in both.
	if replayed.View().LastSeq != afterView.LastSeq || !reflect.DeepEqual(replayed.View().HostCommands, afterView.HostCommands) || shellRuns(t, f.s.rt.opts.Workspace) != 1 {
		t.Fatal("pending command replay lost facts, changed revision or repeated execution")
	}
	if len(afterView.HostCommands) != 1 {
		t.Error("shell completed across approval pause without a durable pending fact")
	}
	if len(after.Messages) != len(before.Messages)+1 || shellRuns(t, f.s.rt.opts.Workspace) != 1 || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("concurrent shell lost history or repeated execution")
	}
	if !after.Resume[f.input.TraceID].CanResume {
		t.Errorf("already started shell invalidated approval checkpoint: %+v", after.Resume[f.input.TraceID])
	}
	if reopen {
		if err := f.s.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		beforeDisk := commandDiskImage(t, diskOpts.StateRoot)
		readOnly := diskOpts
		readOnly.ReadOnly = true
		reader, err := OpenAgentSession(t.Context(), readOnly)
		if err != nil {
			t.Fatal(err)
		}
		readSnapshot := approvalSnapshot(t, reader)
		if !reflect.DeepEqual(readSnapshot.Messages, after.Messages) || !reflect.DeepEqual(reader.rt.manager.View().HostCommands, afterView.HostCommands) {
			t.Fatal("read-only open lost pending shell")
		}
		if err := reader.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(beforeDisk, commandDiskImage(t, diskOpts.StateRoot)) {
			t.Fatal("read-only open changed disk files or metadata")
		}
		opened, err := OpenAgentSession(t.Context(), diskOpts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = opened.Close(context.Background()) })
		f.s, f.manager = opened, opened.rt.manager
		if f.model.Calls() != 1 || f.runs.Load() != 0 || shellRuns(t, diskOpts.Workspace) != 1 || len(f.manager.View().HostCommandConsumptions) != 0 {
			t.Fatal("open repeated work or consumed pending shell")
		}
		resumeForFreshApproval(t, opened, f.input.TraceID)
		beforeRejected := f.manager.View()
		rejected, err := opened.ExecuteCommand(t.Context(), request)
		requireSessionCode(t, err, product.CodeStateConflict)
		if rejected.Started || shellRuns(t, diskOpts.Workspace) != 1 || !reflect.DeepEqual(beforeRejected, f.manager.View()) {
			t.Fatal("reopened approval admitted another shell")
		}
	}
	answerApproval(t, f, "allowed-once")
	captured := &commandContextModel{versionedPauseModel: f.model}
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error { rt.opts.Model = captured; return nil }); err != nil {
		t.Fatal(err)
	}
	resume := ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "command-pending-resume"}
	receipt, err := f.s.Resume(t.Context(), resume)
	if err != nil {
		t.Fatalf("original approval Resume failed after admitted shell: %v", err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	// Synchronize with the mailbox: terminal state is committed just before flush.
	final := approvalSnapshot(t, f.s)
	if final.Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 2 || f.runs.Load() != 1 || shellRuns(t, f.s.rt.opts.Workspace) != 1 {
		t.Fatal("resume lost or repeated original work")
	}
	if retry, err := f.s.Resume(t.Context(), resume); err != nil || retry != receipt || f.model.Calls() != 2 || f.runs.Load() != 1 {
		t.Fatal("resume retry repeated work", err)
	}
	requests := captured.inputs()
	if len(requests) != 1 {
		t.Fatal("resumed model request was not captured")
	}
	wantResumed, err := agent.ConvertToLLM(f.manager.View().Messages)
	if err != nil {
		t.Fatal(err)
	}
	// The completed assistant and flushed command are later than the resumed
	// model request. Compare its exact original prompt/assistant/tool prefix.
	if len(wantResumed) < 3 {
		t.Fatal("completed history lost the original prompt/tool exchange")
	}
	if !reflect.DeepEqual(commandConversation(requests[0]), wantResumed[:3]) {
		gotJSON, _ := json.Marshal(commandConversation(requests[0]))
		wantJSON, _ := json.Marshal(wantResumed[:3])
		t.Fatalf("resume context differs: got=%s want=%s", gotJSON, wantJSON)
	}
	next, err := f.s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"next independent"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[next.TraceID].State) })
	requests = captured.inputs()
	if len(requests) != 2 || f.model.Calls() != 3 || f.runs.Load() != 1 || shellRuns(t, f.s.rt.opts.Workspace) != 1 {
		t.Fatal("next input repeated command or tool")
	}
	wantNext := append([]*schema.AgenticMessage(nil), wantResumed...)
	wantNext = append(wantNext, schema.UserAgenticMessage("next independent"))
	if !reflect.DeepEqual(commandConversation(requests[1]), wantNext) {
		t.Fatal("next independent request lost pending command content/order")
	}
	commandCount := 0
	for _, msg := range f.manager.View().Messages {
		if msg.Kind == agent.KindCommand {
			commandCount++
		}
	}
	wantCount := 1
	if exclude {
		wantCount = 0
	}
	if commandCount != wantCount {
		t.Fatal("command context exclusion or consumption count differs")
	}
	displayed := 0
	for _, msg := range approvalSnapshot(t, f.s).Messages {
		if msg.Kind == agent.KindCommand {
			displayed++
		}
	}
	if displayed != 1 {
		t.Fatal("command Snapshot missing or duplicated after consumption")
	}
}
