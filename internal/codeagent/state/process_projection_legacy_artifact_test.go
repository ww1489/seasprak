package state_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

func TestHistoricalProcessFinishToolsOmitsOversizedArtifactOnlyInModel(t *testing.T) {
	for _, id := range []string{strings.Repeat("a", 52000), strings.Repeat("<", 9000)} {
		for _, content := range []string{"", "HEAD" + strings.Repeat("中🙂<", 7000) + "TAIL"} {
			t.Run(fmt.Sprintf("id=%d/content=%d", len(id), len(content)), func(t *testing.T) {
				_, store := fixture(t)
				original := agent.ToolObservation{Status: "failed", Process: true, Executed: true, Terminated: true, ExitCode: 23, SideEffect: "confirmed", Content: content, Truncated: true}
				seedP2Call(t, store, &original)
				manager, err := state.NewManager(store, "session")
				if err != nil {
					t.Fatal(err)
				}
				projection := agent.ToolOutputProjection{CallID: "call", Observation: original, Content: content, Artifact: agent.ArtifactRef{ID: id, SessionID: "session", Environment: "memory", Hash: "complete-digest", Size: 60000, Available: true}}
				if err := manager.SaveToolProjection(t.Context(), projection); err != nil {
					t.Fatal(err)
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
				if !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.ToolProjections, after.ToolProjections) || !reflect.DeepEqual(before.Observations, after.Observations) || before.Budget != after.Budget {
					t.Fatal("model output changed original durable facts")
				}
				if len(after.Messages) != 1 {
					t.Fatalf("messages=%d", len(after.Messages))
				}
				model := after.Messages[0].Standard.ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
				if len(model) > 51200 || !json.Valid([]byte(model)) {
					t.Errorf("model bytes=%d validJSON=%v", len(model), json.Valid([]byte(model)))
				}
				var body struct {
					Content   string
					Truncated bool
					Artifact  *agent.ArtifactRef
					LogError  string
				}
				if err := json.Unmarshal([]byte(model), &body); err != nil {
					t.Fatal(err)
				}
				if body.Artifact != nil || !body.Truncated || body.LogError != "resource_unavailable: process log reference omitted due to model output limit" {
					t.Error("model did not report reference omission accurately")
				}
				if content != "" && (!strings.HasPrefix(body.Content, "HEAD") || !strings.HasSuffix(body.Content, "TAIL")) {
					t.Error("reference omission discarded the log preview")
				}
				if err := manager.FinishTools(t.Context(), turn); err != nil || manager.View().LastSeq != after.LastSeq {
					t.Fatal("repeated recovery committed again", err)
				}
				reopened, err := state.NewManager(store, "session")
				if err != nil {
					t.Fatal(err)
				}
				view := reopened.View()
				if view.ToolProjections["call"] != projection || *view.Calls["call"].Observation != original || !reflect.DeepEqual(view.Messages, after.Messages) {
					t.Fatal("persisted reference, facts or bounded message changed on replay")
				}
			})
		}
	}
}
