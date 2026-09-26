package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/testkit"
)

type resumeCommitFault struct {
	store.Store
	store.CheckpointBlobs
	persistFirst bool
}

func (s resumeCommitFault) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	for _, rec := range commit.ControlRecords {
		if rec.Type != "operation" {
			continue
		}
		var op state.Operation
		if err := json.Unmarshal(rec.Payload, &op); err != nil {
			return store.CommitReceipt{}, err
		}
		if op.Kind == "resume" {
			if s.persistFirst {
				if _, err := s.Store.Append(ctx, id, expected, commit); err != nil {
					return store.CommitReceipt{}, err
				}
			}
			return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "resume commit acknowledgement unavailable")
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestResumeCommitFailureStartsNothingAndLostResponseCannotReuseOldPoint(t *testing.T) {
	for _, persistFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-append", true: "after-append"}[persistFirst], func(t *testing.T) {
			f := pausedResumeFixture(t, false, resumeFixtureOptions{Disk: true})
			faults := resumeCommitFault{Store: f.s.rt.opts.Store, CheckpointBlobs: f.s.rt.opts.Store.(store.CheckpointBlobs), persistFirst: persistFirst}
			manager, err := state.NewManager(faults, f.opts.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error { rt.manager = manager; rt.opts.Store = faults; return nil }); err != nil {
				t.Fatal(err)
			}
			before := manager.View()
			receipt, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "lost-reply"})
			if err == nil || receipt.OperationID != "" || !reflect.DeepEqual(before, manager.View()) || f.model.Calls() != 1 || f.runs.Load() != 0 {
				t.Fatalf("failed resume receipt=%+v err=%v", receipt, err)
			}
			if err := f.s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			model := versionedPauseModel{testkit.NewFake(testkit.Step{Text: "must not auto-run"})}
			f.opts.Model = model
			opened, err := OpenAgentSession(t.Context(), f.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close(context.Background())
			view := opened.rt.manager.View()
			if model.Calls() != 0 || f.runs.Load() != 0 {
				t.Fatal("Open restarted an accepted resume")
			}
			if persistFirst {
				if view.Traces[f.input.TraceID].ExecutionStopped || view.Traces[f.input.TraceID].CheckpointID != "" || view.Traces[f.input.TraceID].State != "paused" {
					t.Fatalf("crashed accepted resume=%+v", view.Traces[f.input.TraceID])
				}
				_, err := opened.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: view.LastSeq})
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIncompatibleResume {
					t.Fatalf("consumed checkpoint resume=%v", err)
				}
				again, err := opened.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "lost-reply"})
				if err != nil || again.OperationID == "" {
					t.Fatalf("lost accepted receipt=%+v err=%v", again, err)
				}
				status, err := opened.GetOperation(t.Context(), again.OperationID)
				if err != nil || status.State != "accepted" || model.Calls() != 0 {
					t.Fatalf("accepted interrupted operation=%+v err=%v", status, err)
				}
			} else {
				if !view.Traces[f.input.TraceID].ExecutionStopped || view.LastSeq != before.LastSeq {
					t.Fatal("failed append consumed checkpoint")
				}
			}
		})
	}
}

func TestConcurrentResumeAcceptsOneExecution(t *testing.T) {
	f := pausedResumeFixture(t, false)
	before := f.manager.View()
	cmd := ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq, IdempotencyKey: "concurrent"}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			receipt, err := f.s.Resume(t.Context(), cmd)
			if err != nil || receipt.OperationID == "" {
				t.Errorf("concurrent resume=%+v err=%v", receipt, err)
			}
		})
	}
	wg.Wait()
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	view := f.manager.View()
	count := 0
	for _, op := range view.Operations {
		if op.Kind == "resume" {
			count++
		}
	}
	if count != 1 || f.model.Calls() != 2 || f.runs.Load() != 1 {
		t.Fatalf("accepted resumes=%d model=%d tools=%d", count, f.model.Calls(), f.runs.Load())
	}
}

func TestResumeRejectsUnknownEffectsWithoutAcceptance(t *testing.T) {
	f := pausedResumeFixture(t, false)
	view := f.manager.View()
	for _, call := range view.Calls {
		call.Observation = &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown"}
		if err := f.manager.SaveCall(t.Context(), call); err != nil {
			t.Fatal(err)
		}
	}
	before := f.manager.View()
	_, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired || !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 || f.model.Calls() != 1 {
		t.Fatalf("unknown-effect resume=%v", err)
	}
}
