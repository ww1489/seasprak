package codeagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	"github.com/ww1489/seasprak/internal/config"
)

func TestApprovalRuntimeDeadlineRetainsMonotonicTime(t *testing.T) {
	issued := time.Now()
	if issued == issued.Round(0) {
		t.Fatal("system clock did not supply a monotonic reading")
	}
	clock := &manualActivityClock{now: issued}
	rt := &runtime{clock: clock}
	pending := rt.newApproval(agent.FrozenExecution{ID: "execution:clock", CallID: "clock", Hash: "frozen"})
	deadline := pending.approval.ExpiresAt
	if deadline == deadline.Round(0) {
		t.Error("runtime deadline stripped the monotonic reading; wall-clock corrections can reject a fresh approval")
	}
	if deadline.Sub(issued) != config.ApprovalValidity {
		t.Fatal("approval lifetime differs from 24 hours")
	}
	interactions, approvals := rt.snapshotApprovals(state.View{})
	if interactions[pending.interaction.ID].ExpiresAt != deadline.UTC() || approvals[pending.approval.ID].ExpiresAt != deadline.UTC() {
		t.Fatal("public snapshot deadline is not the equivalent UTC timestamp")
	}
	if pending.approval.ExpiresAt != deadline || pending.interaction.ExpiresAt != deadline {
		t.Fatal("snapshot conversion changed the runtime deadline")
	}
	if approvals[pending.approval.ID].State != "asked" {
		t.Fatal("fresh approval was not valid in snapshot")
	}
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		valid   bool
	}{
		{"before-issue", -time.Nanosecond, false},
		{"issued", 0, true},
		{"before-deadline", config.ApprovalValidity - time.Nanosecond, true},
		{"at-deadline", config.ApprovalValidity, false},
		{"after-deadline", config.ApprovalValidity + time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := approvalWithinValidity(issued.Add(tc.elapsed), pending.approval); got != tc.valid {
				t.Fatalf("valid=%t want=%t elapsedNs=%d", got, tc.valid, tc.elapsed)
			}
		})
	}
}

// approvalDiagnosticClock preserves the real clock and timer behavior. It only
// records samples so wall-clock corrections can be distinguished from elapsed
// time and from tests that deliberately replace the clock.
type approvalDiagnosticClock struct {
	systemActivityClock
	mu      sync.Mutex
	samples []time.Time
}

func (c *approvalDiagnosticClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.systemActivityClock.Now()
	c.samples = append(c.samples, now)
	return now
}

func diagnoseApprovalClock(t *testing.T, s *AgentSession) {
	t.Helper()
	clock := &approvalDiagnosticClock{}
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		if rt.clock != nil {
			t.Errorf("clock diagnostics expected unchanged system clock, got %T", rt.clock)
		} else {
			rt.clock = clock
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.rt.do(context.Background(), func(rt *runtime) error {
			clock.mu.Lock()
			samples := append([]time.Time(nil), clock.samples...)
			clock.mu.Unlock()
			var minWall, maxDrift time.Duration
			for i := 1; i < len(samples); i++ {
				wall := samples[i].Round(0).Sub(samples[i-1].Round(0))
				mono := samples[i].Sub(samples[i-1])
				if wall < minWall {
					minWall = wall
				}
				if drift := wall - mono; drift < -time.Millisecond || drift > time.Millisecond {
					t.Logf("clock step sample=%d wallDeltaNs=%d monotonicDeltaNs=%d", i, wall, mono)
				}
				if drift := wall - mono; drift < 0 {
					maxDrift = min(maxDrift, drift)
				}
			}
			t.Logf("approval clock samples=%d minWallStepNs=%d negativeDriftNs=%d sourceUnchanged=%t", len(samples), minWall, maxDrift, rt.clock == clock)
			if t.Failed() {
				v := rt.manager.View()
				for _, pending := range rt.approvals {
					a := pending.approval
					issued := a.ExpiresAt.Add(-config.ApprovalValidity)
					tr := v.Traces[a.Scope.TraceID]
					t.Logf("approval state=%s answered=%t claimed=%t tools=%d models=%d", tr.State, pending.decision != "", pending.claimedExecution != "", tr.Usage.ToolExecutions, tr.Usage.LogicalModelCalls)
					for i, sample := range samples {
						if i >= len(samples)-12 {
							t.Logf("approval sample=%d sinceIssueNs=%d untilExpiryNs=%d", i, sample.Sub(issued), a.ExpiresAt.Sub(sample))
						}
					}
				}
			}
			return nil
		}); err != nil {
			t.Errorf("approval diagnostics: %v", err)
		}
	})
}
