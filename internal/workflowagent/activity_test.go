package workflowagent

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestWorkflowActivityDeadlineDoesNotResetOnResume(t *testing.T) {
	var count atomic.Int32
	gate := make(chan struct{})
	defer close(gate)
	m := testkit.NewFake(testkit.Step{Text: "never accepted", Gate: gate, Repeat: true})
	opts := testOptions(t, modelThenTool(), m, &count)
	opts.Limits = config.Limits{ActivityBudget: 30 * time.Millisecond}
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	before := m.Calls()
	// The deadline may expire while durable admission is being committed,
	// especially under -race. Occupancy remains consumed if cancellation
	// wins after its commit but before the physical call starts.
	if s.State != "paused" || before > 1 || before > s.Usage.TransportRequests || s.Usage.TransportRequests > 1 || count.Load() != 0 {
		t.Fatalf("deadline %+v models=%d tools=%d", s, before, count.Load())
	}
	_, err := w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	requireCode(t, err, product.CodeBudgetExhausted)
	if m.Calls() != before || count.Load() != 0 {
		t.Fatal("activity exhaustion started another request")
	}
}
