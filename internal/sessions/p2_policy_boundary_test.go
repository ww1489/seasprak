package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// Test-only barrier after real authorization, before the real atomic claim.
type policyBarrierAuthorizer struct {
	base      sessionAuthorizer
	after     func()
	decisions int
}

func (a *policyBarrierAuthorizer) Authorize(ctx context.Context, c agent.FrozenCall) (agent.Decision, error) {
	decision, err := a.base.Authorize(ctx, c)
	if err == nil && decision == agent.DecisionAllow {
		a.decisions++
		a.after()
	}
	return decision, err
}
func policyPendingRuntime(t *testing.T) (*runtime, agent.ExecutionScope, tools.Definition, *atomic.Int32) {
	t.Helper()
	backend, err := memory.Open("policy-race", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := state.NewManager(backend, "policy-race")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetExecutionPolicy(t.Context(), 0, agent.ResolvedPolicy{}); err != nil {
		t.Fatal(err)
	}
	receipt, err := m.Accept(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"work"}`)}, agent.TargetAgent{Name: "main", Generation: "gen"})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetTraceState(t.Context(), receipt.TraceID, "running", false); err != nil {
		t.Fatal(err)
	}
	scope := agent.ExecutionScope{SessionID: "policy-race", TraceID: receipt.TraceID, InvocationID: m.View().Traces[receipt.TraceID].InvocationID, Generation: "gen", ExecutionID: "execution", TurnID: "turn"}
	if err := m.SaveTurn(t.Context(), agent.TurnRecord{ID: "turn", TraceID: scope.TraceID, InvocationID: scope.InvocationID}); err != nil {
		t.Fatal(err)
	}
	call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "call", ProviderCallID: "provider", Name: "work", Arguments: `{}`, Generation: "gen"}}
	msg := assistantMessage(scope, &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}})
	if err := m.SaveAssistant(t.Context(), msg, []agent.ToolRecord{call}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runs := &atomic.Int32{}
	def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "write", PolicyRef: "forged-from-definition"}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ran", nil }}
	rt := &runtime{manager: m, opts: Options{SessionID: "policy-race", Store: backend, Tools: []tools.Definition{def}}, mailbox: make(chan command, 64), done: make(chan struct{}), subs: map[int]*subscription{}, active: &execution{scope: scope, turnID: scope.TurnID, ctx: ctx, cancel: cancel, activity: &activityLease{clock: systemActivityClock{}, committed: true, sample: time.Now(), record: state.ActivityBudget{Reserved: time.Minute}}}}
	go rt.loop()
	t.Cleanup(func() {
		cancel()
		_ = rt.do(context.Background(), func(rt *runtime) error { rt.active = nil; rt.closing = true; return nil })
		<-rt.done
	})
	return rt, scope, def, runs
}

func TestP2PolicyClaimRechecksAfterAuthorization(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "tightened", true: "cancelled"}[cancelled], func(t *testing.T) {
			rt, scope, def, runs := policyPendingRuntime(t)
			budget := agent.NewBudget(config.DefaultLimits())
			authorizer := &policyBarrierAuthorizer{base: sessionAuthorizer{rt: rt, scope: scope}, after: func() {
				if cancelled {
					rt.active.cancel()
					return
				}
				if err := rt.setExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{SandboxMode: "read-only"}); err != nil {
					t.Fatal(err)
				}
			}}
			exec, err := tools.NewExecutor("gen", []tools.Definition{def}, rt, authorizer, budget)
			if err != nil {
				t.Fatal(err)
			}
			out, err := exec.Run(t.Context(), scope, "provider", "work", `{}`)
			want := "denied"
			if cancelled {
				want = "cancelled"
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err=%v", err)
				}
			} else {
				policyCode(t, err, product.CodePermissionDenied)
			}
			v := rt.manager.View()
			call := v.Calls["call"]
			if authorizer.decisions != 1 || runs.Load() != 0 || budget.Snapshot().ToolExecutions != 0 || v.Traces[scope.TraceID].Usage.ToolExecutions != 0 || call.Claimed || out.Status != want || call.Observation == nil || call.Observation.Status != want || call.Observation.Executed {
				t.Fatalf("decisions=%d runs=%d out=%+v call=%+v", authorizer.decisions, runs.Load(), out, call)
			}
			if frozen := v.FrozenExecutions["execution:call"]; frozen.PolicyRef == "" || frozen.PolicyRef == "forged-from-definition" {
				t.Fatal("tool-supplied policy ref was trusted")
			}
			if err := rt.FinishTurn(t.Context(), scope, agent.TurnFact{TurnID: "turn", HasTools: true}); err != nil {
				t.Fatal(err)
			}
			results := 0
			for _, m := range rt.manager.View().Messages {
				if m.Kind == agent.KindToolResult {
					results++
				}
			}
			if results != 1 {
				t.Fatalf("paired results=%d", results)
			}
		})
	}
}

func TestP2PolicyAuthorizationRequiresCommittedMatchingDescriptor(t *testing.T) {
	rt, scope, def, runs := policyPendingRuntime(t)
	auth := sessionAuthorizer{rt: rt, scope: scope}
	authorization := rt.manager.View().Calls["call"].Call
	_, err := auth.Authorize(t.Context(), authorization)
	policyCode(t, err, product.CodePermissionDenied)
	f := agent.FrozenExecution{ID: "execution:call", CallID: "call", Scope: scope, Origin: "model", Tool: "work", ToolVersion: "1", SchemaHash: toolArgumentHash(def.Schema), Generation: "gen", ProviderCallID: "provider", OriginalArgumentsHash: toolArgumentHash([]byte(`{}`)), FinalArgumentsHash: toolArgumentHash([]byte(`{}`)), FinalArguments: json.RawMessage(`{}`), BackendID: "trusted-run", Effect: "write", PolicyRef: "forged"}
	f.Hash, _ = f.Digest()
	raw, _ := json.Marshal(f)
	before := rt.manager.View().LastSeq
	err = rt.CommitFact(t.Context(), scope, agent.Fact{Kind: "tool_frozen", Payload: raw})
	policyCode(t, err, product.CodePermissionDenied)
	if rt.manager.View().LastSeq != before {
		t.Fatal("forged frozen policy committed")
	}
	f.PolicyRef = rt.manager.View().ExecutionPolicy.Ref
	f.Hash, _ = f.Digest()
	raw, _ = json.Marshal(f)
	if err := rt.CommitFact(t.Context(), scope, agent.Fact{Kind: "tool_frozen", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	authorization.Hash = f.Hash
	if d, err := auth.Authorize(t.Context(), authorization); err != nil || d != agent.DecisionAllow {
		t.Fatalf("decision=%s err=%v", d, err)
	}
	for _, field := range []string{"hash", "arguments", "provider", "generation", "scope"} {
		bad := authorization
		current := auth
		switch field {
		case "hash":
			bad.Hash = "forged"
		case "arguments":
			bad.Arguments = `{"changed":true}`
		case "provider":
			bad.ProviderCallID = "other"
		case "generation":
			bad.Generation = "other"
		case "scope":
			current.scope.TurnID = "other"
		}
		_, err := current.Authorize(t.Context(), bad)
		policyCode(t, err, product.CodePermissionDenied)
	}
	if runs.Load() != 0 || rt.manager.View().Calls["call"].Claimed {
		t.Fatal("authorization alone executed")
	}
}

// policyWaitDeadline lets the test expire the caller's wait only after the
// mailbox has committed cancellation. A real 20ms timer could expire before
// acceptance under race instrumentation, which does not cancel execution.
type policyWaitDeadline struct{ context.Context }

func (c policyWaitDeadline) Err() error {
	if c.Context.Err() != nil {
		return context.Cause(c.Context)
	}
	return nil
}

func TestP2PolicySessionChangedAfterFreezeAndCancelledHook(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "cancelled-hook"}[cancelled], func(t *testing.T) {
			backend, err := memory.Open("policy-hook", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			m, err := state.NewManager(backend, "policy-hook")
			if err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			gate := make(chan struct{})
			var runs atomic.Int32
			def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{PolicyRef: "forged"}, BeforeCall: []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, f agent.FrozenExecution) error {
				if f.PolicyRef == "forged" || f.PolicyRef != m.View().ExecutionPolicy.Ref {
					t.Error("untrusted policy reference")
				}
				close(entered)
				<-release
				return nil
			}}, Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "ran", nil }}
			opts := Options{SessionID: "policy-hook", Profile: ProfileMemory, Store: backend, Model: testkit.NewFake(testkit.Step{Gate: gate, ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "done"}), Tools: []tools.Definition{def}}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, m, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			receipt := activitySubmit(t, s)
			frame := activityFrame(t, s)
			close(gate)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("hook not entered")
			}
			if cancelled {
				base, expire := context.WithCancelCause(context.Background())
				ctx := policyWaitDeadline{base}
				defer expire(context.Canceled)
				result := make(chan error, 1)
				go func() { result <- s.Cancel(ctx, receipt.TraceID) }()
				select {
				case <-frame.ctx.Done(): // accepted intent, not a stopped proof
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation was not accepted")
				}
				expire(context.DeadlineExceeded)
				err := <-result
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("cancel=%v", err)
				}
				select {
				case <-frame.done:
					t.Fatal("uncooperative hook declared exited")
				default:
				}
			} else if err := s.rt.setExecutionPolicy(t.Context(), 1, agent.ResolvedPolicy{SandboxMode: "read-only"}); err != nil {
				t.Fatal(err)
			}
			close(release)
			activityWait(t, frame)
			v := m.View()
			wantStatus, wantState := "denied", "failed"
			if cancelled {
				wantStatus, wantState = "cancelled", "cancelled"
			}
			if runs.Load() != 0 || v.Traces[receipt.TraceID].Usage.ToolExecutions != 0 || v.Traces[receipt.TraceID].State != wantState {
				t.Fatalf("runs=%d trace=%+v", runs.Load(), v.Traces[receipt.TraceID])
			}
			for _, c := range v.Calls {
				if c.Claimed || c.Observation == nil || c.Observation.Status != wantStatus || c.Observation.Executed {
					t.Fatalf("call=%+v", c)
				}
			}
			results := 0
			for _, msg := range v.Messages {
				if msg.Kind == agent.KindToolResult {
					results++
				}
			}
			if results != 1 {
				t.Fatal("unpaired denied result")
			}
		})
	}
}
