package state

import (
	"encoding/json"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestP2SelectionDefaultOperationBinding(t *testing.T) {
	content := json.RawMessage(`{"name":"B","version":"B-v1"}`)
	digest, err := operationDigest(OperationCommand{Kind: "select_default_model", Target: "next_trace", ExpectedRevision: 9, Content: content})
	if err != nil {
		t.Fatal(err)
	}
	original := Selection{ID: "selection", OperationID: "operation", Revision: 10, Scope: agent.ExecutionScope{SessionID: "session", BranchID: "branch", Generation: "gen"}, Kind: "model", ApplyAt: "next_trace", State: "active", ModelName: "B", ModelVersion: "B-v1"}
	accepted := Operation{Receipt: OperationReceipt{OperationID: "operation", Target: "next_trace", AcceptedCommit: 10}, SessionID: "session", Kind: "select_default_model", Digest: digest}
	for _, mutation := range []string{"unchanged", "name", "version", "trace", "invocation", "execution", "turn", "selection-revision", "apply-at", "kind", "operation-kind", "target", "revision", "operation-id", "session", "digest"} {
		t.Run(mutation, func(t *testing.T) {
			selected, op := original, accepted
			switch mutation {
			case "name":
				selected.ModelName = "C"
			case "version":
				selected.ModelVersion = "B-v2"
			case "trace":
				selected.Scope.TraceID = "trace"
			case "invocation":
				selected.Scope.InvocationID = "invocation"
			case "execution":
				selected.Scope.ExecutionID = "execution"
			case "turn":
				selected.Scope.TurnID = "turn"
			case "selection-revision":
				selected.Scope.SelectionRevision = 10
			case "apply-at":
				selected.ApplyAt = "next_turn"
			case "kind":
				selected.Kind = "tools"
			case "operation-kind":
				op.Kind = "select_next_turn_model"
			case "target":
				op.Receipt.Target = "trace"
			case "revision":
				selected.Revision++
				op.Receipt.AcceptedCommit++ // The original request revision remains bound by the digest.
			case "operation-id":
				selected.OperationID = "different"
			case "session":
				selected.Scope.SessionID = "different"
			case "digest":
				op.Digest = "different"
			}
			if got := SelectionMatchesOperation(selected, op); got != (mutation == "unchanged") {
				t.Fatalf("binding=%v for %s", got, mutation)
			}
		})
	}
}
