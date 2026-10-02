package eino

import (
	"context"
	"fmt"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// Two independent fake scripts share the registered agent name, but are
// selected only by the full externally rebound invocation scope. Native
// approval names and terminal provider CallIDs intentionally collide.
func TestP3ChildResumeContractSameAgentAddressIsolation(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("different_ancestors_%t", nested), func(t *testing.T) {
			p := &p3ChildProbe{scope: p3ChildRootScope(), models: map[string]*p3ChildModel{}}
			var rootCalls []schema.FunctionToolCall
			var rootResults []schema.FunctionToolResult
			roots := map[string]p3ExpectedRoot{}
			for _, side := range []string{"left", "right"} {
				providerID := "root-" + side
				childScope := p3ChildScope(p.scope, providerID)
				address := adk.Address{{Type: adk.AddressSegmentAgent, ID: "root"}, {Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: providerID}}
				rootTarget, rootTask, rootResult := "leaf", "task:"+side+"-leaf", side+"-leaf-result"
				if nested {
					rootTarget, rootTask, rootResult = side, "task:"+side+"-parent", side+"-parent-result"
					p.models[side+"-parent"] = p3NewChildModel(side, rootTask, childScope, []schema.FunctionToolCall{p3DelegateCall("same-child-call", "leaf", "task:"+side+"-leaf")}, []schema.FunctionToolResult{p3ToolResult("same-child-call", "delegate_task", side+"-leaf-result")}, rootResult)
					address = append(address, adk.AddressSegment{Type: adk.AddressSegmentAgent, ID: side}, adk.AddressSegment{Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: "same-child-call"})
					childScope = p3ChildScope(childScope, "same-child-call")
				}
				rootCalls = append(rootCalls, p3DelegateCall(providerID, rootTarget, rootTask))
				rootResults = append(rootResults, p3ToolResult(providerID, "delegate_task", rootResult))
				m := p3NewChildModel("leaf", "task:"+side+"-leaf", childScope, []schema.FunctionToolCall{{CallID: "shared-ask", Name: "approve", Arguments: `{}`}}, []schema.FunctionToolResult{p3ToolResult("shared-ask", "approve", "leaf-approved")}, side+"-leaf-result")
				m.effect = &p.a
				if side == "right" {
					m.effect = &p.b
				}
				p.models[side] = m
				address = append(address, adk.AddressSegment{Type: adk.AddressSegmentAgent, ID: "leaf"}, adk.AddressSegment{Type: adk.AddressSegmentTool, ID: "approve", SubID: "shared-ask"})
				roots[side] = p3ExpectedRoot{info: "approve:leaf", address: address}
			}
			rootCalls = append(rootCalls, schema.FunctionToolCall{CallID: "root-done", Name: "done", Arguments: `{}`})
			rootResults = append(rootResults, p3ToolResult("root-done", "done", "done-result"))
			p.models["root"] = p3NewChildModel("root", "task:root", p.scope, rootCalls, rootResults, "root-result")
			if p.models["left"].scope.InvocationID == p.models["right"].scope.InvocationID {
				t.Fatal("same-agent delegates share a test invocation identity")
			}
			ctx := WithExecutionScope(t.Context(), p.scope)
			store := &p3WorkflowStore{}
			makeRunner := func(ctx context.Context, store *p3WorkflowStore) *adk.TypedRunner[*schema.AgenticMessage] {
				ag, err := p.newAgent(ctx, "root")
				if err != nil {
					t.Fatal(err)
				}
				return adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag, CheckPointStore: store})
			}
			contexts := p3DrainChildProbe(t, makeRunner(ctx, store).Run(ctx, []*schema.AgenticMessage{schema.UserAgenticMessage("task:root")}, adk.WithCheckPointID("tree")))
			ids := p3ProbeRoots(t, contexts, roots)
			oldRightID := ids["right"]
			for _, m := range p.models {
				if m.Calls() != 1 {
					t.Fatal("same-agent initial model count differs")
				}
			}
			if p.a.Load() != 0 || p.b.Load() != 0 || p.done.Load() != 1 {
				t.Fatal("unanswered same-agent branch executed an effect")
			}
			blob, ok, err := store.Get(ctx, "tree")
			if err != nil || !ok || len(blob) == 0 {
				t.Fatal("same-agent aggregate checkpoint missing")
			}
			reopened := &p3WorkflowStore{}
			if err := reopened.Set(ctx, "tree", blob); err != nil {
				t.Fatal(err)
			}
			blob[0] ^= 0xff
			ctx = WithExecutionScope(t.Context(), p.scope) // scope is caller state, not native persistence
			it, err := makeRunner(ctx, reopened).ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{ids["left"]: "allowed-once"}})
			if err != nil {
				t.Fatal(err)
			}
			contexts = p3DrainChildProbe(t, it)
			remaining := p3ProbeRoots(t, contexts, map[string]p3ExpectedRoot{"right": roots["right"]})
			if remaining["right"] == oldRightID {
				t.Fatal("reinterrupt reused the old opaque target ID; re-evaluate the pinned recovery mapping")
			}
			t.Logf("unanswered target: old ID=%s new ID=%s complete Address=%s", oldRightID, remaining["right"], roots["right"].address.String())
			if p.a.Load() != 1 || p.b.Load() != 0 || p.done.Load() != 1 || p.models["root"].Calls() != 1 || p.models["left"].Calls() != 2 || p.models["right"].Calls() != 1 {
				t.Fatal("selective answer crossed same-agent/provider-ID ancestor boundaries")
			}
			if nested && (p.models["left-parent"].Calls() != 2 || p.models["right-parent"].Calls() != 1) {
				t.Fatal("selective answer crossed a different ancestor invocation")
			}
			it, err = makeRunner(ctx, reopened).ResumeWithParams(ctx, "tree", &adk.ResumeParams{Targets: map[string]any{remaining["right"]: "allowed-once"}})
			if err != nil {
				t.Fatal(err)
			}
			p3ProbeRoots(t, p3DrainChildProbe(t, it), nil)
			if p.a.Load() != 1 || p.b.Load() != 1 || p.done.Load() != 1 || p.doneEntries.Load() != 1 {
				t.Fatal("same-agent completed sibling effect was repeated")
			}
			for key, m := range p.models {
				wantDelegations := int32(2)
				if key == "root" {
					wantDelegations = 0
				} else if key == "right" || key == "right-parent" {
					wantDelegations = 3
				}
				wantApprovals := int32(0)
				if key == "left" {
					wantApprovals = 2
				} else if key == "right" {
					wantApprovals = 3
				}
				if m.Calls() != 2 || m.generates.Load() != 2 || m.streams.Load() != 0 || m.metadataSeen.Load() != 1 || m.delegations.Load() != wantDelegations || m.approvalEntries.Load() != wantApprovals {
					t.Fatalf("%s invocation/model/tool counts differ: %v", key, p.counts())
				}
			}
			if ScopeFromContext(ctx, p.scope) != p.scope {
				t.Fatal("same-agent children mutated their parent's full scope")
			}
			t.Logf("same-agent final counts: %v", p.counts())
		})
	}
}
