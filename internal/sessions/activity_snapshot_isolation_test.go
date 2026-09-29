package sessions

import (
	"context"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

func TestActivityConcurrentSnapshotsOwnMutableObjects(t *testing.T) {
	var backend *activityStore
	s, manager, clock, model := activitySession(t, 3*time.Second, func(st store.Store) store.Store {
		backend = &activityStore{Store: st, committed: make(chan state.ActivityBudget, 10)}
		return backend
	})
	r := activitySubmit(t, s)
	activityStarted(t, model)
	frame := activityFrame(t, s)
	<-backend.committed
	owned, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	originalContent := string(owned.Inputs[r.InputID].Content)
	if originalContent != `{"text":"hi"}` {
		t.Fatalf("unexpected input fixture: %s", originalContent)
	}
	started, stop, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		mutate := func() {
			owned.Traces[r.TraceID].State = "tampered"
			owned.Traces[r.TraceID].Activity.Reserved = 0
			owned.Inputs[r.InputID].Content[9] = 'x'
			owned.Traces["snapshot-only"] = &state.TraceState{ID: "snapshot-only"}
		}
		mutate()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				mutate()
				goruntime.Gosched()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	awaitActivitySignal(t, started)
	// This deterministic safety test uses the existing lease fixture; the
	// independent real-clock polling/performance tests remain unchanged.
	clock.advance(500 * time.Millisecond)
	select {
	case <-backend.committed:
	case <-time.After(time.Second):
		t.Fatal("renewal missing")
	}
	for i := 0; i < 8; i++ {
		fresh, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		tr := fresh.Traces[r.TraceID]
		if tr.State != "running" || tr.Activity.Revision != 2 || tr.Activity.Reserved != time.Second || string(fresh.Inputs[r.InputID].Content) != originalContent || fresh.Traces["snapshot-only"] != nil {
			t.Fatalf("snapshot shared caller-owned data: trace=%+v content=%s", tr, fresh.Inputs[r.InputID].Content)
		}
		// A second independently returned snapshot must also be safe to mutate.
		fresh.Traces[r.TraceID].State = "another-caller"
		fresh.Inputs[r.InputID].Content[9] = 'y'
	}
	view := manager.View()
	if view.Traces[r.TraceID].State != "running" || string(view.Inputs[r.InputID].Content) != originalContent || view.Traces["snapshot-only"] != nil || model.calls.Load() != 1 {
		t.Fatal("concurrent snapshot mutation reached committed state or invoked model")
	}
	if err := s.rt.do(context.Background(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	model.release <- struct{}{}
	activityWait(t, frame)
}
