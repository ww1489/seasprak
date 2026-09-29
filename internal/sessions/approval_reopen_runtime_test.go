package sessions

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// resumeForFreshApproval exercises the first explicit recovery step without
// answering it. Original call identities, results and budget must stay intact.
func resumeForFreshApproval(t *testing.T, s *AgentSession, traceID string) Snapshot {
	t.Helper()
	before := approvalSnapshot(t, s)
	if len(before.Interactions) != 0 || len(before.Approvals) != 0 {
		t.Fatal("Open restored approval before explicit Resume")
	}
	if _, err := s.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: before.Revision}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		tr := s.rt.manager.View().Traces[traceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	after := approvalSnapshot(t, s)
	if after.Traces[traceID].State != "paused" || len(after.Interactions) == 0 || !reflect.DeepEqual(before.Calls, after.Calls) || !reflect.DeepEqual(before.Traces[traceID].Usage, after.Traces[traceID].Usage) {
		t.Fatal("unanswered recovery did not preserve original calls and budget")
	}
	return after
}

func TestApprovalDiskOpenDoesNotAskOrWrite(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "writable"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			s, opts, process, _, traceID := capabilityApprovalFixture(t, true)
			answerCommand(t, s, "allowed-once")
			before := s.rt.manager.View()
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(opts.StateRoot, "sessions", opts.SessionID, "journal.jsonl")
			journal, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			opts.ReadOnly = readOnly
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			snap := approvalSnapshot(t, opened)
			if len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
				t.Error("Open created pending approval before explicit Resume")
			}
			if !reflect.DeepEqual(before, opened.rt.manager.View()) || process.calls.Load() != 0 || opts.Model.(versionedPauseModel).Calls() != 1 || snap.Traces[traceID].Usage.ToolExecutions != 0 {
				t.Error("Open changed durable state, model calls or tool budget")
			}
			if readOnly {
				_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: snap.Revision})
				requireSessionCode(t, err, product.CodePermissionDenied)
				if current := approvalSnapshot(t, opened); len(current.Interactions) != 0 || len(current.Approvals) != 0 {
					t.Error("rejected read-only Resume created an approval")
				}
			}
			if err := opened.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(journal, after) {
				t.Error("Open/Snapshot/Close changed the disk journal")
			}
		})
	}
}

func TestApprovalDiskReopenRevokedPolicyDoesNotAskOrExecute(t *testing.T) {
	s, opts, process, callID, traceID := capabilityApprovalFixture(t, true)
	old := answerCommand(t, s, "allowed-once")
	if err := s.rt.setExecutionPolicy(t.Context(), s.rt.manager.View().ExecutionPolicy.Revision, agent.ResolvedPolicy{ApprovalPolicy: "never"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	before := opened.rt.manager.View()
	_, err = opened.RespondInteraction(t.Context(), InteractionResponse{InteractionID: old.InteractionID, Decision: "allowed-once", ExpectedRevision: before.LastSeq})
	requireSessionCode(t, err, product.CodeNotFound)
	_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: before.LastSeq})
	requireSessionCode(t, err, product.CodePermissionDenied)
	snap := approvalSnapshot(t, opened)
	if len(snap.Interactions) != 0 || len(snap.Approvals) != 0 || !reflect.DeepEqual(before, opened.rt.manager.View()) || process.calls.Load() != 0 || opts.Model.(versionedPauseModel).Calls() != 1 || before.Calls[callID].Claimed || before.Calls[callID].Observation != nil || before.Traces[traceID].Usage.ToolExecutions != 0 {
		t.Fatal("revoked policy recovered old permission, asked or consumed work")
	}
}

func TestApprovalReopenExplicitResumeWithoutAnswerWaitsAgain(t *testing.T) {
	s, opts, process, callID, traceID := capabilityApprovalFixture(t, true)
	old := answerCommand(t, s, "allowed-once")
	original := s.rt.manager.View().Calls[callID]
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	before := opened.rt.manager.View()
	if process.calls.Load() != 0 || before.Traces[traceID].Usage.ToolExecutions != 0 {
		t.Fatal("opening consumed work")
	}
	_, err = opened.RespondInteraction(t.Context(), old)
	requireSessionCode(t, err, product.CodeNotFound)
	if _, err := opened.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: before.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		tr := opened.rt.manager.View().Traces[traceID]
		return tr.State == "paused" || terminal(tr.State)
	})
	waited := opened.rt.manager.View()
	if waited.Traces[traceID].State != "paused" || process.calls.Load() != 0 || waited.Calls[callID].Claimed || waited.Calls[callID].Observation != nil || waited.Traces[traceID].Usage.ToolExecutions != 0 || waited.Calls[callID].Scope != original.Scope || waited.Calls[callID].Call != original.Call {
		t.Fatalf("unanswered explicit resume did not wait: state=%s error=%s", waited.Traces[traceID].State, waited.Traces[traceID].Error)
	}
	answerCommand(t, opened, "allowed-once")
	if _, err := opened.Resume(t.Context(), ResumeCommand{TraceID: traceID, ExpectedRevision: waited.LastSeq}); err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[traceID].State) })
	after := opened.rt.manager.View()
	if after.Traces[traceID].State != "completed" || process.calls.Load() != 1 || after.Traces[traceID].Usage.ToolExecutions != 1 || after.Traces[traceID].Usage.LogicalModelCalls != 2 || after.Calls[callID].Observation == nil {
		t.Fatalf("fresh permission did not execute exactly once: %+v", after.Traces[traceID])
	}
}
