package state_test

import (
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestRuntimeApprovedToolClaimKeepsAtomicIntentAndBudget(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "commit-failure"
		}
		t.Run(name, func(t *testing.T) {
			f := approvalCall(t)
			v := f.m.View()
			cp := state.CheckpointRef{ID: "runtime-checkpoint", BlobHash: "saved-blob", BlobSize: 100, Scope: f.call.Scope, Target: v.Traces[f.call.Scope.TraceID].Target, HistoryCommit: v.LastSeq, ProjectionRevision: v.LastSeq, LeafID: v.LeafID, UnfinishedTurnIDs: []string{f.call.Scope.TurnID}, CallIDs: []string{f.call.Call.CallID}, ApprovalTargets: map[string]string{f.call.Call.CallID: "eino-target"}}
			if err := f.m.CommitToolWaitCheckpoint(t.Context(), f.call.Scope.TraceID, "", cp, cp.ApprovalTargets); err != nil {
				t.Fatal(err)
			}
			if _, err := f.m.CommitResume(t.Context(), state.OperationCommand{Kind: "resume", Target: f.call.Scope.TraceID, ExpectedRevision: f.m.View().LastSeq}, cp.ID, "new-execution"); err != nil {
				t.Fatal(err)
			}
			before := f.m.View()
			usage := before.Traces[f.call.Scope.TraceID].Usage
			usage.ToolExecutions++
			f.store.fail = fail
			err := f.m.ClaimRuntimeApprovedTool(t.Context(), f.call.Call, usage, "new-execution")
			if fail {
				if err == nil || !reflect.DeepEqual(before, f.m.View()) {
					t.Fatal("failed atomic claim changed intent or usage")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			saved, err := f.store.Load(t.Context(), "session")
			if err != nil {
				t.Fatal(err)
			}
			records := saved.Commits[len(saved.Commits)-1].ControlRecords
			if len(records) != 2 || records[0].Type != "trace" || records[1].Type != "tool_call" {
				t.Fatalf("claim and usage are not the same permission-free commit: %+v", records)
			}
			after := f.m.View()
			if !after.Calls[f.call.Call.CallID].Claimed || after.Traces[f.call.Scope.TraceID].Usage != usage || len(after.ApprovalClaims) != 0 || len(after.ApprovalDecisions) != 0 {
				t.Fatal("claim did not preserve atomic facts")
			}
			reopened, err := state.NewManager(f.store, "session")
			if err != nil {
				t.Fatal(err)
			}
			recovered := reopened.View()
			if !recovered.TraceHasUnresolvedEffects(f.call.Scope.TraceID) || recovered.Traces[f.call.Scope.TraceID].Usage != usage {
				t.Fatal("replay lost unknown claim or budget")
			}
			err = reopened.ClaimRuntimeApprovedTool(t.Context(), f.call.Call, usage, "new-execution")
			requireP2Code(t, err, product.CodeStateConflict)
			if !reflect.DeepEqual(recovered, reopened.View()) {
				t.Fatal("repeated runtime claim changed recovered facts")
			}
		})
	}
}
