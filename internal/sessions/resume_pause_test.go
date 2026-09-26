package sessions

import (
	"context"
	"testing"
	"time"
)

func TestResumedExecutionCanPauseAgainAfterOriginalToolCompletes(t *testing.T) {
	f := pausedResumeFixture(t, false, resumeFixtureOptions{BlockResumedTool: true})
	before := f.manager.View()
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.toolEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("resumed original tool did not start")
	}
	pauseResult := make(chan error, 1)
	go func() { _, err := f.s.Pause(context.Background(), f.input.TraceID); pauseResult <- err }()
	waitResumeCondition(t, func() bool { return len(f.manager.View().Operations) == 3 })
	close(f.toolRelease)
	select {
	case err := <-pauseResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second pause did not complete")
	}
	snap, err := f.s.Snapshot(t.Context())
	if err != nil || !snap.Resume[f.input.TraceID].CanResume {
		t.Fatalf("second pause lost original model identity: eligibility=%+v err=%v", snap.Resume, err)
	}
	if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: snap.Revision}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
	if f.model.Calls() != 2 || f.runs.Load() != 1 || f.manager.View().Traces[f.input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("repeated pause restarted work: models=%d tools=%d", f.model.Calls(), f.runs.Load())
	}
}
