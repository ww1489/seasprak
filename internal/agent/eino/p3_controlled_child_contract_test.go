package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This controlled-path characterization deliberately uses an ordinary trusted
// callback, not a certified delegation owner. Native interrupt errors alone
// must never authorize continuing a claimed structural call.
type p3ChildToolFactSink struct {
	mu                   sync.Mutex
	record               agent.ToolRecord
	claims, observations int
	claimErr             error
}

type p3ChildToolFacts struct {
	record               agent.ToolRecord
	claims, observations int
}

func (s *p3ChildToolFactSink) snapshot() p3ChildToolFacts {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.record
	if record.Observation != nil {
		observation := *record.Observation
		record.Observation = &observation
	}
	if record.Projection != nil {
		projection := *record.Projection
		record.Projection = &projection
	}
	return p3ChildToolFacts{record: record, claims: s.claims, observations: s.observations}
}

func (s *p3ChildToolFactSink) LookupTool(context.Context, agent.ExecutionScope, string) (agent.ToolRecord, error) {
	return s.snapshot().record, nil
}

func (s *p3ChildToolFactSink) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch fact.Kind {
	case "tool_intent":
		if s.claimErr != nil {
			return s.claimErr
		}
		s.claims++
		s.record.Claimed = true
	case "tool_observation":
		var record agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &record); err != nil {
			return err
		}
		s.observations++
		s.record = record
	}
	return nil
}

func p3CheckNativeChildSignal(err error, providerCallID string) error {
	signal, ok := err.(*adk.InterruptSignal)
	if !ok || signal.ID == "" {
		return fmt.Errorf("callback did not return actual native *adk.InterruptSignal: %T", err)
	}
	if _, interrupted := compose.IsInterruptRerunError(err); !interrupted {
		return errors.New("public native-signal detector missed callback error")
	}
	if state, ok := signal.State.([]byte); !ok || len(state) == 0 {
		return errors.New("native AgentTool signal omitted opaque child checkpoint bytes")
	}
	var roots []*adk.InterruptSignal
	var visit func(*adk.InterruptSignal)
	visit = func(s *adk.InterruptSignal) {
		if s.IsRootCause {
			roots = append(roots, s)
		}
		for _, child := range s.Subs {
			visit(child)
		}
	}
	visit(signal)
	// The native graph bridge reconstructs public contexts and leaves the
	// exported leaf signal state nil; private approval state is in the opaque
	// AgentTool bytes. This is characterization, not a semantic blob parser.
	if len(roots) != 1 || roots[0].ID == "" || roots[0].Info != "approve:b" || roots[0].State != nil {
		return errors.New("native signal lost expected b root cause or changed exposed-state semantics")
	}
	var address adk.Address
	for _, segment := range roots[0].Address {
		if segment.Type == adk.AddressSegmentAgent || segment.Type == adk.AddressSegmentTool {
			address = append(address, segment)
		}
	}
	wantAddress := adk.Address{{Type: adk.AddressSegmentTool, ID: "delegate_task", SubID: providerCallID}, {Type: adk.AddressSegmentAgent, ID: "b"}, {Type: adk.AddressSegmentTool, ID: "approve", SubID: "b-ask"}}
	if !reflect.DeepEqual(address, wantAddress) {
		return errors.New("native callback b root cause changed its agent/tool address")
	}
	return nil
}

func TestP3ChildResumeContractOrdinaryCallbacksFailClosed(t *testing.T) {
	for _, failure := range []string{"native_interrupt", "ordinary_error", "claim_failure"} {
		t.Run(failure, func(t *testing.T) {
			p := newP3ChildProbe()
			scope := p.scope
			p.models["b"].scope = p3ChildScope(scope, "provider-child")
			ctx := WithExecutionScope(t.Context(), scope)
			const args = `{"agent":"b","task":"task:b"}`
			frozen := agent.FrozenCall{CallID: "product-child", ProviderCallID: "provider-child", Name: "delegate_task", Arguments: args, Generation: scope.Generation, SelectionRevision: scope.SelectionRevision, Hash: "synthetic-frozen-hash"}
			sink := &p3ChildToolFactSink{record: agent.ToolRecord{Scope: scope, Call: frozen}}
			sentinel := errors.New("synthetic controlled-path failure")
			if failure == "claim_failure" {
				sink.claimErr = sentinel
			}
			initialFacts, initialCounts := sink.snapshot(), p.counts()
			var callbacks atomic.Int32
			var callbackMu sync.Mutex
			var callbackErr, signalCertificationErr error
			def := tools.Definition{Name: "delegate_task", Version: "delegate-task-v1", Schema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"task":{"type":"string"}},"required":["agent","task"],"additionalProperties":false}`), Execution: tools.ExecutionDescription{BackendID: "trusted-run", Effect: "none", Concurrency: "shared"}, Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				callbacks.Add(1)
				out, err := "", sentinel
				if failure != "ordinary_error" {
					out, err = (&p3DynamicChildTool{probe: p}).InvokableRun(ctx, string(raw))
				}
				var certificationErr error
				if failure == "native_interrupt" {
					certificationErr = p3CheckNativeChildSignal(err, "provider-child")
				}
				callbackMu.Lock()
				callbackErr, signalCertificationErr = err, certificationErr
				callbackMu.Unlock()
				return out, err // preserve the real signal for Executor's fail-closed path
			}}
			budget := agent.NewBudget(config.DefaultLimits())
			executor, err := tools.NewExecutor(scope.Generation, []tools.Definition{def}, sink, allowAll{}, budget, tools.WithResourceScheduler(tools.NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			node, err := compose.NewAgenticToolsNode(ctx, &compose.ToolsNodeConfig{Tools: []tool.BaseTool{NewPipelineTool(testkit.ToolInfo("delegate_task", "explicit child"), executor, scope)}})
			if err != nil {
				t.Fatal(err)
			}
			input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider-child", Name: "delegate_task", Arguments: args})}}
			output, err := node.Invoke(ctx, input)
			if _, interrupted := compose.IsInterruptRerunError(err); interrupted {
				t.Fatal("uncertified callback leaked a native resumable signal")
			}
			if _, interrupted := compose.ExtractInterruptInfo(err); interrupted {
				t.Fatal("uncertified callback leaked a graph resumable signal")
			}
			facts := sink.snapshot()
			callbackMu.Lock()
			capturedErr, certificationErr := callbackErr, signalCertificationErr
			callbackMu.Unlock()
			if failure == "claim_failure" {
				if !errors.Is(err, sentinel) || len(output) != 0 || callbacks.Load() != 0 || capturedErr != nil || !reflect.DeepEqual(facts, initialFacts) || facts.record.Claimed || facts.record.Observation != nil || budget.Snapshot().ToolExecutions != 0 || !reflect.DeepEqual(p.counts(), initialCounts) {
					t.Fatal("failed claim changed identity, claimed state, or any model/tool/effect count")
				}
				t.Logf("claim failure: unclaimed, callback=0, budget=0, all counts=%v", p.counts())
				return
			}
			wantObservation := &agent.ToolObservation{Status: "failed", Content: "trusted tool execution failed", SideEffect: "unknown", Executed: true}
			if err != nil || len(output) != 1 || callbacks.Load() != 1 || facts.claims != 1 || facts.observations != 1 || !facts.record.Claimed || facts.record.Call != frozen || facts.record.Scope != scope || !reflect.DeepEqual(facts.record.Observation, wantObservation) || budget.Snapshot().ToolExecutions != 1 {
				t.Fatal("uncertified callback lost its claimed/executed unknown observation or frozen identity")
			}
			if failure == "native_interrupt" {
				if certificationErr != nil || capturedErr == nil {
					t.Fatalf("callback failed native signal certification: %v", certificationErr)
				}
				if p.models["b"].Calls() != 1 || p.models["b"].delegations.Load() != 1 || p.models["b"].approvalEntries.Load() != 1 {
					t.Fatal("native callback did not enter its child model and unanswered approval exactly once")
				}
			} else if !errors.Is(capturedErr, sentinel) || !reflect.DeepEqual(p.counts(), initialCounts) {
				t.Fatal("ordinary-error callback unexpectedly invoked child work")
			}
			if p.a.Load() != 0 || p.b.Load() != 0 || p.done.Load() != 0 || p.models["root"].Calls() != 0 || p.models["a"].Calls() != 0 || p.models["leaf"].Calls() != 0 {
				t.Fatal("failed callback admitted unrelated work or an unanswered effect")
			}
			message := output[0]
			if message == nil || message.Role != schema.AgenticRoleTypeUser || len(message.ContentBlocks) != 1 || message.ContentBlocks[0].FunctionToolResult == nil {
				t.Fatal("failed/unknown result projection is missing")
			}
			projection := message.ContentBlocks[0].FunctionToolResult
			if projection.CallID != frozen.ProviderCallID || projection.Name != frozen.Name || len(projection.Content) != 1 || projection.Content[0].Text == nil {
				t.Fatal("failed/unknown output projection changed frozen provider identity")
			}
			var outcome tools.Outcome
			if json.Unmarshal([]byte(projection.Content[0].Text.Text), &outcome) != nil || !reflect.DeepEqual(outcome, tools.Outcome{Status: "failed", Content: "trusted tool execution failed", SideEffect: "unknown", Executed: true}) {
				t.Fatal("callback output is not the exact failed/executed/unknown projection")
			}
			beforeBudget, beforeCounts := budget.Snapshot(), p.counts()
			retry, err := node.Invoke(ctx, input)
			if err != nil || !reflect.DeepEqual(retry, output) || callbacks.Load() != 1 || !reflect.DeepEqual(sink.snapshot(), facts) || !reflect.DeepEqual(budget.Snapshot(), beforeBudget) || !reflect.DeepEqual(p.counts(), beforeCounts) {
				t.Fatal("ordinary retry changed projection, claims, budget, model, tool, or effect counts")
			}
			t.Logf("ordinary callback: claimed/executed failed/unknown, claims=1 observations=1 callbacks=1 budget=1; retry unchanged; counts=%v", beforeCounts)
		})
	}
}
