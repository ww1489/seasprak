package sessions

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func TestSnapshotAttemptAndObservationProjection(t *testing.T) {
	for _, status := range []string{"started", "accepted", "failed", "incomplete", "aborted"} {
		t.Run(status, func(t *testing.T) {
			initial := state.ModelAttempt{ID: "attempt", ModelCallID: "model-call", MessageID: "message", StreamID: "stream", State: "started", Attempt: 1, Scope: agent.ExecutionScope{TraceID: "trace"}}
			v := state.View{LastSeq: 9, Cursor: 7, ModelAttempts: map[string]state.ModelAttempt{initial.ID: initial}, AttemptResults: map[string]state.ModelAttemptTransition{}, Observations: map[string]state.ObservationRevision{
				"observation": {ID: "observation", CallID: "call", Version: 2, PreviousID: "previous", DetailsRef: "fixture-private-details", ArtifactRefs: []string{"fixture-private-artifact"}, StartEvidenceRef: "fixture-private-start", ExitRef: "fixture-private-exit", CancellationRef: "fixture-private-cancel", FileFactsRef: "fixture-private-file", Observation: agent.ToolObservation{Status: "outcome_unknown", SideEffect: "unknown", Executed: true, Process: true, ExitCode: -1, Content: "fixture-private-content", ExecutionError: "fixture-private-error", LogError: "fixture-private-log"}},
			}}
			if status != "started" {
				v.AttemptResults[initial.ID] = state.ModelAttemptTransition{AttemptID: initial.ID, State: status, FinishReason: "stop", UsageRef: "usage", DiagnosticRef: "diagnostic"}
			}
			rt := &runtime{opts: Options{SessionID: "session"}}
			snap := rt.snapshot(v, nil)
			got := snap.ModelAttempts[initial.ID]
			if got.State != status || got.ID != initial.ID || got.Scope != initial.Scope || got.StreamID != initial.StreamID || snap.Revision != 9 || snap.Cursor != 7 {
				t.Fatalf("attempt identity/state or snapshot position lost: %+v", got)
			}
			if status != "started" && (got.FinishReason != "stop" || got.UsageRef != "usage" || got.DiagnosticRef != "diagnostic") {
				t.Fatal("terminal references not projected")
			}
			obs := snap.Observations["observation"]
			if obs.ID != "observation" || obs.CallID != "call" || obs.Version != 2 || obs.PreviousID != "previous" || obs.Observation.Status != "outcome_unknown" || obs.Observation.SideEffect != "unknown" || !obs.Observation.Executed || !obs.Observation.Process || obs.Observation.ExitCode != -1 {
				t.Fatalf("observation facts lost: %+v", obs)
			}
			raw, err := json.Marshal(snap)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "fixture-private-") {
				t.Fatal("snapshot exposed private observation data")
			}
			delete(snap.ModelAttempts, initial.ID)
			obs.Observation.Status = "succeeded"
			snap.Observations[obs.ID] = obs
			if !reflect.DeepEqual(v.ModelAttempts[initial.ID], initial) || v.Observations[obs.ID].Observation.Status != "outcome_unknown" {
				t.Fatal("public mutation altered committed view")
			}
			again := rt.snapshot(v, nil)
			if again.ModelAttempts[initial.ID].State != status || again.Observations[obs.ID].Observation.Status != "outcome_unknown" {
				t.Fatal("next snapshot reused a mutable cache")
			}
		})
	}
}
