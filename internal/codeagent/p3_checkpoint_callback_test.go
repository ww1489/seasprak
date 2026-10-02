package codeagent

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

const p3CodeCheckpointChildEnv = "SEASPRAK_STEP9_CODE_CHECKPOINT_CHILD"

// Process-wide handlers are registered once before execution, only in a child.
// The parent never changes the callback registry.
func p3CodeCheckpointSubprocess(t *testing.T, name, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^"+name+"$", "-test.count=1", "-test.v", "-test.timeout=45s")
	cmd.Env = append(os.Environ(), p3CodeCheckpointChildEnv+"="+mode)
	output, err := cmd.CombinedOutput()
	t.Logf("child %s:\n%s", mode, output)
	if ctx.Err() != nil || err != nil {
		t.Fatalf("checkpoint child must complete its assertions: process=%v context=%v", err, ctx.Err())
	}
}

type p3CodeCheckpointCallbacks struct {
	p3CodeCheckpointAtomicCounts
	preflight p3CodeCheckpointAtomicCounts
	panicAt   callbacks.CallbackTiming
	armed     atomic.Bool
	drains    sync.WaitGroup
}

type p3CodeCheckpointAtomicCounts struct {
	needed  atomic.Int64
	timings [5]atomic.Int64
}

type p3CodeCheckpointCallbackCounts struct {
	Needed  int64
	Timings [5]int64
}

func (h *p3CodeCheckpointAtomicCounts) snapshot() p3CodeCheckpointCallbackCounts {
	c := p3CodeCheckpointCallbackCounts{Needed: h.needed.Load()}
	for i := range c.Timings {
		c.Timings[i] = h.timings[i].Load()
	}
	return c
}

func (c p3CodeCheckpointCallbackCounts) minus(before p3CodeCheckpointCallbackCounts) p3CodeCheckpointCallbackCounts {
	c.Needed -= before.Needed
	for i := range c.Timings {
		c.Timings[i] -= before.Timings[i]
	}
	return c
}

// This distinction uses the product's public execution-scope API, not an Eino
// private context key. Real workers have their own scope; preflight does not.
func p3CodeCheckpointIsPreflight(ctx context.Context) bool {
	return einorun.ScopeFromContext(ctx, agent.ExecutionScope{}).ExecutionID == ""
}

func (h *p3CodeCheckpointCallbacks) Needed(ctx context.Context, _ *callbacks.RunInfo, _ callbacks.CallbackTiming) bool {
	h.needed.Add(1)
	if p3CodeCheckpointIsPreflight(ctx) {
		h.preflight.needed.Add(1)
		if h.armed.Load() && h.panicAt == callbacks.CallbackTiming(255) {
			panic("synthetic preflight Needed panic")
		}
	}
	return true
}

func (h *p3CodeCheckpointCallbacks) record(ctx context.Context, timing callbacks.CallbackTiming) {
	h.timings[timing].Add(1)
	if p3CodeCheckpointIsPreflight(ctx) {
		h.preflight.timings[timing].Add(1)
		if h.armed.Load() && h.panicAt == timing {
			panic("synthetic preflight lifecycle panic")
		}
	}
}
func (h *p3CodeCheckpointCallbacks) OnStart(ctx context.Context, _ *callbacks.RunInfo, _ callbacks.CallbackInput) context.Context {
	h.record(ctx, callbacks.TimingOnStart)
	return ctx
}
func (h *p3CodeCheckpointCallbacks) OnEnd(ctx context.Context, _ *callbacks.RunInfo, output callbacks.CallbackOutput) context.Context {
	h.record(ctx, callbacks.TimingOnEnd)
	if out := adk.ConvTypedCallbackOutput[*schema.AgenticMessage](output); out != nil && out.Events != nil {
		h.drains.Add(1)
		go func() {
			defer h.drains.Done()
			for {
				if _, ok := out.Events.Next(); !ok {
					return
				}
			}
		}()
	}
	return ctx
}
func (h *p3CodeCheckpointCallbacks) OnError(ctx context.Context, _ *callbacks.RunInfo, _ error) context.Context {
	h.record(ctx, callbacks.TimingOnError)
	return ctx
}
func (h *p3CodeCheckpointCallbacks) OnStartWithStreamInput(ctx context.Context, _ *callbacks.RunInfo, in *schema.StreamReader[callbacks.CallbackInput]) context.Context {
	h.record(ctx, callbacks.TimingOnStartWithStreamInput)
	in.Close()
	return ctx
}
func (h *p3CodeCheckpointCallbacks) OnEndWithStreamOutput(ctx context.Context, _ *callbacks.RunInfo, out *schema.StreamReader[callbacks.CallbackOutput]) context.Context {
	h.record(ctx, callbacks.TimingOnEndWithStreamOutput)
	out.Close()
	return ctx
}

func p3CodeCheckpointRegister() *p3CodeCheckpointCallbacks {
	h := &p3CodeCheckpointCallbacks{}
	callbacks.AppendGlobalHandlers(h)
	return h
}

func TestP3CheckpointCallbacksPausedSnapshot(t *testing.T) {
	const name = "TestP3CheckpointCallbacksPausedSnapshot"
	mode := os.Getenv(p3CodeCheckpointChildEnv)
	if !strings.HasPrefix(mode, "snapshot:") {
		for _, kind := range []string{"pause", "approval"} {
			t.Run(kind, func(t *testing.T) { p3CodeCheckpointSubprocess(t, name, "snapshot:"+kind) })
		}
		return
	}
	h := p3CodeCheckpointRegister()
	f := resumeFixture{}
	if mode == "snapshot:approval" {
		a := waitingApprovalSession(t, nil)
		f.s, f.manager, f.input, f.model, f.runs = a.s, a.manager, a.input, a.model, a.runs
	} else {
		f = pausedResumeFixture(t, false)
	}
	before := f.manager.View()
	tr := before.Traces[f.input.TraceID]
	if tr.State != "paused" || tr.CheckpointID == "" || !tr.ExecutionStopped || tr.Settled {
		t.Fatalf("preflight fixture must be a successful actual pause: %+v", tr)
	}
	h.drains.Wait()
	baseline := h.snapshot()
	snap, err := f.s.Snapshot(t.Context())
	h.drains.Wait()
	delta := h.snapshot().minus(baseline)
	if err != nil || !snap.Resume[f.input.TraceID].CanResume {
		t.Fatalf("valid actual paused Snapshot failed: eligibility=%+v err=%v", snap.Resume[f.input.TraceID], err)
	}
	if delta != (p3CodeCheckpointCallbackCounts{}) {
		t.Errorf("actual paused Snapshot invoked callbacks: %+v", delta)
	}
	if !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("Snapshot changed paused journal, usage or business invocation counts")
	}
	t.Logf("actual %s Snapshot: callbacks=%+v models=1 tools=0 usage=%+v revision=%d", strings.TrimPrefix(mode, "snapshot:"), delta, tr.Usage, before.LastSeq)
}

// Faults affect only the copy returned by Get. Put, the committed reference and
// the underlying saved blob always remain the real product checkpoint.
type p3CodeCheckpointStore struct {
	store.Store
	store.CheckpointBlobs
	gets, puts, appends, associations atomic.Int64
	corrupt                           atomic.Bool
	mu                                sync.Mutex
	lastRef                           store.BlobRef
}

type p3CodeCheckpointStoreCounts struct {
	Gets, Puts, Appends, Associations int64
}

func (s *p3CodeCheckpointStore) counts() p3CodeCheckpointStoreCounts {
	return p3CodeCheckpointStoreCounts{s.gets.Load(), s.puts.Load(), s.appends.Load(), s.associations.Load()}
}
func (s *p3CodeCheckpointStore) Put(ctx context.Context, id string, data []byte) (store.BlobRef, error) {
	s.puts.Add(1)
	ref, err := s.CheckpointBlobs.Put(ctx, id, data)
	if err == nil {
		s.mu.Lock()
		s.lastRef = ref
		s.mu.Unlock()
	}
	return ref, err
}
func (s *p3CodeCheckpointStore) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	s.appends.Add(1)
	for _, record := range commit.ControlRecords {
		if record.Type == "checkpoint_ref" {
			s.associations.Add(1)
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}
func (s *p3CodeCheckpointStore) Get(ctx context.Context, id string, ref store.BlobRef) ([]byte, error) {
	s.gets.Add(1)
	data, err := s.CheckpointBlobs.Get(ctx, id, ref)
	if err != nil || !s.corrupt.Load() {
		return bytes.Clone(data), err
	}
	var envelope p3SemanticCheckpointEnvelope
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&envelope); err != nil {
		return nil, err
	}
	if !envelope.HasRunnerState || len(envelope.RunnerCheckpoint) == 0 || len(envelope.CanceledItems) != 1 || len(envelope.UnhandledItems) != 0 {
		return nil, product.NewError(product.CodeStorageUnavailable, "fault requires an actual paused envelope")
	}
	envelope.RunnerCheckpoint = []byte("synthetic corrupt native load")
	var out bytes.Buffer
	if err := gob.NewEncoder(&out).Encode(envelope); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func p3CodeCheckpointFixture(t *testing.T, kind string, corrupt bool) (resumeFixture, *p3CodeCheckpointStore, error) {
	t.Helper()
	if kind == "approval" {
		var observed *p3CodeCheckpointStore
		a := startApprovalSession(t, nil, func(backend store.Store) store.Store {
			observed = &p3CodeCheckpointStore{Store: backend, CheckpointBlobs: backend.(store.CheckpointBlobs)}
			observed.corrupt.Store(corrupt)
			return observed
		})
		waitResumeCondition(t, func() bool {
			tr := a.manager.View().Traces[a.input.TraceID]
			return tr.State == "paused" || terminal(tr.State)
		})
		return resumeFixture{s: a.s, manager: a.manager, opts: a.s.rt.opts, input: a.input, model: a.model, runs: a.runs}, observed, nil
	}
	id := agent.MustID()
	backend, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	observed := &p3CodeCheckpointStore{Store: backend, CheckpointBlobs: backend}
	observed.corrupt.Store(corrupt)
	manager, err := state.NewManager(observed, id)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	runs := &atomic.Int32{}
	model := versionedPauseModel{testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "original-provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "finished"})}
	opts := Options{SessionID: id, Profile: ProfileMemory, Store: observed, Workspace: t.TempDir(), Model: model, GenerationFingerprint: "step9-checkpoint-bundle-v1",
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(context.Context, json.RawMessage) (string, error) {
			runs.Add(1)
			return "original result", nil
		}}},
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"checkpoint integration"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return model.Calls() == 1 })
	type pauseResult struct {
		receipt state.OperationReceipt
		err     error
	}
	done := make(chan pauseResult, 1)
	go func() {
		receipt, err := s.Pause(t.Context(), input.TraceID)
		done <- pauseResult{receipt, err}
	}()
	waitResumeCondition(t, func() bool { return len(manager.View().Operations) == 1 })
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(gate) })
	select {
	case result := <-done:
		return resumeFixture{s: s, manager: manager, opts: opts, input: input, pause: result.receipt, model: model, runs: runs}, observed, result.err
	case <-time.After(5 * time.Second):
		t.Fatal("actual Pause did not finish")
	}
	return resumeFixture{}, nil, nil
}

func TestP3CheckpointCallbacksCorruptPausedQueriesDoNotWrite(t *testing.T) {
	const name = "TestP3CheckpointCallbacksCorruptPausedQueriesDoNotWrite"
	mode := os.Getenv(p3CodeCheckpointChildEnv)
	if !strings.HasPrefix(mode, "corrupt-query:") {
		for _, kind := range []string{"pause", "approval"} {
			t.Run(kind, func(t *testing.T) { p3CodeCheckpointSubprocess(t, name, "corrupt-query:"+kind) })
		}
		return
	}
	h := p3CodeCheckpointRegister()
	kind := strings.TrimPrefix(mode, "corrupt-query:")
	f, observed, err := p3CodeCheckpointFixture(t, kind, false)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "approval" {
		answerApproval(t, approvalSessionFixture{s: f.s, manager: f.manager, input: f.input, model: f.model, runs: f.runs}, "allowed-once")
	}
	before := f.manager.View()
	tr := before.Traces[f.input.TraceID]
	if tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || tr.CheckpointID == "" || len(before.Checkpoints) != 1 {
		t.Fatalf("negative query must begin at a real successfully paused trace: %+v", tr)
	}
	cp := before.Checkpoints[tr.CheckpointID]
	ref := store.BlobRef{Hash: cp.BlobHash, Size: cp.BlobSize}
	blob, err := observed.CheckpointBlobs.Get(t.Context(), f.opts.SessionID, ref)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := observed.Store.Load(t.Context(), f.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	h.drains.Wait()
	baseline := h.snapshot()
	counts := observed.counts()
	observed.corrupt.Store(true)
	snap, err := f.s.Snapshot(t.Context())
	eligibility := snap.Resume[f.input.TraceID]
	if err != nil || eligibility.CanResume || eligibility.Code != product.CodeIncompatibleResume || eligibility.Reason != "checkpoint has no compatible native agent state" {
		t.Fatalf("Snapshot must reach native validation and reject: eligibility=%+v err=%v", eligibility, err)
	}
	_, err = f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "corrupt-preflight"})
	pe, ok := product.AsError(err)
	if !ok || pe.Code != product.CodeIncompatibleResume || pe.Message != "checkpoint has no compatible native agent state" {
		t.Fatalf("Resume must reach native validation and reject: %v", err)
	}
	h.drains.Wait()
	afterCounts := observed.counts()
	if afterCounts.Gets != counts.Gets+2 || afterCounts.Puts != counts.Puts || afterCounts.Appends != counts.Appends || afterCounts.Associations != counts.Associations {
		t.Fatalf("negative queries must load twice without writes: before=%+v after=%+v", counts, afterCounts)
	}
	afterJournal, err := observed.Store.Load(t.Context(), f.opts.SessionID)
	if err != nil || !reflect.DeepEqual(journal, afterJournal) || !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("rejected queries changed revision/journal/usage/authorizations or started work")
	}
	afterBlob, err := observed.CheckpointBlobs.Get(t.Context(), f.opts.SessionID, ref)
	if err != nil || !bytes.Equal(blob, afterBlob) || h.snapshot().minus(baseline) != (p3CodeCheckpointCallbackCounts{}) {
		t.Fatal("rejected queries changed the saved blob or invoked callbacks")
	}
	observed.corrupt.Store(false)
	valid, err := f.s.Snapshot(t.Context())
	if err != nil || !valid.Resume[f.input.TraceID].CanResume || !reflect.DeepEqual(before, f.manager.View()) {
		t.Fatal("copy-only fault damaged the real checkpoint")
	}
	t.Logf("actual %s negative Snapshot/Resume: Gets_delta=2 Append_delta=0 Put_delta=0 callback_delta=0 models=1 tools=0 usage=%+v", kind, tr.Usage)
}

func TestP3CheckpointCallbacksPublication(t *testing.T) {
	const name = "TestP3CheckpointCallbacksPublication"
	mode := os.Getenv(p3CodeCheckpointChildEnv)
	if !strings.HasPrefix(mode, "publication:") {
		for _, kind := range []string{"pause", "approval"} {
			for _, fault := range []string{"count", "start", "end", "needed", "corrupt"} {
				t.Run(kind+"/"+fault, func(t *testing.T) { p3CodeCheckpointSubprocess(t, name, "publication:"+kind+":"+fault) })
			}
		}
		return
	}
	parts := strings.Split(strings.TrimPrefix(mode, "publication:"), ":")
	kind, fault := parts[0], parts[1]
	h := p3CodeCheckpointRegister()
	if fault == "start" || fault == "end" || fault == "needed" {
		h.panicAt = callbacks.TimingOnStart
		if fault == "end" {
			h.panicAt = callbacks.TimingOnEnd
		} else if fault == "needed" {
			h.panicAt = callbacks.CallbackTiming(255)
		}
		h.armed.Store(true)
	}
	f, observed, pauseErr := p3CodeCheckpointFixture(t, kind, fault == "corrupt")
	h.drains.Wait()
	v := f.manager.View()
	tr := v.Traces[f.input.TraceID]
	counts := observed.counts()
	if counts.Puts != 1 || counts.Gets != 1 || f.model.Calls() != 1 || f.runs.Load() != 0 || tr.Usage.LogicalModelCalls != 1 || tr.Usage.TransportRequests != 1 || tr.Usage.ToolExecutions != 0 {
		t.Fatalf("publication repeated work or missed save/load: counts=%+v trace=%+v models=%d tools=%d", counts, tr, f.model.Calls(), f.runs.Load())
	}
	if h.preflight.snapshot() != (p3CodeCheckpointCallbackCounts{}) || h.snapshot().Needed == 0 || h.snapshot().Timings[callbacks.TimingOnStart] == 0 || h.snapshot().Timings[callbacks.TimingOnEnd] == 0 {
		t.Fatalf("publication preflight entered callbacks or normal execution lost callbacks: preflight=%+v total=%+v", h.preflight.snapshot(), h.snapshot())
	}
	if fault == "corrupt" {
		observed.mu.Lock()
		savedRef := observed.lastRef
		observed.mu.Unlock()
		original, err := observed.CheckpointBlobs.Get(t.Context(), f.opts.SessionID, savedRef)
		if err != nil {
			t.Fatal("failed publication lost the actual saved orphan blob")
		}
		beforeCallbacks := h.snapshot()
		if err := einorun.ValidatePausedCheckpoint(original, agent.InputRef{InputID: f.input.InputID, TraceID: f.input.TraceID, Kind: "prompt"}, tr.Target.Name); err != nil || h.snapshot() != beforeCallbacks {
			t.Fatalf("fault must affect only the returned copy, not the saved checkpoint: %v", err)
		}
		if kind == "pause" {
			requireSessionCode(t, pauseErr, product.CodeStateConflict)
			if v.Operations[f.pause.OperationID].State != "failed" {
				t.Fatal("failed Pause published a success receipt")
			}
		}
		if tr.State != "failed" || !tr.ExecutionStopped || !tr.Settled || len(v.Checkpoints) != 0 || tr.CheckpointID != "" || counts.Associations != 0 || !strings.Contains(tr.Error, product.CodeIncompatibleResume) {
			t.Fatalf("invalid native checkpoint acquired a paused/waiting association: trace=%+v checkpoints=%d counts=%+v", tr, len(v.Checkpoints), counts)
		}
		snap, err := f.s.Snapshot(t.Context())
		if err != nil || snap.Resume[tr.ID].CanResume {
			t.Fatal("failed publication advertised valid Resume")
		}
		for _, in := range snap.Interactions {
			if in.State == "ready" || in.CheckpointRef != "" {
				t.Fatal("failed approval publication bound a ready interaction")
			}
		}
		// Failure finalization is allowed to append; only valid checkpoint
		// publication is forbidden. Do not misstate this as a zero-write Pause.
		t.Logf("actual %s corrupt publication: blob_save=1 validation_Get=1 associations=0 state=failed models=1 tools=0 Append=%d usage=%+v", kind, counts.Appends, tr.Usage)
		return
	}
	if pauseErr != nil || tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || tr.CheckpointID == "" || len(v.Checkpoints) != 1 || counts.Associations != 1 {
		t.Fatalf("legal checkpoint was not safely published: trace=%+v counts=%+v err=%v", tr, counts, pauseErr)
	}
	cp := v.Checkpoints[tr.CheckpointID]
	if kind == "approval" {
		if len(cp.ApprovalTargets) != 1 || len(v.FrozenExecutions) != 1 {
			t.Fatal("approval publication lost its original frozen authorization target")
		}
	} else if v.Operations[f.pause.OperationID].State != "completed" || v.Operations[f.pause.OperationID].ResultRef != cp.ID {
		t.Fatal("Pause receipt and valid checkpoint were not associated together")
	}
	t.Logf("actual %s %s publication: blob_save=1 validation_Get=1 associations=1 state=paused preflight_callbacks=0 normal_callbacks=%+v usage=%+v", kind, fault, h.snapshot(), tr.Usage)
}

func TestP3CheckpointCallbacksDiskReopenResume(t *testing.T) {
	const name = "TestP3CheckpointCallbacksDiskReopenResume"
	mode := os.Getenv(p3CodeCheckpointChildEnv)
	if !strings.HasPrefix(mode, "disk:") {
		for _, boundary := range []string{"after-model", "after-tool"} {
			t.Run(boundary, func(t *testing.T) { p3CodeCheckpointSubprocess(t, name, "disk:"+boundary) })
		}
		return
	}
	h := p3CodeCheckpointRegister()
	afterTool := mode == "disk:after-tool"
	f := pausedResumeFixture(t, afterTool, resumeFixtureOptions{Disk: true})
	before := f.manager.View()
	tr := before.Traces[f.input.TraceID]
	if tr.State != "paused" || !tr.ExecutionStopped || tr.Settled || f.model.Calls() != 1 || tr.Usage.LogicalModelCalls != 1 || tr.Usage.TransportRequests != 1 || tr.Usage.ToolExecutions != int(f.runs.Load()) {
		t.Fatalf("disk fixture was not safely paused: %+v", tr)
	}
	h.drains.Wait()
	if h.preflight.snapshot() != (p3CodeCheckpointCallbackCounts{}) {
		t.Fatal("disk Pause publication invoked preflight callbacks")
	}
	cp := before.Checkpoints[tr.CheckpointID]
	ref := store.BlobRef{Hash: cp.BlobHash, Size: cp.BlobSize}
	blob, err := f.s.rt.opts.Store.(store.CheckpointBlobs).Get(t.Context(), f.opts.SessionID, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	journalPath := journalPath(f.opts.StateRoot, f.opts.SessionID)
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	resumedModel := versionedPauseModel{testkit.NewFake(testkit.Step{Text: "resumed answer"})}
	f.opts.Model = resumedModel
	baseline := h.snapshot()
	opened, err := OpenAgentSession(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	snap, err := opened.Snapshot(t.Context())
	h.drains.Wait()
	currentJournal, readErr := os.ReadFile(journalPath)
	if err != nil || readErr != nil || !snap.Resume[f.input.TraceID].CanResume || !reflect.DeepEqual(before, opened.rt.manager.View()) || !bytes.Equal(journal, currentJournal) || resumedModel.Calls() != 0 || f.model.Calls() != 1 || h.snapshot().minus(baseline) != (p3CodeCheckpointCallbackCounts{}) {
		t.Fatalf("Close/Open/Snapshot changed durable state, ran work or invoked callbacks: err=%v readErr=%v", err, readErr)
	}
	cmd := ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "step9-resume"}
	receipt, err := opened.Resume(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[f.input.TraceID].State) })
	h.drains.Wait()
	after := opened.rt.manager.View()
	completed := after.Traces[f.input.TraceID]
	if completed.State != "completed" || !completed.Settled || f.model.Calls() != 1 || resumedModel.Calls() != 1 || f.runs.Load() != 1 || completed.Usage.LogicalModelCalls != 2 || completed.Usage.TransportRequests != 2 || completed.Usage.ToolExecutions != 1 || len(after.Calls) != 1 || len(after.ModelAttempts) != 2 {
		t.Fatalf("resume duplicated model/tool execution or budgets: trace=%+v models=%d/%d tools=%d calls=%d attempts=%d", completed, f.model.Calls(), resumedModel.Calls(), f.runs.Load(), len(after.Calls), len(after.ModelAttempts))
	}
	for id, original := range before.Calls {
		current := after.Calls[id]
		if current.Scope != original.Scope || current.Call != original.Call || current.Observation == nil || current.Observation.Content != "saved result" || afterTool && !reflect.DeepEqual(current, original) {
			t.Fatal("resume changed original call identity or repeated its completed result")
		}
	}
	if h.preflight.snapshot() != (p3CodeCheckpointCallbackCounts{}) {
		t.Fatalf("Resume validation invoked callbacks: %+v", h.preflight.snapshot())
	}
	normal := h.snapshot().minus(baseline)
	if normal.Needed == 0 || normal.Timings[callbacks.TimingOnStart] == 0 || normal.Timings[callbacks.TimingOnEnd] == 0 {
		t.Fatal("normal resumed execution callbacks were suppressed")
	}
	saved, err := opened.rt.opts.Store.(store.CheckpointBlobs).Get(t.Context(), f.opts.SessionID, ref)
	if err != nil || !bytes.Equal(blob, saved) {
		t.Fatal("resume preflight altered the original disk blob")
	}
	again, err := opened.Resume(t.Context(), cmd)
	if err != nil || again != receipt || !reflect.DeepEqual(after, opened.rt.manager.View()) {
		t.Fatalf("same-process Resume receipt lost idempotency: receipt=%+v want=%+v err=%v", again, receipt, err)
	}
	status, err := opened.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "completed" {
		t.Fatalf("Resume operation not completed: %+v err=%v", status, err)
	}
	if err := opened.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAgentSession(t.Context(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	again, err = reopened.Resume(t.Context(), cmd)
	if err != nil || again != receipt || !reflect.DeepEqual(after, reopened.rt.manager.View()) || resumedModel.Calls() != 1 || f.model.Calls() != 1 || f.runs.Load() != 1 || h.preflight.snapshot() != (p3CodeCheckpointCallbackCounts{}) {
		t.Fatalf("disk replay repeated work/budget or changed the receipt: receipt=%+v err=%v", again, err)
	}
	t.Logf("%s Close/Open/Snapshot/Resume/completed: preflight_callbacks=0 normal_callbacks=%+v models=1+1 tools=1 budget=2/2/1 stable_receipt=true", strings.TrimPrefix(mode, "disk:"), normal)
}
