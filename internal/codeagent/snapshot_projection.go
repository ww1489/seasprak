package codeagent

import (
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

// ModelAttemptView describes committed attempt identity and its latest state.
// References identify evidence; their private contents are not exposed here.
type ModelAttemptView struct {
	ID, ModelCallID, MessageID, StreamID                             string
	Scope                                                            agent.ExecutionScope
	Purpose                                                          string
	Attempt, TransportAttempt                                        uint64
	State, ModelConfigVersion, FinishReason, DiagnosticRef, UsageRef string
}

// ObservationView provides the identity and version required by Reconcile.
// Raw tool output, backend error text and internal evidence references are omitted.
type ObservationView struct {
	ID, CallID  string
	Version     uint64
	PreviousID  string
	Observation agent.ToolObservation
}

func snapshotAttemptViews(v state.View) map[string]ModelAttemptView {
	out := make(map[string]ModelAttemptView, len(v.ModelAttempts))
	for id, a := range v.ModelAttempts {
		if terminal, ok := v.AttemptResults[id]; ok {
			a.State, a.FinishReason = terminal.State, terminal.FinishReason
			a.DiagnosticRef, a.UsageRef = terminal.DiagnosticRef, terminal.UsageRef
		}
		out[id] = ModelAttemptView{ID: a.ID, ModelCallID: a.ModelCallID, MessageID: a.MessageID, StreamID: a.StreamID, Scope: a.Scope, Purpose: a.Purpose, Attempt: a.Attempt, TransportAttempt: a.TransportAttempt, State: a.State, ModelConfigVersion: a.ModelConfigVersion, FinishReason: a.FinishReason, DiagnosticRef: a.DiagnosticRef, UsageRef: a.UsageRef}
	}
	return out
}

func snapshotObservationViews(v state.View) map[string]ObservationView {
	out := make(map[string]ObservationView, len(v.Observations))
	for id, r := range v.Observations {
		o := r.Observation
		out[id] = ObservationView{ID: r.ID, CallID: r.CallID, Version: r.Version, PreviousID: r.PreviousID, Observation: agent.ToolObservation{Status: o.Status, SideEffect: o.SideEffect, Executed: o.Executed, Process: o.Process, ExitCode: o.ExitCode, Terminated: o.Terminated, Truncated: o.Truncated}}
	}
	return out
}
