package eino

import (
	"context"
	"encoding/json"
	"github.com/ww1489/seasprak/internal/config"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

type turnBoundary struct {
	id       string
	finished []agent.TurnFact
	scopes   []agent.ExecutionScope
}

func (b *turnBoundary) PrepareNextTurn(context.Context, agent.ExecutionScope) (agent.TurnPlan, error) {
	return agent.TurnPlan{TurnID: b.id}, nil
}

func (b *turnBoundary) ShouldStop(context.Context, agent.ExecutionScope) (bool, string, error) {
	return false, "continue", nil
}

func (b *turnBoundary) FinishTurn(_ context.Context, scope agent.ExecutionScope, fact agent.TurnFact) error {
	b.finished = append(b.finished, fact)
	b.scopes = append(b.scopes, scope)
	return nil
}

func (b *turnBoundary) TakeSteering(context.Context, agent.ExecutionScope) (*agent.AgentMessage, error) {
	return nil, nil
}

type captureModel struct {
	inner *testkit.FakeModel
	ctxs  []context.Context
}

func (m *captureModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	m.ctxs = append(m.ctxs, ctx)
	return m.inner.Generate(ctx, in, opts...)
}

func (m *captureModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.ctxs = append(m.ctxs, ctx)
	return m.inner.Stream(ctx, in, opts...)
}

func drain(t *testing.T, iter *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) {
	t.Helper()
	for {
		ev, ok := iter.Next()
		if !ok {
			return
		}
		if ev.Err != nil {
			t.Fatal(ev.Err)
		}
	}
}

func TestNoToolFinishTurnCarriesID(t *testing.T) {
	fake := &captureModel{inner: testkit.NewFake(testkit.Step{Text: "done"})}
	boundary := &turnBoundary{id: "turn-1"}
	fallback := agent.ExecutionScope{TraceID: "tr", InvocationID: "inv"}
	ag, err := NewAgent(context.Background(), Deps{
		Model: fake, Sink: nopFact{}, Budget: agent.NewBudget(config.DefaultLimits()),
		Boundary: boundary, Instruction: "test", Scope: fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionScope(context.Background(), fallback)
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	drain(t, runner.Query(ctx, "ping"))
	if len(boundary.finished) != 1 {
		t.Fatalf("finish calls = %d", len(boundary.finished))
	}
	if boundary.finished[0].TurnID != "turn-1" || boundary.scopes[0].TurnID != "turn-1" {
		t.Fatalf("fact=%+v scope=%+v", boundary.finished[0], boundary.scopes[0])
	}
	if len(fake.ctxs) == 0 || ScopeFromContext(fake.ctxs[0], fallback).TurnID != "turn-1" {
		t.Fatal("model context did not carry the turn")
	}
}

func TestFinishAfterToolsUsesContextOrFallback(t *testing.T) {
	boundary := &turnBoundary{id: "turn-9"}
	fallback := agent.ExecutionScope{TraceID: "tr"}
	hook := FinishAfterTools(boundary, fallback)
	ctx := WithExecutionScope(context.Background(), agent.ExecutionScope{TraceID: "tr", TurnID: "turn-9"})
	if err := hook(ctx); err != nil {
		t.Fatal(err)
	}
	if boundary.finished[0].TurnID != "turn-9" || !boundary.finished[0].HasTools {
		t.Fatalf("fact %+v", boundary.finished[0])
	}
	boundary.finished = nil
	if err := hook(context.Background()); err != nil {
		t.Fatal(err)
	}
	if boundary.finished[0].HasTools != true || boundary.scopes[1].TraceID != "tr" {
		t.Fatalf("fallback fact=%+v scope=%+v", boundary.finished[0], boundary.scopes[1])
	}
}

func TestRetriesOnlyRetryableErrorsWithinBudget(t *testing.T) {
	retryable := &product.Error{Code: product.CodeResourceUnavailable, Message: "busy", Retryable: true}
	fake := testkit.NewFake(testkit.Step{Err: retryable}, testkit.Step{Err: retryable}, testkit.Step{Text: "ok"})
	budg := agent.NewBudget(config.DefaultLimits())
	ag, err := NewAgent(context.Background(), Deps{
		Model: fake, Sink: nopFact{}, Budget: budg, Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	drain(t, runner.Query(context.Background(), "ping"))
	if fake.Calls() != 3 {
		t.Fatalf("calls = %d, want 3", fake.Calls())
	}
	if budg.Snapshot().LogicalModelCalls != 1 || budg.Snapshot().TransportRequests != 3 {
		t.Fatalf("usage %+v", budg.Snapshot())
	}

	permanent := testkit.NewFake(testkit.Step{Err: product.NewError(product.CodeInvalidArgument, "no")}, testkit.Step{Text: "later"})
	ag, err = NewAgent(context.Background(), Deps{
		Model: permanent, Sink: nopFact{}, Budget: agent.NewBudget(config.DefaultLimits()), Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner = adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	iter := runner.Query(context.Background(), "ping")
	var saw error
	for {
		ev, ok := iter.Next()
		if !ok {
			break
		}
		if ev.Err != nil {
			saw = ev.Err
		}
	}
	if saw == nil || permanent.Calls() != 1 {
		t.Fatalf("err=%v calls=%d", saw, permanent.Calls())
	}
}

func TestRetryAllowsLastLogicalTurnUntilTransportBudget(t *testing.T) {
	limits := config.DefaultLimits()
	limits.TraceLogicalModelCalls = 1
	limits.LogicalModelRequests = 3
	limits.TraceTransportRequests = 5
	budg := agent.NewBudget(limits)
	if err := budg.BeginTurn(); err != nil {
		t.Fatal(err)
	}
	if err := budg.OccupyModel(); err != nil {
		t.Fatal(err)
	}
	busy := &product.Error{Code: product.CodeResourceUnavailable, Message: "busy", Retryable: true}
	rc := &adk.TypedRetryContext[*schema.AgenticMessage]{Err: busy}
	if !retryDecision(context.Background(), rc, budg).Retry {
		t.Fatal("retry refused inside the last allowed logical turn")
	}
	if err := budg.OccupyModel(); err != nil || budg.OccupyModel() != nil {
		t.Fatal("per-turn occupy")
	}
	if retryDecision(context.Background(), rc, budg).Retry {
		t.Fatal("retry allowed after the per-turn request budget was used")
	}
	limits.TraceTransportRequests = 1
	limits.LogicalModelRequests = 3
	limits.TraceLogicalModelCalls = 4
	blocked := agent.NewBudget(limits)
	if err := blocked.BeginTurn(); err != nil || blocked.OccupyModel() != nil {
		t.Fatal("transport setup")
	}
	if retryDecision(context.Background(), rc, blocked).Retry {
		t.Fatal("retry allowed after trace transport budget was used")
	}
}

func TestMaxIterationFollowsTraceLogicalLimit(t *testing.T) {
	limits := config.DefaultLimits()
	limits.TraceLogicalModelCalls = 4
	fake := testkit.NewFake(testkit.Step{
		Repeat: true, Finish: "tool_calls",
		ToolCalls: []schema.FunctionToolCall{{CallID: "p1", Name: "echo", Arguments: `{}`}},
	})
	ag, err := NewAgent(context.Background(), Deps{
		Model: fake, Tools: []tool.BaseTool{echoTool{}}, Sink: nopFact{},
		Budget: agent.NewBudget(limits), Instruction: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	iter := runner.Query(context.Background(), "ping")
	for {
		_, ok := iter.Next()
		if !ok {
			break
		}
	}
	if fake.Calls() != 4 {
		t.Fatalf("calls = %d, want max iteration 4", fake.Calls())
	}
}

type echoTool struct{}

func (echoTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "echo", Desc: "echo"}, nil
}

func (echoTool) InvokableRun(context.Context, string, ...tool.Option) (string, error) {
	return "ok", nil
}

type nopFact struct{}

func (nopFact) CommitFact(context.Context, agent.ExecutionScope, agent.Fact) error { return nil }

func TestScopeFromContextFallsBack(t *testing.T) {
	fallback := agent.ExecutionScope{TraceID: "fallback"}
	if got := ScopeFromContext(context.Background(), fallback); got != fallback {
		t.Fatalf("got %+v", got)
	}
	ctx := WithExecutionScope(context.Background(), agent.ExecutionScope{TraceID: "live", TurnID: "t"})
	if got := ScopeFromContext(ctx, fallback); got.TurnID != "t" {
		t.Fatalf("got %+v", got)
	}
}

type factList struct{ facts []scopedFact }

type scopedFact struct {
	scope agent.ExecutionScope
	fact  agent.Fact
}

func (s *factList) CommitFact(_ context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	s.facts = append(s.facts, scopedFact{scope: scope, fact: fact})
	return nil
}

func TestPipelineUsesProviderCallIDAndTurnScope(t *testing.T) {
	const providerID = "prov-7"
	fake := testkit.NewFake(testkit.Step{
		Finish:    "tool_calls",
		ToolCalls: []schema.FunctionToolCall{{CallID: providerID, Name: "add", Arguments: `{"n":1}`}},
	}, testkit.Step{Text: "done"})
	sink := &factList{}
	exec, err := tools.NewExecutor("gen", []tools.Definition{{
		Version: "1", Name: "add",
		Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
		Run:    func(context.Context, json.RawMessage) (string, error) { return "1", nil },
	}}, sink, allowAll{}, agent.NewBudget(config.DefaultLimits()))
	if err != nil {
		t.Fatal(err)
	}
	info := &schema.ToolInfo{Name: "add", Desc: "add"}
	fallback := agent.ExecutionScope{TraceID: "tr"}
	boundary := &turnBoundary{id: "turn-7"}
	ag, err := NewAgent(context.Background(), Deps{
		Model: fake, Tools: []tool.BaseTool{NewPipelineTool(info, exec, fallback)},
		Sink: sink, Budget: agent.NewBudget(config.DefaultLimits()), Boundary: boundary,
		Instruction: "test", Scope: fallback,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionScope(context.Background(), fallback)
	runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
	drain(t, runner.Query(ctx, "ping", adk.WithAfterToolCallsHook(FinishAfterTools(boundary, fallback))))
	var intent agent.FrozenCall
	var saw bool
	for _, item := range sink.facts {
		if item.fact.Kind != "tool_intent" {
			continue
		}
		saw = true
		if err := json.Unmarshal(item.fact.Payload, &intent); err != nil {
			t.Fatal(err)
		}
		if item.scope.TurnID != "turn-7" {
			t.Fatalf("tool scope %+v", item.scope)
		}
	}
	if !saw || intent.ProviderCallID != providerID {
		t.Fatalf("saw=%v intent=%+v", saw, intent)
	}
}

type allowAll struct{}

func (allowAll) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return agent.DecisionAllow, nil
}
