package state_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

func TestHistoricalProcessFinishToolsModelBudget(t *testing.T) {
	for _, source := range []string{"observation", "projection", "effective"} {
		for _, unit := range []string{`"`, `\`, `<`, "中🙂<"} {
			t.Run(fmt.Sprintf("%s/%q", source, unit), func(t *testing.T) {
				_, store := fixture(t)
				original := agent.ToolObservation{Status: "failed", Process: true, Executed: true, Terminated: true, ExitCode: 23, SideEffect: "confirmed", Content: "HEAD" + strings.Repeat(unit, 51000/len(unit)) + "TAIL", Truncated: true, LogError: "resource_unavailable: full process log was not saved"}
				seedP2Call(t, store, &original)
				manager, err := state.NewManager(store, "session")
				if err != nil {
					t.Fatal(err)
				}
				projection := agent.ToolOutputProjection{CallID: "call", Observation: original, Content: original.Content, Truncated: true, Artifact: agent.ArtifactRef{ID: "artifact:complete", SessionID: "session", Environment: "memory", Hash: "digest", Size: 60000, Available: true}}
				if source == "projection" {
					if err := manager.SaveToolProjection(t.Context(), projection); err != nil {
						t.Fatal(err)
					}
				}
				if source == "effective" {
					first, err := manager.LatestObservation("call")
					if err != nil {
						t.Fatal(err)
					}
					receipt, err := manager.AcceptOperation(t.Context(), state.OperationCommand{Kind: "reconcile", Target: "call", Content: json.RawMessage(`{}`), ExpectedRevision: 1})
					if err != nil {
						t.Fatal(err)
					}
					effective := original
					effective.Content = "HEAD-effective" + effective.Content + "TAIL"
					next := state.ObservationRevision{ID: "observation-2", CallID: "call", PreviousID: first.ID, Version: 2, Observation: effective, DetailsRef: "details"}
					reconciliation := state.Reconciliation{ID: "reconciliation", OperationID: receipt.OperationID, CallID: "call", ObservationID: first.ID, ObservationVersion: 1, NewObservationID: next.ID, EvidenceRefs: []string{"evidence"}, EvidenceSource: "executor"}
					if err := manager.AppendObservation(t.Context(), 1, next, &reconciliation); err != nil {
						t.Fatal(err)
					}
				}
				turn := agent.TurnRecord{ID: "turn", TraceID: "trace", InvocationID: "invocation", CallIDs: []string{"call"}}
				if err := manager.SaveTurn(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
				manager, err = state.NewManager(store, "session")
				if err != nil {
					t.Fatal(err)
				}
				before := manager.View()
				if err := manager.FinishTools(t.Context(), turn); err != nil {
					t.Fatal(err)
				}
				after := manager.View()
				if !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.ToolProjections, after.ToolProjections) || !reflect.DeepEqual(before.Observations, after.Observations) || !reflect.DeepEqual(before.Budget, after.Budget) {
					t.Fatal("model reconstruction mutated durable execution facts")
				}
				if len(after.Messages) != 1 {
					t.Fatalf("messages=%d", len(after.Messages))
				}
				text := after.Messages[0].Standard.ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
				if len(text) > 51200 || !json.Valid([]byte(text)) {
					t.Errorf("restored model JSON bytes=%d valid=%v", len(text), json.Valid([]byte(text)))
				}
				var body struct {
					Content   string
					Truncated bool
					Artifact  agent.ArtifactRef
				}
				if err := json.Unmarshal([]byte(text), &body); err != nil {
					t.Fatal(err)
				}
				if !body.Truncated || !utf8.ValidString(body.Content) || !strings.HasPrefix(body.Content, "HEAD") || !strings.HasSuffix(body.Content, "TAIL") {
					t.Error("UTF-8 head/tail or truncation was lost")
				}
				if source == "projection" && body.Artifact != projection.Artifact {
					t.Error("artifact identity changed")
				}
				if source == "effective" && !strings.HasPrefix(body.Content, "HEAD-effective") {
					t.Error("effective observation was ignored")
				}
				if err := manager.FinishTools(t.Context(), turn); err != nil || manager.View().LastSeq != after.LastSeq {
					t.Fatal("reconstruction repeated commits", err)
				}
				reopened, err := state.NewManager(store, "session")
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(after.Messages, reopened.View().Messages) || *reopened.View().Calls["call"].Observation != original {
					t.Fatal("persisted message or original execution facts changed on replay")
				}
			})
		}
	}
}
