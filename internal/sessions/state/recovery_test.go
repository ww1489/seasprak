package state_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestExecutionStoppedConfirmationIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	m, backend := fixture(t)
	r := accept(t, m, "stopped", `{"text":"run"}`)
	if err := m.ConfirmExecutionStopped(ctx, r.TraceID); err == nil {
		t.Fatal("queued execution cannot have a stopped worker")
	}
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTraceState(ctx, r.TraceID, "paused", false); err != nil {
		t.Fatal(err)
	}
	if m.View().Traces[r.TraceID].ExecutionStopped {
		t.Fatal("paused state alone fabricated stopped evidence")
	}
	if err := m.ConfirmExecutionStopped(ctx, r.TraceID); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	if !before.Traces[r.TraceID].ExecutionStopped {
		t.Fatal("stopped evidence missing")
	}
	if err := m.ConfirmExecutionStopped(ctx, r.TraceID); err != nil || !reflect.DeepEqual(before, m.View()) {
		t.Fatalf("duplicate confirmation changed facts: %v", err)
	}
	reopened, err := state.NewManager(backend, "session")
	if err != nil || !reflect.DeepEqual(before, reopened.View()) {
		t.Fatalf("stopped evidence did not replay: %v", err)
	}
}

func TestExecutionStoppedFailedCommitDoesNotConfirm(t *testing.T) {
	ctx := context.Background()
	m, backend := fixture(t)
	r := accept(t, m, "failure", `{"text":"run"}`)
	if err := m.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	before := m.View()
	backend.fail = true
	if err := m.ConfirmExecutionStopped(ctx, r.TraceID); err == nil {
		t.Fatal("expected stopped evidence commit failure")
	}
	if !reflect.DeepEqual(before, m.View()) {
		t.Fatal("failed persistence exposed stopped evidence")
	}
	if pe, ok := product.AsError(m.Fault()); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatalf("commit failure did not fault the manager: %v", m.Fault())
	}
}

func TestUnresolvedEffectsDistinguishInFlightFromUnknown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		trace      string
		call       agent.ToolRecord
		conflicts  bool
		unresolved bool
	}{
		{name: "unclaimed", trace: "paused"},
		{name: "running_claim", trace: "running", call: agent.ToolRecord{Claimed: true}, unresolved: true},
		{name: "paused_claim", trace: "paused", call: agent.ToolRecord{Claimed: true}, conflicts: true, unresolved: true},
		{name: "terminal_claim", trace: "cancelled", call: agent.ToolRecord{Claimed: true}, conflicts: true, unresolved: true},
		{name: "unknown_effect", trace: "cancelled", call: agent.ToolRecord{Claimed: true, Observation: &agent.ToolObservation{Status: "failed", SideEffect: "unknown"}}, conflicts: true, unresolved: true},
		{name: "unknown_outcome", trace: "running", call: agent.ToolRecord{Claimed: true, Observation: &agent.ToolObservation{Status: "outcome_unknown", SideEffect: "none"}}, conflicts: true, unresolved: true},
		{name: "known_result", trace: "paused", call: agent.ToolRecord{Claimed: true, Observation: &agent.ToolObservation{Status: "succeeded", SideEffect: "none"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.call.Scope.TraceID = "trace"
			v := state.View{Traces: map[string]*state.TraceState{"trace": {ID: "trace", State: tc.trace}}, Calls: map[string]agent.ToolRecord{"call": tc.call}}
			if got := v.HasUnresolvedEffects(); got != tc.conflicts {
				t.Fatalf("conflict=%v, want %v", got, tc.conflicts)
			}
			if got := v.TraceHasUnresolvedEffects("trace"); got != tc.unresolved {
				t.Fatalf("trace unresolved=%v, want %v", got, tc.unresolved)
			}
			if v.TraceHasUnresolvedEffects("other") {
				t.Fatal("call assigned to another trace")
			}
		})
	}
}
