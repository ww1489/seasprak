package sessions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type blockedApprovalBlobStore struct {
	store.Store
	store.CheckpointBlobs
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedApprovalBlobStore) Put(ctx context.Context, id string, data []byte) (store.BlobRef, error) {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.release:
		return s.CheckpointBlobs.Put(ctx, id, data)
	case <-ctx.Done():
		return store.BlobRef{}, ctx.Err()
	}
}

func TestPauseDuringApprovalCheckpointSavePreservesAnswerableCheckpoint(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := startApprovalSession(t, nil, func(backend store.Store) store.Store {
		return &blockedApprovalBlobStore{Store: backend, CheckpointBlobs: backend.(store.CheckpointBlobs), entered: entered, release: release}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("approval checkpoint save did not enter")
	}
	result := make(chan error, 1)
	go func() { _, err := f.s.Pause(context.Background(), f.input.TraceID); result <- err }()
	waitResumeCondition(t, func() bool {
		for _, op := range f.manager.View().Operations {
			if op.Kind == "pause" && op.State == "accepted" {
				return true
			}
		}
		return false
	})
	close(release)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("late accepted pause destroyed approval checkpoint: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late pause did not complete")
	}
	v := f.manager.View()
	tr := v.Traces[f.input.TraceID]
	cp := v.Checkpoints[tr.CheckpointID]
	if tr.State != "paused" || len(cp.ApprovalTargets) != 1 || len(v.ApprovalBindings) != 0 || f.runs.Load() != 0 || tr.Usage.ToolExecutions != 0 {
		t.Fatalf("late pause lost its original target: %+v", tr)
	}
	answerApproval(t, f, "allowed-once")
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.manager.View().Traces[f.input.TraceID].State != "completed" || f.runs.Load() != 1 || f.model.Calls() != 2 {
		t.Fatal("late pause could not restore original approved call")
	}
}

func TestPauseDuringApprovalPreparationPreservesAnswerableCheckpoint(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f := startApprovalSession(t, func(context.Context, agent.FrozenExecution) error {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return nil
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("approval preparation did not enter")
	}
	type pauseResult struct {
		receipt state.OperationReceipt
		err     error
	}
	result := make(chan pauseResult, 1)
	go func() {
		receipt, err := f.s.Pause(context.Background(), f.input.TraceID)
		result <- pauseResult{receipt, err}
	}()
	waitResumeCondition(t, func() bool {
		for _, op := range f.manager.View().Operations {
			if op.Kind == "pause" && op.State == "accepted" {
				return true
			}
		}
		return false
	})
	close(release)
	select {
	case paused := <-result:
		if paused.err != nil {
			t.Fatalf("pause did not preserve approval waiting: %v", paused.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not finish")
	}
	v := f.manager.View()
	tr := v.Traces[f.input.TraceID]
	cp := v.Checkpoints[tr.CheckpointID]
	if tr.State != "paused" || len(cp.ApprovalTargets) != 1 || len(v.ApprovalBindings) != 0 || f.runs.Load() != 0 || f.model.Calls() != 1 {
		t.Fatalf("pause lost approval target: trace=%+v interactions=%v bindings=%d", tr, cp.InteractionIDs, len(v.ApprovalBindings))
	}
	answerApproval(t, f, "allowed-once")
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.runs.Load() != 1 || f.model.Calls() != 2 || f.manager.View().Traces[f.input.TraceID].State != "completed" {
		t.Fatal("mixed pause/approval did not resume the original call exactly once")
	}
}
