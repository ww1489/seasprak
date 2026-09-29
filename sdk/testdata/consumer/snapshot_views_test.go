package consumer_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/sdk"
)

// Step 22 requires queryable attempts and versioned observations. Exercise a
// real accepted model/tool turn before checking the public snapshot, rather
// than proving only that an empty DTO has fields.
func TestSDKConsumerSnapshotIncludesAttemptAndObservationViews(t *testing.T) {
	var runs atomic.Int32
	model := &consumerOutputModel{}
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		return "known result", nil
	}}
	def.Execution.Effect, def.Execution.BackendID = "read", "trusted-run"
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-snapshot-views", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model, Tools: []sdk.ToolDefinition{def}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"run work"}`)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := waitConsumerApprovalState(t, s, input.TraceID, "completed")
	if runs.Load() != 1 || model.calls.Load() != 2 || len(snapshot.Calls) != 1 {
		t.Fatal("fixture did not run the accepted model/tool pipeline")
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		ModelAttempts map[string]struct{ ID, State string }
		Observations  map[string]struct {
			ID, CallID string
			Version    uint64
		}
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	t.Run("attempts", func(t *testing.T) {
		if len(view.ModelAttempts) != 2 {
			t.Fatalf("public snapshot must expose both model attempts; got %d", len(view.ModelAttempts))
		}
		for _, attempt := range view.ModelAttempts {
			if attempt.ID == "" || attempt.State != "accepted" {
				t.Fatal("public attempt view must include the committed terminal state, not only the initial started record")
			}
		}
	})
	t.Run("observations", func(t *testing.T) {
		if len(view.Observations) != 1 {
			t.Fatalf("public snapshot must expose the observation identity/version needed by Reconcile; got %d", len(view.Observations))
		}
		for _, observation := range view.Observations {
			if observation.ID == "" || observation.Version != 1 || snapshot.Calls[observation.CallID].Observation == nil {
				t.Fatal("observation cannot be bound to the original call")
			}
		}
	})
}
