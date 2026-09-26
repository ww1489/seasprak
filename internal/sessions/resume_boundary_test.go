package sessions

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type runnerlessResumeStore struct {
	store.Store
	store.CheckpointBlobs
	input agent.InputRef
}

func (s runnerlessResumeStore) Get(context.Context, string, store.BlobRef) ([]byte, error) {
	var data bytes.Buffer
	err := gob.NewEncoder(&data).Encode(struct {
		RunnerCheckpoint              []byte
		HasRunnerState                bool
		UnhandledItems, CanceledItems []agent.InputRef
	}{CanceledItems: []agent.InputRef{s.input}})
	return data.Bytes(), err
}
func TestResumeWithNoRunnerRejectsBeforeAcceptance(t *testing.T) {
	f := pausedResumeFixture(t, false)
	if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
		var input agent.InputRef
		for _, cp := range rt.manager.View().Checkpoints {
			input = cp.Input
		}
		rt.opts.Store = runnerlessResumeStore{Store: rt.opts.Store, CheckpointBlobs: rt.opts.Store.(store.CheckpointBlobs), input: input}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := f.manager.View()
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume || !reflect.DeepEqual(before, f.manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatalf("runnerless resume=%v", err)
	}
}

func TestOldExecutionCannotPublishWhileResumedOriginalToolRuns(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{BlockResumedTool: true})
	before := f.manager.View()
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed tool did not enter")
	}
	for _, call := range before.Calls {
		body, _ := json.Marshal(agent.ToolOutputFact{CallID: call.Call.CallID, ToolOutputDelta: agent.ToolOutputDelta{ToolCallID: call.Call.CallID, Stream: "output", Text: "old segment"}, StreamID: "old-stream", ChunkSeq: 1})
		view := f.manager.View()
		err := f.s.rt.CommitFact(t.Context(), call.Scope, agent.Fact{Kind: "tool_output", Payload: body})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStateConflict || !reflect.DeepEqual(view, f.manager.View()) {
			t.Fatalf("old output=%v", err)
		}
		if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
			if len(rt.active.toolChunks) != 0 {
				t.Error("old output polluted resumed stream")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	close(f.toolRelease)
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.runs.Load() != 1 || f.model.Calls() != 2 {
		t.Fatal("late old output restarted work")
	}
}

func TestCancelWaitsForResumedToolToExit(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{BlockResumedTool: true})
	before := f.manager.View()
	receipt, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed tool did not enter")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	err = f.s.Cancel(ctx, f.input.TraceID)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel blocked tool=%v", err)
	}
	tr := f.manager.View().Traces[f.input.TraceID]
	if tr.State != "cancelling" || tr.ExecutionStopped || tr.Settled {
		t.Fatalf("early stopped proof=%+v", tr)
	}
	close(f.toolRelease)
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	tr = f.manager.View().Traces[f.input.TraceID]
	status, err := f.s.GetOperation(t.Context(), receipt.OperationID)
	if err != nil || status.State != "cancelled" || tr.State != "cancelled" || !tr.ExecutionStopped || !tr.Settled || f.model.Calls() != 1 || f.runs.Load() != 1 {
		t.Fatalf("resumed cancel trace=%+v operation=%+v err=%v", tr, status, err)
	}
}
