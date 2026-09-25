package consumer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type toolModel struct {
	calls     atomic.Int32
	sawResult atomic.Bool
}

func (m *toolModel) Generate(ctx context.Context, in []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.calls.Add(1) == 1 {
		return &schema.AgenticMessage{
			Role:          schema.AgenticRoleTypeAssistant,
			ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider-1", Name: "echo", Arguments: `{"n":1}`})},
			Extra:         map[string]any{"seasprak.finish": "tool_calls"},
		}, nil
	}
	for _, msg := range in {
		for _, block := range msg.ContentBlocks {
			if block.FunctionToolResult != nil && block.FunctionToolResult.CallID == "provider-1" {
				m.sawResult.Store(true)
			}
		}
	}
	return &schema.AgenticMessage{
		Role:          schema.AgenticRoleTypeAssistant,
		ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})},
		Extra:         map[string]any{"seasprak.finish": "stop"},
	}, nil
}

func (m *toolModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

type executionProbe struct {
	mu           sync.Mutex
	decision     sdk.Decision
	stop         bool
	turns        int
	finished     []sdk.TurnFact
	authorized   []sdk.FrozenCall
	intents      []sdk.FrozenCall
	observations []sdk.ToolRecord
}

func (p *executionProbe) CommitFact(_ context.Context, scope sdk.ExecutionScope, fact sdk.Fact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if scope.TurnID != fmt.Sprintf("turn-%d", p.turns) {
		return sdk.NewError(sdk.CodeStateConflict, "fact has the wrong turn")
	}
	switch fact.Kind {
	case "tool_intent":
		var call sdk.FrozenCall
		if err := json.Unmarshal(fact.Payload, &call); err != nil {
			return err
		}
		p.intents = append(p.intents, call)
	case "tool_observation":
		var record sdk.ToolRecord
		if err := json.Unmarshal(fact.Payload, &record); err != nil {
			return err
		}
		p.observations = append(p.observations, record)
	}
	return nil
}

func (p *executionProbe) Authorize(_ context.Context, call sdk.FrozenCall) (sdk.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorized = append(p.authorized, call)
	return p.decision, nil
}

func (p *executionProbe) PrepareNextTurn(context.Context, sdk.ExecutionScope) (sdk.TurnPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.turns != len(p.finished) {
		return sdk.TurnPlan{}, sdk.NewError(sdk.CodeStateConflict, "previous turn was not finished")
	}
	p.turns++
	return sdk.TurnPlan{TurnID: fmt.Sprintf("turn-%d", p.turns)}, nil
}

func (p *executionProbe) ShouldStop(context.Context, sdk.ExecutionScope) (bool, string, error) {
	return p.stop, "consumer boundary", nil
}

func (p *executionProbe) FinishTurn(_ context.Context, scope sdk.ExecutionScope, fact sdk.TurnFact) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if fact.TurnID != scope.TurnID || fact.TurnID != fmt.Sprintf("turn-%d", p.turns) {
		return sdk.NewError(sdk.CodeStateConflict, "finished the wrong turn")
	}
	p.finished = append(p.finished, fact)
	return nil
}

func (p *executionProbe) TakeSteering(context.Context, sdk.ExecutionScope) (*sdk.AgentMessage, error) {
	return nil, nil
}

func TestIndependentAgentToolPipeline(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		decision              sdk.Decision
		stop                  bool
		wantTools, wantModels int
	}{
		{"allow", sdk.DecisionAllow, false, 1, 2},
		{"deny", sdk.DecisionDeny, false, 0, 2},
		{"controlled-stop", sdk.DecisionAllow, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &toolModel{}
			probe := &executionProbe{decision: tc.decision, stop: tc.stop}
			budget := sdk.NewBudget(sdk.Limits{TraceLogicalModelCalls: 2, TraceTransportRequests: 2, TraceToolCalls: 1})
			scope := sdk.ExecutionScope{SessionID: "standalone", TraceID: "trace", InvocationID: "invocation", Generation: "generation"}
			var runs atomic.Int32
			executor, err := sdk.NewExecutor(scope.Generation, []sdk.ToolDefinition{{
				Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`),
				Run: func(ctx context.Context, args json.RawMessage) (string, error) {
					runs.Add(1)
					if sdk.ScopeFromContext(ctx, sdk.ExecutionScope{}).TurnID != "turn-1" {
						return "", errors.New("tool lost turn scope")
					}
					return string(args), nil
				},
			}}, probe, probe, budget)
			if err != nil {
				t.Fatal(err)
			}
			pipeline := sdk.NewPipelineTool(&schema.ToolInfo{Name: "echo", Desc: "echo", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"n": {Type: schema.Integer, Required: true}})}, executor, scope)
			ag, err := sdk.NewAgent(context.Background(), sdk.AgentDeps{
				Model: model, Tools: []tool.BaseTool{pipeline}, Sink: probe, Budget: budget,
				Boundary: probe, Scope: scope, Instruction: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := sdk.WithExecutionScope(context.Background(), scope)
			runner := adk.NewTypedRunner(adk.TypedRunnerConfig[*schema.AgenticMessage]{Agent: ag})
			iter := runner.Query(ctx, "echo once", adk.WithAfterToolCallsHook(sdk.FinishAfterTools(probe, scope)))
			var stopped bool
			for {
				event, ok := iter.Next()
				if !ok {
					break
				}
				if event.Err != nil {
					if !tc.stop || !errors.Is(event.Err, sdk.ErrControlledStop) {
						t.Fatal(event.Err)
					}
					stopped = true
				}
			}
			if stopped != tc.stop || int(model.calls.Load()) != tc.wantModels || int(runs.Load()) != tc.wantTools {
				t.Fatalf("stopped=%v model calls=%d tool calls=%d", stopped, model.calls.Load(), runs.Load())
			}
			if !tc.stop && !model.sawResult.Load() {
				t.Fatal("model did not receive the tool result")
			}
			usage := budget.Snapshot()
			if usage.LogicalModelCalls != tc.wantModels || usage.TransportRequests != tc.wantModels || usage.ToolExecutions != tc.wantTools {
				t.Fatalf("usage = %+v", usage)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if len(probe.authorized) != 1 || probe.authorized[0].ProviderCallID != "provider-1" {
				t.Fatalf("authorization calls = %+v", probe.authorized)
			}
			if len(probe.finished) != tc.wantModels || !probe.finished[0].HasTools || probe.finished[0].Stop != tc.stop {
				t.Fatalf("finished turns = %+v", probe.finished)
			}
			if len(probe.intents) != tc.wantTools || len(probe.observations) != tc.wantTools {
				t.Fatalf("intents=%d observations=%d", len(probe.intents), len(probe.observations))
			}
			for _, record := range probe.observations {
				if record.Scope.TurnID != "turn-1" || record.Call.ProviderCallID != "provider-1" || record.Observation == nil || !record.Observation.Executed || record.Observation.Status != "succeeded" {
					t.Fatalf("observation = %+v", record)
				}
			}
		})
	}
}

func TestPipelineCancellationHasNoSideEffects(t *testing.T) {
	probe := &executionProbe{decision: sdk.DecisionAllow}
	budget := sdk.NewBudget(sdk.DefaultLimits())
	var runs atomic.Int32
	executor, err := sdk.NewExecutor("generation", []sdk.ToolDefinition{{
		Name: "echo", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`),
		Run: func(context.Context, json.RawMessage) (string, error) { runs.Add(1); return "", nil },
	}}, probe, probe, budget)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := sdk.NewPipelineTool(&schema.ToolInfo{Name: "echo"}, executor, sdk.ExecutionScope{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pipeline.InvokableRun(ctx, `{}`); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if runs.Load() != 0 || len(probe.authorized) != 0 || len(probe.intents) != 0 || budget.Snapshot().ToolExecutions != 0 {
		t.Fatal("cancelled tool produced side effects")
	}
}
