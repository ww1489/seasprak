package codeagent

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// A real resumed worker stays behind the hook barrier while an earlier
// authorization is obtained and then invalidated before tool_intent admission.
func TestApprovalClaimRechecksExpiryAndPolicyAfterAuthorization(t *testing.T) {
	for _, gate := range []string{"expiry", "policy"} {
		t.Run(gate, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var hooks atomic.Int32
			f := waitingApprovalSession(t, func(context.Context, agent.FrozenExecution) error {
				if hooks.Add(1) == 2 {
					close(entered)
					<-release
				}
				return nil
			})
			response := answerApproval(t, f, "allowed-once")
			expiry := approvalSnapshot(t, f.s).Interactions[response.InteractionID].ExpiresAt
			if _, err := f.s.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: f.manager.View().LastSeq}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("resumed tool did not reach hook barrier")
			}
			var scope agent.ExecutionScope
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
				scope = rt.active.scope
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			call := onlyControlledCall(t, f.s)
			frozen := f.manager.View().FrozenExecutions["execution:"+call.Call.CallID]
			authorization := call.Call
			authorization.Hash, authorization.Arguments = frozen.Hash, string(frozen.FinalArguments)
			decision, err := (sessionAuthorizer{rt: f.s.rt, scope: scope}).Authorize(t.Context(), authorization)
			if err != nil || decision != agent.DecisionAllow {
				t.Fatalf("initial authorization=%s err=%v", decision, err)
			}
			if gate == "expiry" {
				// Replace the approval wall clock without expiring the independent
				// activity lease, so only the 24-hour approval boundary is tested.
				if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
					rt.clock = &manualActivityClock{now: expiry}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := f.s.rt.setExecutionPolicy(t.Context(), f.manager.View().ExecutionPolicy.Revision, agent.ResolvedPolicy{ApprovalPolicy: "never"}); err != nil {
				t.Fatal(err)
			}
			before := f.manager.View()
			usage := before.Traces[f.input.TraceID].Usage
			usage.ToolExecutions++
			raw, err := json.Marshal(call.Call)
			if err != nil {
				t.Fatal(err)
			}
			err = f.s.rt.CommitFact(t.Context(), scope, agent.Fact{Kind: "tool_intent", Payload: raw, Budget: &usage})
			requireSessionCode(t, err, product.CodePermissionDenied)
			if !reflect.DeepEqual(before, f.manager.View()) || f.runs.Load() != 0 || f.model.Calls() != 1 || before.Calls[call.Call.CallID].Claimed || before.Traces[f.input.TraceID].Usage.ToolExecutions != 0 {
				t.Fatal("stale authorization committed claim, intent, budget or execution")
			}
			if err := f.s.rt.do(t.Context(), func(rt *runtime) error {
				if rt.approvals[response.InteractionID].claimedExecution != "" {
					t.Error("rejected claim consumed runtime approval")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			close(release)
			waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
			final := f.manager.View()
			if f.runs.Load() != 0 || final.Calls[call.Call.CallID].Claimed || final.Traces[f.input.TraceID].Usage.ToolExecutions != 0 {
				t.Fatal("released worker executed with revoked or expired approval")
			}
		})
	}
}
