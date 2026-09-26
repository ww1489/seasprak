package sessions

import (
	"encoding/json"
	"testing"
)

func TestSnapshotComputesResumeEligibilityWithoutExecutionOrCommit(t *testing.T) {
	f := pausedResumeFixture(t, false)
	before := f.manager.View()
	snap, err := f.s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		Revision uint64
		Resume   map[string]struct {
			CanResume    bool
			Code, Reason string
		}
	}
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	if public.Revision != before.LastSeq {
		t.Fatalf("snapshot revision=%d want committed revision=%d", public.Revision, before.LastSeq)
	}
	eligibility, ok := public.Resume[f.input.TraceID]
	if !ok || !eligibility.CanResume || eligibility.Code != "" || eligibility.Reason != "" {
		t.Fatalf("snapshot lacks derived resume eligibility: %+v", public.Resume)
	}
	if f.manager.View().LastSeq != before.LastSeq || f.model.Calls() != 1 || f.runs.Load() != 0 {
		t.Fatal("resume eligibility ran work or wrote the journal")
	}
}
