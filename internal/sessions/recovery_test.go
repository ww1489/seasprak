package sessions

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

// TestSessionCrash 在默认套件内用真实子进程 Kill/Wait/重开覆盖五个业务提交窗口，
// 每个窗口各有 Append 前与成功后两种（共 10 子案例）。证据映射：
//
//	input.accepted_before     无受理记录；不 Submit 新任务冒充恢复
//	input.accepted_after      Input pending；幂等回放原回执；queued+hold
//	assistant_before          Input 已消费；尚无带工具的 complete assistant
//	assistant_after           第一条带工具 complete assistant；Call Claimed=false Observation=nil
//	tool_intent_before        assistant 已在；Claimed=false Observation=nil
//	tool_intent_after         Claimed=true Observation=nil；效果次数=0
//	tool_observation_before   Claimed=true Observation=nil；效果次数=1；预算占额不是执行证明
//	tool_observation_after    Observation 已持久（succeeded/executed/none/1）；效果次数=1
//	trace.settled_before      Observation 在；settled=0；Open 后 paused
//	trace.settled_after       settled=1 且终态 completed
func TestSessionCrash(t *testing.T) {
	if os.Getenv("SEASPRAK_RECOVERY_CHILD") != "" {
		runRecoveryChild(t)
		return
	}
	windows := []string{windowAccepted, windowAssistant, windowIntent, windowObservation, windowSettled}
	phases := []string{"before", "after"}
	for _, window := range windows {
		for _, phase := range phases {
			t.Run(window+"_"+phase, func(t *testing.T) {
				runRecoveryParent(t, window, phase)
			})
		}
	}
}

func runRecoveryChild(t *testing.T) {
	t.Helper()
	ws := os.Getenv("SEASPRAK_RECOVERY_WORKSPACE")
	stateRoot := os.Getenv("SEASPRAK_RECOVERY_STATE")
	sessionID := os.Getenv("SEASPRAK_RECOVERY_SESSION")
	window := os.Getenv("SEASPRAK_RECOVERY_WINDOW")
	phase := os.Getenv("SEASPRAK_RECOVERY_PHASE")
	effect := os.Getenv("SEASPRAK_RECOVERY_EFFECT")
	if ws == "" || stateRoot == "" || sessionID == "" || window == "" || phase == "" || effect == "" {
		t.Fatal("child missing recovery environment")
	}
	model := testkit.NewFake(
		testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "add-1", Name: "add", Arguments: `{"n":1}`}}},
		testkit.Step{Text: "sum is 1"},
	)
	opts := recoveryOpts(ws, stateRoot, sessionID, model, effect)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	created, err := CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatalf("create empty session: %v", err)
	}
	if err := created.Close(ctx); err != nil {
		t.Fatalf("close empty session: %v", err)
	}
	header, err := readHeader(stateRoot, sessionID, 0)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatalf("align tools: %v", err)
	}
	opts.Workspace = binding.HostRealRoot
	backend, err := jsonl.Open(sessionID, stateRoot, header, jsonl.Options{OpenExisting: true})
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	wrapped := &crashStore{inner: backend, sessionID: sessionID, window: window, phase: phase, model: model, effect: effect}
	manager, err := state.NewManager(wrapped, sessionID)
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	opts.Store = wrapped
	started, err := Start(opts, manager, binding.Generation)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = started.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: append(json.RawMessage(nil), recoveryPrompt...), IdempotencyKey: recoveryIdemKey})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	select {
	case <-ctx.Done():
		t.Fatalf("window %s %s was not reached: %v", window, phase, ctx.Err())
	}
}

func runRecoveryParent(t *testing.T, window, phase string) {
	t.Helper()
	ws := t.TempDir()
	stateRoot := t.TempDir()
	effect := filepath.Join(t.TempDir(), "effect")
	if err := os.WriteFile(effect, []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := "crash-" + strings.ReplaceAll(window, ".", "-") + "-" + phase
	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionCrash$", "-test.count=1", "-test.timeout=60s")
	cmd.Env = append(os.Environ(),
		"SEASPRAK_RECOVERY_CHILD=1",
		"SEASPRAK_RECOVERY_WORKSPACE="+ws,
		"SEASPRAK_RECOVERY_STATE="+stateRoot,
		"SEASPRAK_RECOVERY_SESSION="+sessionID,
		"SEASPRAK_RECOVERY_WINDOW="+window,
		"SEASPRAK_RECOVERY_PHASE="+phase,
		"SEASPRAK_RECOVERY_EFFECT="+effect,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitCh := make(chan error, 1)
	ready := make(chan crashReport, 1)
	scanDone := make(chan struct{})
	var stdoutBuf bytes.Buffer
	go func() {
		defer close(scanDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			stdoutBuf.WriteString(line)
			stdoutBuf.WriteByte('\n')
			if !strings.HasPrefix(line, crashReadyPrefix) {
				continue
			}
			var rep crashReport
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, crashReadyPrefix)), &rep); err != nil {
				continue
			}
			select {
			case ready <- rep:
			default:
			}
		}
	}()
	go func() {
		<-scanDone // StdoutPipe must be drained before Wait closes it.
		waitCh <- cmd.Wait()
	}()
	var stopOnce sync.Once
	stopped := false
	stop := func() bool {
		stopOnce.Do(func() {
			if cmd.Process != nil {
				if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
					t.Errorf("kill: %v", err)
				}
			}
			select {
			case <-scanDone:
			case <-time.After(15 * time.Second):
				_ = stdout.Close()
				t.Error("stdout reader did not finish after kill")
				return
			}
			select {
			case <-waitCh:
				stopped = true
			case <-time.After(15 * time.Second):
				t.Error("child wait timed out")
			}
		})
		return stopped
	}
	t.Cleanup(func() { stop() })

	var rep crashReport
	select {
	case rep = <-ready:
		if !stop() {
			t.Fatal("child did not stop at the reported crash window")
		}
	case <-scanDone:
		if !stop() {
			t.Fatal("child exited stdout without completing wait")
		}
		t.Fatalf("child stdout closed without a crash report\nstdout:\n%s\nstderr:\n%s", stdoutBuf.String(), stderr.String())
	case <-time.After(40 * time.Second):
		if !stop() {
			t.Fatal("child did not stop after crash window timeout")
		}
		t.Fatalf("timed out waiting for child window %s %s\nstdout:\n%s\nstderr:\n%s", window, phase, stdoutBuf.String(), stderr.String())
	}

	wantModel, wantTool := expectedCalls(window)
	if rep.ModelCalls != wantModel || rep.ToolCalls != wantTool {
		t.Fatalf("child counts at %s %s: model=%d tool=%d, want model=%d tool=%d",
			window, phase, rep.ModelCalls, rep.ToolCalls, wantModel, wantTool)
	}
	if got := mustReadEffect(t, effect); got != rep.ToolCalls {
		t.Fatalf("effect file %d does not match reported tool calls %d", got, rep.ToolCalls)
	}
	crashed, err := loadCrashJournal(stateRoot, sessionID)
	if err != nil {
		t.Fatalf("read crashed journal: %v", err)
	}
	assertCrashJournal(t, crashed, rep)
	assertReopenedSession(t, ws, stateRoot, sessionID, effect, crashed, rep)
}

func mustReadEffect(t *testing.T, path string) int {
	t.Helper()
	n, err := readEffect(path)
	if err != nil {
		t.Fatalf("effect file: %v", err)
	}
	return n
}

func assertCrashJournal(t *testing.T, stored store.StoredSession, rep crashReport) {
	t.Helper()
	if rep.Window != windowAccepted && len(rep.Prefix) == 0 {
		t.Fatal("expected persisted prefix before this window")
	}
	for i, want := range rep.Prefix {
		got, ok := commitByID(stored, want.CommitID)
		if !ok {
			t.Fatalf("prefix commit %d %s missing after kill", i, want.CommitID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("prefix commit %s changed after kill: got=%+v want=%+v", want.CommitID, got, want)
		}
	}
	got, hasTarget := commitByID(stored, rep.Target.CommitID)
	present := crashWindowPresent(stored, rep.Window)
	if rep.Phase == "before" {
		if hasTarget || present {
			t.Fatalf("before-window %s persisted after kill: target=%v present=%v", rep.Window, hasTarget, present)
		}
		return
	}
	if !hasTarget || !present {
		t.Fatalf("after-window %s missing after kill: target=%v present=%v", rep.Window, hasTarget, present)
	}
	if !reflect.DeepEqual(got, rep.Target) {
		t.Fatalf("target commit changed after kill: got=%+v want=%+v", got, rep.Target)
	}
}

func assertReopenedSession(t *testing.T, ws, stateRoot, sessionID, effect string, crashed store.StoredSession, rep crashReport) {
	t.Helper()
	effectBefore := mustReadEffect(t, effect)
	firstFake := testkit.NewFake(testkit.Step{Text: "must-not-run"})
	first := openCrashed(t, ws, stateRoot, sessionID, firstFake, effect)
	assertOpenedState(t, first, ws, stateRoot, sessionID, crashed, rep, effectBefore)
	assertNoRerun(t, firstFake, effect, effectBefore)
	assertIdempotentReplay(t, first, crashed, rep)
	assertNoRerun(t, firstFake, effect, effectBefore)
	closeCrashed(t, first)
	assertNoRerun(t, firstFake, effect, effectBefore)

	afterFirst, err := loadCrashJournal(stateRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	assertCrashJournal(t, afterFirst, rep)
	if got := countSettled(afterFirst); got != wantSettled(rep) {
		t.Fatalf("settled events after first reopen = %d, want %d", got, wantSettled(rep))
	}

	secondFake := testkit.NewFake(testkit.Step{Text: "must-not-run"})
	second := openCrashed(t, ws, stateRoot, sessionID, secondFake, effect)
	assertOpenedState(t, second, ws, stateRoot, sessionID, afterFirst, rep, effectBefore)
	assertNoRerun(t, secondFake, effect, effectBefore)
	assertIdempotentReplay(t, second, afterFirst, rep)
	assertNoRerun(t, secondFake, effect, effectBefore)
	closeCrashed(t, second)
	assertNoRerun(t, secondFake, effect, effectBefore)

	afterSecond, err := loadCrashJournal(stateRoot, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	assertCrashJournal(t, afterSecond, rep)
	if got := countSettled(afterSecond); got != wantSettled(rep) {
		t.Fatalf("settled events after second reopen = %d, want %d", got, wantSettled(rep))
	}
}

func assertNoRerun(t *testing.T, fake *testkit.FakeModel, effect string, wantEffect int) {
	t.Helper()
	if fake.Calls() != 0 {
		t.Fatalf("reopen or replay ran the model %d times", fake.Calls())
	}
	if got := mustReadEffect(t, effect); got != wantEffect {
		t.Fatalf("tool effect changed from %d to %d", wantEffect, got)
	}
}

func openCrashed(t *testing.T, ws, stateRoot, sessionID string, model *testkit.FakeModel, effect string) *AgentSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := OpenAgentSession(ctx, recoveryOpts(ws, stateRoot, sessionID, model, effect))
	if err != nil {
		t.Fatalf("OpenAgentSession: %v", err)
	}
	return s
}

func closeCrashed(t *testing.T, s *AgentSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("close reopened session: %v", err)
	}
}

func assertOpenedState(t *testing.T, s *AgentSession, ws, stateRoot, sessionID string, journal store.StoredSession, rep crashReport, effect int) {
	t.Helper()
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	binding := assertWorkspaceBinding(t, ws, stateRoot, sessionID)
	if countSettled(journal) != wantSettled(rep) {
		t.Fatalf("settled events = %d, want %d", countSettled(journal), wantSettled(rep))
	}
	if rep.Window == windowAccepted && rep.Phase == "before" {
		if len(snap.Inputs) != 0 || len(snap.Traces) != 0 {
			t.Fatalf("accepted-before reopen created work: inputs=%d traces=%d", len(snap.Inputs), len(snap.Traces))
		}
		return
	}
	rec, ok := receiptIn(append(append([]store.Commit{}, rep.Prefix...), rep.Target)...)
	if !ok {
		t.Fatalf("missing accepted receipt in report: %+v", rep)
	}
	in := snap.Inputs[rec.InputID]
	if in == nil {
		t.Fatalf("input %s missing after reopen", rec.InputID)
	}
	if in.TraceID != rec.TraceID {
		t.Fatalf("input trace %s, want %s", in.TraceID, rec.TraceID)
	}
	if in.Target.Generation != binding.Generation {
		t.Fatalf("input generation %s, want %s", in.Target.Generation, binding.Generation)
	}
	wantInput := "consumed"
	if rep.Window == windowAccepted {
		wantInput = "pending"
	}
	if in.State != wantInput {
		t.Fatalf("input %s state=%s, want %s", rec.InputID, in.State, wantInput)
	}
	tr := snap.Traces[rec.TraceID]
	if tr == nil {
		t.Fatalf("trace %s missing after reopen", rec.TraceID)
	}
	if tr.Generation != binding.Generation || tr.Target.Generation != binding.Generation {
		t.Fatalf("trace generation %s/%s, want %s", tr.Generation, tr.Target.Generation, binding.Generation)
	}
	switch {
	case rep.Window == windowAccepted:
		if tr.State != "queued" || !tr.Hold || tr.Settled {
			t.Fatalf("accepted-after should be queued+hold, got state=%s hold=%v settled=%v", tr.State, tr.Hold, tr.Settled)
		}
	case rep.Window == windowSettled && rep.Phase == "after":
		if tr.State != "completed" || !tr.Settled {
			t.Fatalf("settled-after should be completed, got state=%s settled=%v", tr.State, tr.Settled)
		}
	default:
		if tr.State != "paused" || tr.Settled {
			t.Fatalf("unfinished reopen should be paused, got state=%s settled=%v (must not resume)", tr.State, tr.Settled)
		}
	}
	if tr.Usage != expectedUsage(rep.Window) {
		t.Fatalf("persisted usage %+v, want %+v (budget occupancy is not execution)", tr.Usage, expectedUsage(rep.Window))
	}
	_, wantTool := expectedCalls(rep.Window)
	if effect != wantTool {
		t.Fatalf("actual tool effect %d, want %d", effect, wantTool)
	}
	assertMessagesAndCalls(t, snap, binding.Generation, journal, rep)
}

func assertWorkspaceBinding(t *testing.T, ws, stateRoot, sessionID string) workspaceBinding {
	t.Helper()
	header, err := readHeader(stateRoot, sessionID, 0)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	binding, err := decodeBinding(header.Workspace)
	if err != nil {
		t.Fatalf("decode binding: %v", err)
	}
	real, err := store.ResolveDir(ws)
	if err != nil {
		t.Fatalf("resolve workspace: %v", err)
	}
	if !store.SamePath(binding.HostRealRoot, real) {
		t.Fatalf("workspace binding %s, want %s", binding.HostRealRoot, real)
	}
	if binding.Generation == "" {
		t.Fatal("generation binding is empty")
	}
	return binding
}

func assertMessagesAndCalls(t *testing.T, snap Snapshot, generation string, journal store.StoredSession, rep crashReport) {
	t.Helper()
	for _, c := range persistedCommits(rep) {
		if _, ok := commitByID(journal, c.CommitID); !ok {
			t.Fatalf("persisted commit %s missing after reopen", c.CommitID)
		}
		for _, e := range c.Entries {
			if e.Type != "message" {
				continue
			}
			var msg agent.AgentMessage
			if err := json.Unmarshal(e.Payload, &msg); err != nil {
				t.Fatalf("message payload: %v", err)
			}
			if !snapshotHasMessage(snap, msg.ID) {
				t.Fatalf("message %s missing after reopen", msg.ID)
			}
		}
		for _, r := range c.ControlRecords {
			if r.Type != "tool_call" {
				continue
			}
			var rec agent.ToolRecord
			if err := json.Unmarshal(r.Payload, &rec); err != nil {
				t.Fatalf("tool payload: %v", err)
			}
			if _, ok := snap.Calls[rec.Call.CallID]; !ok {
				t.Fatalf("call %s missing after reopen", rec.Call.CallID)
			}
		}
	}
	var assistant *agent.AgentMessage
	for i := range snap.Messages {
		msg := &snap.Messages[i]
		if msg.Kind == agent.KindAssistant && msg.Status == agent.StatusComplete && messageHasTools(*msg) {
			assistant = msg
			break
		}
	}
	wantAssistant := (rep.Window == windowAssistant && rep.Phase == "after") ||
		rep.Window == windowIntent || rep.Window == windowObservation || rep.Window == windowSettled
	if wantAssistant && assistant == nil {
		t.Fatal("expected first tool-bearing complete assistant after reopen")
	}
	if !wantAssistant && assistant != nil {
		t.Fatalf("unexpected tool-bearing assistant before that window: %+v", assistant)
	}
	wantCall := wantAssistant
	if !wantCall {
		if len(snap.Calls) != 0 {
			t.Fatalf("unexpected tool records: %+v", snap.Calls)
		}
		return
	}
	if len(snap.Calls) != 1 {
		t.Fatalf("tool records = %d, want 1: %+v", len(snap.Calls), snap.Calls)
	}
	var rec agent.ToolRecord
	for _, call := range snap.Calls {
		rec = call
	}
	if rec.Call.Generation != generation || rec.Scope.Generation != generation {
		t.Fatalf("tool generation call=%s scope=%s, want %s", rec.Call.Generation, rec.Scope.Generation, generation)
	}
	wantClaimed := (rep.Window == windowIntent && rep.Phase == "after") ||
		rep.Window == windowObservation || rep.Window == windowSettled
	wantObs := (rep.Window == windowObservation && rep.Phase == "after") || rep.Window == windowSettled
	if rec.Claimed != wantClaimed {
		t.Fatalf("Claimed=%v, want %v", rec.Claimed, wantClaimed)
	}
	if !wantObs {
		if rec.Observation != nil {
			t.Fatalf("unexpected observation before that window: %+v", rec.Observation)
		}
		return
	}
	want := &agent.ToolObservation{Status: "succeeded", Content: "1", SideEffect: "none", Executed: true}
	if !reflect.DeepEqual(rec.Observation, want) {
		t.Fatalf("observation %+v, want %+v", rec.Observation, want)
	}
}

func snapshotHasMessage(snap Snapshot, id string) bool {
	for _, msg := range snap.Messages {
		if msg.ID == id {
			return true
		}
	}
	return false
}

func assertIdempotentReplay(t *testing.T, s *AgentSession, journal store.StoredSession, rep crashReport) {
	t.Helper()
	if rep.Window == windowAccepted && rep.Phase == "before" {
		return
	}
	original, ok := acceptedReceipt(journal)
	if !ok {
		t.Fatal("accepted receipt missing; cannot prove idempotent replay")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	again, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: append(json.RawMessage(nil), recoveryPrompt...), IdempotencyKey: recoveryIdemKey})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if again.InputID != original.InputID || again.TraceID != original.TraceID || again.AcceptedCommit != original.AcceptedCommit {
		t.Fatalf("idempotent replay rescheduled: got=%+v want=%+v", again, original)
	}
}
