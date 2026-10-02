package workflowagent

import (
	"sync/atomic"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowSnapshotProjectsItsAttemptAndEffectFacts(t *testing.T) {
	var calls atomic.Int32
	m := testkit.NewFake(testkit.Step{Text: "complete"})
	w := newWorkflow(t, testOptions(t, modelThenTool(), m, &calls))
	submit(t, w)
	s := waitStopped(t, w)
	if len(s.ModelAttempts) != 1 || len(s.Observations) != 1 {
		t.Fatalf("snapshot attempts=%d observations=%d", len(s.ModelAttempts), len(s.Observations))
	}
	for _, a := range s.ModelAttempts {
		if a.Status != "accepted" || a.NodeExecutionID == "" || a.ModelCallID != a.NodeExecutionID || a.FailureCode != "" {
			t.Fatalf("attempt %+v", a)
		}
	}
	for _, o := range s.Observations {
		if o.Status != "succeeded" || o.SideEffect != "none" || !o.Executed || o.NodeExecutionID == "" {
			t.Fatalf("effect %+v", o)
		}
	}
	for id, o := range s.Observations {
		o.SideEffect = "unknown"
		s.Observations[id] = o
	}
	for id, a := range s.ModelAttempts {
		a.Status = "failed"
		s.ModelAttempts[id] = a
	}
	again, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range again.Observations {
		if o.SideEffect == "unknown" {
			t.Error("snapshot mutated owner")
		}
	}
	for _, a := range again.ModelAttempts {
		if a.Status == "failed" {
			t.Error("snapshot mutated attempt")
		}
	}
	bad := testkit.NewFake(testkit.Step{Truncated: true, Text: "partial"})
	w2 := newWorkflow(t, testOptions(t, modelThenTool(), bad, &calls))
	submit(t, w2)
	s = waitStopped(t, w2)
	for _, a := range s.ModelAttempts {
		if a.Status != "incomplete" || a.FailureCode != product.CodeInvalidArgument {
			t.Fatalf("failed attempt %+v", a)
		}
	}
	if calls.Load() != 1 || bad.Calls() != 1 {
		t.Fatal("invalid attempt invoked later tool")
	}
}
