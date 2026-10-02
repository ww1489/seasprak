package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/storage/jsonl"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestP2ModelSwitchDiskRebindAcceptedModelPreservesBuild(t *testing.T) {
	for _, supplied := range []string{"A", "B"} {
		t.Run(supplied, func(t *testing.T) {
			entered, gate := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-gate:
				default:
					close(gate)
				}
			}()
			a := selectionConfiguredModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "gate", Name: "gate", Arguments: `{}`}}}), selectionTrustedConfig("A")}
			b := selectionConfiguredModel{testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "approved", Name: "work", Arguments: `{}`}}}), selectionTrustedConfig("B")}
			var gates, runs atomic.Int32
			opts := Options{SessionID: agent.MustID(), Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: ProfileMemory, Principal: "operator", Model: a, GenerationFingerprint: "selection-rebind-v1", Tools: []tools.Definition{
				{Name: "gate", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read"}, Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
					close(entered)
					select {
					case <-gate:
					case <-ctx.Done():
						return "", ctx.Err()
					}
					gates.Add(1)
					return "ready", nil
				}},
				{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", RequestedGrantRef: "one-operation"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "approved", nil }},
			}}
			s, err := CreateAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close(context.Background()) })
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"switch"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool {
				select {
				case <-entered:
					return true
				default:
					return false
				}
			})
			request := SelectNextTurnModelRequest{TraceID: input.TraceID, Model: ModelChoice{Model: b}, ExpectedRevision: s.rt.manager.View().LastSeq, IdempotencyKey: "accepted-B"}
			receipt, err := s.SelectNextTurnModel(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			close(gate)
			waitResumeCondition(t, func() bool {
				v := s.rt.manager.View()
				return v.Traces[input.TraceID].State == "paused" || terminal(v.Traces[input.TraceID].State)
			})
			view := s.rt.manager.View()
			cp := view.Checkpoints[view.Traces[input.TraceID].CheckpointID]
			if cp.ModelConfigVersion != "B-v1" || a.Calls() != 1 || b.Calls() != 1 || gates.Load() != 1 || runs.Load() != 0 {
				t.Fatal("B did not actually activate and pause for approval")
			}
			newDefault := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run default"}), selectionTrustedConfig("C")}
			defaultReceipt, err := s.SetDefaultModel(t.Context(), SetDefaultModelRequest{Model: ModelChoice{Model: newDefault}, ExpectedRevision: view.LastSeq})
			if err != nil {
				t.Fatal(err)
			}
			defaultSelection, ok := selectionForOperation(s.rt.manager.View(), defaultReceipt.OperationID)
			if !ok || defaultSelection.ID == "" || defaultSelection.Revision <= receipt.AcceptedCommit {
				t.Fatal("missing newer nonempty default selection")
			}
			if err = s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			rebuiltA := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run A"}), selectionTrustedConfig("A")}
			rebuiltB := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "resumed B"}), selectionTrustedConfig("B")}
			opts.Model = rebuiltA
			if supplied == "B" {
				opts.Model = rebuiltB
			}
			opened, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = opened.Close(context.Background()) })
			if _, ok := opened.rt.opts.Store.(*jsonl.Store); !ok || opened.rt.manager == s.rt.manager || opened.rt.opts.Store == s.rt.opts.Store {
				t.Fatal("reused old process state")
			}
			if snap := approvalSnapshot(t, opened); len(snap.Interactions) != 0 || len(snap.Approvals) != 0 {
				t.Fatal("Open created pending approval before Resume")
			}
			before := opened.rt.manager.View()
			_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: before.LastSeq})
			requireSessionCode(t, err, product.CodeIncompatibleResume)
			if !reflect.DeepEqual(before, opened.rt.manager.View()) || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 0 || runs.Load() != 0 {
				t.Fatal("unbound or wrong-build resume executed")
			}
			if supplied == "B" {
				return
			} // B cannot replace the original assembly/build A.
			selected, ok := selectionForOperation(before, receipt.OperationID)
			if !ok {
				t.Fatal("accepted selection missing")
			}
			for _, mutation := range []string{"stream", "output"} {
				t.Run(mutation, func(t *testing.T) {
					bad := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run invalid B"}), selectionTrustedConfig("B")}
					if mutation == "stream" {
						delete(bad.cfg.Capabilities.Items, llm.CapTextStream)
					} else {
						bad.cfg.Parameters.MaxOutputTokens = 1000
					}
					request.Model.Model = bad
					replay, err := opened.SelectNextTurnModel(t.Context(), request)
					if err != nil || replay != receipt || !reflect.DeepEqual(before, opened.rt.manager.View()) {
						t.Fatalf("invalid candidate changed historical receipt: %+v %v", replay, err)
					}
					assertReboundSelection(t, opened, selected.ID, defaultSelection.ID, nil)
					_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: before.LastSeq})
					requireSessionCode(t, err, product.CodeIncompatibleResume)
					if !reflect.DeepEqual(before, opened.rt.manager.View()) || bad.Calls() != 0 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 0 || newDefault.Calls() != 0 || gates.Load() != 1 || runs.Load() != 0 {
						t.Fatal("invalid replay or rejected resume changed state or executed")
					}
				})
			}
			request.Model.Model = rebuiltB
			replay, err := opened.SelectNextTurnModel(t.Context(), request)
			if err != nil || replay != receipt || !reflect.DeepEqual(before, opened.rt.manager.View()) {
				t.Fatalf("binding replay mutated acceptance: %+v %v", replay, err)
			}
			// Replaying again with another instance must keep the first rebound instance.
			replacement := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not replace"}), selectionTrustedConfig("B")}
			request.Model.Model = replacement
			if _, err = opened.SelectNextTurnModel(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			assertReboundSelection(t, opened, selected.ID, defaultSelection.ID, rebuiltB.FakeModel)
			if !reflect.DeepEqual(before, opened.rt.manager.View()) || rebuiltB.Calls() != 0 || replacement.Calls() != 0 || runs.Load() != 0 {
				t.Fatal("binding replay committed or automatically executed")
			}
			resumeForFreshApproval(t, opened, input.TraceID)
			if rebuiltB.Calls() != 0 || rebuiltA.Calls() != 0 || replacement.Calls() != 0 || gates.Load() != 1 || runs.Load() != 0 {
				t.Fatal("unanswered Resume replayed model or tools")
			}
			answerCommand(t, opened, "allowed-once")
			resumeStarted := time.Now()
			_, err = opened.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: opened.rt.manager.View().LastSeq})
			if err != nil {
				logSelectionRebindFailure(t, opened, input.TraceID, resumeStarted)
				t.Fatalf("resume rejected: errorCodes=%v checkpointApprovalExpired=%t", selectionRebindErrorCodes(err.Error()), strings.Contains(err.Error(), "checkpoint approval is expired"))
			}
			waitResumeCondition(t, func() bool { return terminal(opened.rt.manager.View().Traces[input.TraceID].State) })
			after := opened.rt.manager.View()
			if after.Traces[input.TraceID].State != "completed" || a.Calls() != 1 || b.Calls() != 1 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 1 || replacement.Calls() != 0 || gates.Load() != 1 || runs.Load() != 1 || !reflect.DeepEqual(cp, after.Checkpoints[cp.ID]) {
				logSelectionRebindFailure(t, opened, input.TraceID, resumeStarted)
				t.Fatalf("rebound B did not preserve original build and execute exactly once: state=%s A=%d B=%d rebuiltA=%d rebuiltB=%d replacement=%d gates=%d work=%d checkpointEqual=%t errorCodes=%v activityExpired=%t contextCanceled=%t", after.Traces[input.TraceID].State, a.Calls(), b.Calls(), rebuiltA.Calls(), rebuiltB.Calls(), replacement.Calls(), gates.Load(), runs.Load(), reflect.DeepEqual(cp, after.Checkpoints[cp.ID]), selectionRebindErrorCodes(after.Traces[input.TraceID].Error), strings.Contains(after.Traces[input.TraceID].Error, "budget_exhausted: activity reservation expired"), strings.Contains(after.Traces[input.TraceID].Error, context.Canceled.Error()))
			}
			if len(after.Calls) != 2 || len(after.Calls) != len(view.Calls) || len(cp.CallIDs) != 1 || newDefault.Calls() != 0 {
				t.Fatal("resume changed the accepted tool call set")
			}
			for id, original := range view.Calls {
				call := after.Calls[id]
				if call.Scope != original.Scope || call.Call != original.Call || call.Observation == nil {
					t.Fatal("original full Scope or FrozenCall changed")
				}
			}
			usage := after.Traces[input.TraceID].Usage
			if usage.LogicalModelCalls != 3 || usage.TransportRequests != 3 || usage.ToolExecutions != 2 {
				t.Fatalf("final usage=%+v, want logical=3 physical=3 tools=2", usage)
			}
			if err := opened.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Reopen again so terminal replay exercises failed optional binding,
			// rather than silently using the already valid process-local slot.
			terminalSession, err := OpenAgentSession(t.Context(), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = terminalSession.Close(context.Background()) })
			terminalBefore := terminalSession.rt.manager.View()
			bad := selectionConfiguredModel{testkit.NewFake(testkit.Step{Text: "must not run terminal"}), selectionTrustedConfig("B")}
			delete(bad.cfg.Capabilities.Items, llm.CapTextStream)
			request.Model.Model = bad
			replay, err = terminalSession.SelectNextTurnModel(t.Context(), request)
			if err != nil || replay != receipt || !reflect.DeepEqual(terminalBefore, terminalSession.rt.manager.View()) || bad.Calls() != 0 || rebuiltA.Calls() != 0 || rebuiltB.Calls() != 1 || gates.Load() != 1 || runs.Load() != 1 {
				t.Fatalf("terminal replay changed receipt, state or calls: %+v %v", replay, err)
			}
			assertReboundSelection(t, terminalSession, selected.ID, defaultSelection.ID, nil)
		})
	}
}

func logSelectionRebindFailure(t *testing.T, s *AgentSession, traceID string, resumeStarted time.Time) {
	t.Helper()
	now := time.Now()
	view := s.rt.manager.View()
	trace := view.Traces[traceID]
	t.Logf("resume diagnostics: state=%s errorCodes=%v activityExpired=%t contextCanceled=%t claimBeforeDecision=%t approvalOutsideInterval=%t logical=%d physical=%d tools=%d activitySettledNs=%d activityReservedNs=%d monotonicElapsedNs=%d wallElapsedNs=%d", trace.State, selectionRebindErrorCodes(trace.Error), strings.Contains(trace.Error, "budget_exhausted: activity reservation expired"), strings.Contains(trace.Error, context.Canceled.Error()), strings.Contains(trace.Error, "claim precedes the approval decision"), strings.Contains(trace.Error, "approval is expired or outside its validity interval"), trace.Usage.LogicalModelCalls, trace.Usage.TransportRequests, trace.Usage.ToolExecutions, trace.Activity.Settled, trace.Activity.Reserved, now.Sub(resumeStarted), now.Round(0).Sub(resumeStarted.Round(0)))
	for _, approval := range view.Approvals {
		if approval.Scope.TraceID != traceID {
			continue
		}
		issued := approval.ExpiresAt.Add(-config.ApprovalValidity)
		decision, decided := view.ApprovalDecisions[approval.ID]
		t.Logf("approval diagnostics: decided=%t startBeforeIssue=%t nowBeforeIssue=%t nowExpired=%t startSinceIssueNs=%d nowSinceIssueNs=%d nowSinceDecisionNs=%d", decided, resumeStarted.Before(issued), now.Before(issued), !now.Before(approval.ExpiresAt), resumeStarted.Sub(issued), now.Sub(issued), now.Sub(decision.DecidedAt))
	}
}

// Persisted execution errors may contain provider text. Emit only known codes,
// never the original error, model configuration, checkpoint payload or output.
func selectionRebindErrorCodes(message string) []string {
	var codes []string
	for _, code := range []string{product.CodeInvalidArgument, product.CodeUnauthenticated, product.CodePermissionDenied, product.CodeNotFound, product.CodeStateConflict, product.CodeIdempotencyConflict, product.CodeUnsupportedCapability, product.CodeBudgetExhausted, product.CodeStorageUnavailable, product.CodeIncompatibleVersion, product.CodeIncompatibleResume, product.CodeReconciliationRequired, product.CodeResyncRequired, product.CodeResourceUnavailable, product.CodeInternal} {
		if strings.Contains(message, code+":") {
			codes = append(codes, code)
		}
	}
	return codes
}

func assertReboundSelection(t *testing.T, s *AgentSession, selectionID, defaultID string, want *testkit.FakeModel) {
	t.Helper()
	if err := s.rt.do(t.Context(), func(rt *runtime) error {
		if rt.defaultModelID != defaultID {
			t.Errorf("replay changed default: got %q want %q", rt.defaultModelID, defaultID)
		}
		bound := rt.modelSlots[selectionID]
		if want == nil {
			if bound != nil || len(rt.modelSlots) != 0 {
				t.Error("invalid candidate created a process-local binding")
			}
		} else if model, ok := bound.(selectionConfiguredModel); !ok || model.FakeModel != want || len(rt.modelSlots) != 1 {
			t.Error("replay replaced or failed to restore the accepted instance")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
