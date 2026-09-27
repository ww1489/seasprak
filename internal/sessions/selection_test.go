package sessions

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent/tools"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestDefaultModelRetryPreservesAcceptedInstanceAndLatestDefault(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-selection", true: "newer-selection"}[supersede], func(t *testing.T) {
			f, release := runningSelectionFixture(t)
			defer release()
			first := testkit.NewFake(testkit.Step{Text: "first", Repeat: true})
			replacement := testkit.NewFake(testkit.Step{Text: "replacement", Repeat: true})
			latest := testkit.NewFake(testkit.Step{Text: "latest", Repeat: true})
			request := SetDefaultModelRequest{Model: ModelChoice{Name: "first", Version: "1", Model: first}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "default"}
			receipt, err := f.s.SetDefaultModel(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if supersede {
				_, err = f.s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Name: "latest", Version: "1", Model: latest}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "latest"})
				if err != nil {
					t.Fatal(err)
				}
			}
			before := f.manager.View().LastSeq
			request.Model.Model = replacement
			again, err := f.s.SetDefaultModel(t.Context(), request)
			if err != nil || again != receipt {
				t.Fatalf("retry=%+v err=%v", again, err)
			}
			assertSelectionRetryUnchanged(t, f.s, receipt, before)
			release()
			waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
			if f.manager.View().Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 2 || f.runs.Load() != 1 || first.Calls()+replacement.Calls()+latest.Calls() != 0 {
				t.Fatal("default changed the original trace")
			}
			input, err := f.s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"independent"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[input.TraceID].State) })
			wantFirst, wantLatest := 1, 0
			if supersede {
				wantFirst, wantLatest = 0, 1
			}
			if f.manager.View().Traces[input.TraceID].State != "completed" || first.Calls() != wantFirst || latest.Calls() != wantLatest || replacement.Calls() != 0 {
				t.Fatalf("model calls first=%d latest=%d replacement=%d", first.Calls(), latest.Calls(), replacement.Calls())
			}
		})
	}
}

func TestNextTurnModelRetryPreservesAcceptedInstance(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	selected := testkit.NewFake(testkit.Step{Text: "selected"})
	replacement := testkit.NewFake(testkit.Step{Text: "replacement"})
	request := SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Name: "selected", Version: "1", Model: selected}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "next-turn"}
	receipt, err := f.s.SelectNextTurnModel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	before := f.manager.View().LastSeq
	request.Model.Model = replacement
	again, err := f.s.SelectNextTurnModel(t.Context(), request)
	if err != nil || again != receipt {
		t.Fatalf("retry=%+v err=%v", again, err)
	}
	assertSelectionRetryUnchanged(t, f.s, receipt, before)
	if selected.Calls()+replacement.Calls() != 0 || f.runs.Load() != 0 {
		t.Fatal("pending selection crossed the unfinished tool batch")
	}
	release()
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.manager.View().Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 1 || f.runs.Load() != 1 || selected.Calls() != 1 || replacement.Calls() != 0 {
		t.Fatalf("calls original=%d selected=%d replacement=%d tools=%d trace=%s", f.model.Calls(), selected.Calls(), replacement.Calls(), f.runs.Load(), f.manager.View().Traces[f.input.TraceID].State)
	}
}

func TestDefaultModelChangePreservesApprovalResumeModel(t *testing.T) {
	f := waitingApprovalSession(t, nil)
	selected := testkit.NewFake(testkit.Step{Text: "default B"})
	original := f.manager.View()
	checkpoint := original.Checkpoints[original.Traces[f.input.TraceID].CheckpointID]
	receipt, err := f.s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Name: "B", Version: "2", Model: selected}, ExpectedRevision: original.LastSeq, IdempotencyKey: "default-B"})
	if err != nil {
		t.Fatal(err)
	}
	if f.model.Calls() != 1 || selected.Calls() != 0 || f.runs.Load() != 0 {
		t.Fatal("default selection executed paused work")
	}
	assertSelectionRetryUnchanged(t, f.s, receipt, f.manager.View().LastSeq)
	answerApproval(t, f, "allowed-once")
	if f.model.Calls() != 1 || selected.Calls() != 0 || f.runs.Load() != 0 {
		t.Fatal("approval decision started execution")
	}
	resumed, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	op, err := f.manager.GetOperation(resumed.OperationID)
	if err != nil || op.State != "completed" || view.Traces[f.input.TraceID].State != "completed" || f.model.Calls() != 2 || selected.Calls() != 0 || f.runs.Load() != 1 {
		t.Fatalf("resume=%+v err=%v A=%d B=%d tools=%d", op, err, f.model.Calls(), selected.Calls(), f.runs.Load())
	}
	if view.Turns[checkpoint.Scope.TurnID].ModelConfigVersion != checkpoint.ModelConfigVersion {
		t.Fatal("original turn model changed")
	}
	input, err := f.s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"new independent trace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[input.TraceID].State) })
	if f.manager.View().Traces[input.TraceID].State != "completed" || f.model.Calls() != 2 || selected.Calls() != 1 || f.runs.Load() != 1 {
		t.Fatalf("new trace A=%d B=%d tools=%d", f.model.Calls(), selected.Calls(), f.runs.Load())
	}
}

func TestNextTurnModelTerminalRetryKeepsReceipt(t *testing.T) {
	f, release := runningSelectionFixture(t)
	defer release()
	selected := testkit.NewFake(testkit.Step{Text: "selected"})
	request := SelectNextTurnModelRequest{TraceID: f.input.TraceID, Model: ModelChoice{Name: "selected", Version: "1", Model: selected}, ExpectedRevision: f.manager.View().LastSeq, IdempotencyKey: "terminal-retry"}
	receipt, err := f.s.SelectNextTurnModel(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	release()
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	before := f.manager.View().LastSeq
	again, err := f.s.SelectNextTurnModel(t.Context(), request)
	if err != nil || again != receipt {
		t.Fatalf("terminal retry=%+v err=%v", again, err)
	}
	assertSelectionRetryUnchanged(t, f.s, receipt, before)
	conflict := request
	conflict.Model.Name = "different"
	_, err = f.s.SelectNextTurnModel(t.Context(), conflict)
	requireSessionCode(t, err, product.CodeIdempotencyConflict)
	conflict = request
	conflict.IdempotencyKey = "new-request"
	conflict.ExpectedRevision = before
	_, err = f.s.SelectNextTurnModel(t.Context(), conflict)
	requireSessionCode(t, err, product.CodeStateConflict)
	assertSelectionRetryUnchanged(t, f.s, receipt, before)
	if f.manager.View().Traces[f.input.TraceID].State != "completed" || selected.Calls() != 1 || f.model.Calls() != 1 || f.runs.Load() != 1 {
		t.Fatal("terminal retry started work")
	}
}

func runningSelectionFixture(t *testing.T) (approvalSessionFixture, func()) {
	t.Helper()
	id := agent.MustID()
	backend, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, id)
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	entered := make(chan struct{})
	model := versionedPauseModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "original done"})}
	runs := &atomic.Int32{}
	opts := Options{SessionID: id, Profile: ProfileMemory, Store: backend, Model: model, Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(entered)
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		runs.Add(1)
		return "tool done", nil
	}}}}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	s, err := Start(opts, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	release := func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	}
	t.Cleanup(release)
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"original"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not enter")
	}
	return approvalSessionFixture{s: s, manager: manager, model: model, input: input, runs: runs}, release
}

func assertSelectionRetryUnchanged(t *testing.T, session *AgentSession, receipt state.OperationReceipt, revision uint64) {
	t.Helper()
	manager, err := state.NewManager(session.rt.opts.Store, session.rt.opts.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if manager.View().LastSeq != revision {
		t.Fatal("retry appended a durable revision")
	}
	op, err := manager.GetOperation(receipt.OperationID)
	if err != nil || op.State != "accepted" || op.Revision != 1 || op.AcceptedCommit != receipt.AcceptedCommit {
		t.Fatalf("operation=%+v err=%v", op, err)
	}
}

func TestSetDefaultModelBindsOnlyTheNextIndependentTrace(t *testing.T) {
	backend, err := memory.Open("model-selection", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, "model-selection")
	if err != nil {
		t.Fatal(err)
	}
	base := testkit.NewFake(testkit.Step{Text: "base", Repeat: true})
	selected := testkit.NewFake(testkit.Step{Text: "selected", Repeat: true})
	session, err := Start(Options{SessionID: "model-selection", Profile: ProfileMemory, Store: backend, Model: base}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())

	receipt, err := session.SetDefaultModel(t.Context(), SetDefaultModelRequest{
		Model:            ModelChoice{Name: "selected", Version: "v2", Model: selected},
		ExpectedRevision: manager.View().LastSeq,
		IdempotencyKey:   "default-selected",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.OperationID == "" {
		t.Fatal("selection operation has no receipt")
	}
	input, err := session.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	view := manager.View()
	trace := view.Traces[input.TraceID]
	if trace.ModelSelectionID == "" {
		t.Fatalf("trace was not bound to the pending default: %+v", trace)
	}
	deadline := time.After(5 * time.Second)
	for selected.Calls() == 0 {
		select {
		case <-deadline:
			t.Fatal("selected model did not run")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if base.Calls() != 0 {
		t.Fatal("the initial model ran after a default selection was accepted")
	}
	deadline = time.After(5 * time.Second)
	for {
		trace = manager.View().Traces[input.TraceID]
		switch trace.State {
		case "completed":
			goto completed
		case "failed", "cancelled", "paused":
			t.Fatalf("selected trace ended unexpectedly: %+v", trace)
		}
		select {
		case <-deadline:
			t.Fatalf("selected trace did not complete: %+v", trace)
		default:
			time.Sleep(time.Millisecond)
		}
	}
completed:
}
