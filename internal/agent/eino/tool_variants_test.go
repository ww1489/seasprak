package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/testkit"
)

type interfaceFactSink struct {
	mu    sync.Mutex
	facts []agent.Fact
	args  string
	fail  string
}

func (s *interfaceFactSink) LookupTool(_ context.Context, scope agent.ExecutionScope, id string) (agent.ToolRecord, error) {
	return agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "product-" + id, ProviderCallID: id, Name: "work", Arguments: s.args, Generation: "gen"}}, nil
}
func (s *interfaceFactSink) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fact.Kind == s.fail {
		return errors.New("synthetic commit failure")
	}
	s.facts = append(s.facts, fact)
	return nil
}
func (s *interfaceFactSink) counts(t *testing.T) (int, int, agent.ToolObservation) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var claims, observations int
	var obs agent.ToolObservation
	for _, f := range s.facts {
		switch f.Kind {
		case "tool_intent":
			claims++
		case "tool_observation":
			observations++
			var rec agent.ToolRecord
			if err := json.Unmarshal(f.Payload, &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Observation != nil {
				obs = *rec.Observation
			}
		}
	}
	return claims, observations, obs
}

type interfaceProcess struct {
	calls atomic.Int32
	args  string
}

func (p *interfaceProcess) Execute(ctx context.Context, req agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := req.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.args = string(req.Authorization.Frozen.FinalArguments)
	p.calls.Add(1)
	return agent.ProcessObservation{Started: true, Terminated: true, SideEffect: "confirmed", Content: "完成"}, nil
}
func (*interfaceProcess) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

func TestEnhancedToolUsesClaimedExecutorThroughRealNode(t *testing.T) {
	const arguments = `{"n":9007199254740993,"text":"中文"}`
	for _, mode := range []string{"invokable", "enhanced-invokable"} {
		for _, streamed := range []bool{false, true} {
			t.Run(mode+"/stream="+strconv.FormatBool(streamed), func(t *testing.T) {
				sink := &interfaceFactSink{args: arguments}
				backend := &interfaceProcess{}
				def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"]}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"controlled"}, Resources: []agent.ExecutionResource{{Identity: "shared"}}}}
				exec, err := tools.NewExecutor("gen", []tools.Definition{def}, sink, allowAll{}, agent.NewBudget(config.DefaultLimits()), tools.WithOperations(tools.Operations{Process: backend}), tools.WithResourceScheduler(tools.NewResourceScheduler()), tools.WithResourceDomain("memory", "workspace"))
				if err != nil {
					t.Fatal(err)
				}
				base, err := NewPipelineToolForInterface(testkit.ToolInfo("work", "work"), exec, agent.ExecutionScope{SessionID: "session", Generation: "gen"}, mode)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := base.(tool.EnhancedInvokableTool); ok != (mode == "enhanced-invokable") {
					t.Fatalf("wrong Eino interface for %q: %T", mode, base)
				}
				node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{base}})
				if err != nil {
					t.Fatal(err)
				}
				input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "original-中文", Name: "work", Arguments: arguments})}}
				var output []*schema.AgenticMessage
				if !streamed {
					output, err = node.Invoke(t.Context(), input)
				} else {
					reader, streamErr := node.Stream(t.Context(), input)
					if streamErr != nil {
						t.Fatal(streamErr)
					}
					defer reader.Close()
					for {
						msgs, recvErr := reader.Recv()
						if recvErr == io.EOF {
							break
						}
						if recvErr != nil {
							t.Fatal(recvErr)
						}
						output = append(output, msgs...)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(output) != 1 || len(output[0].ContentBlocks) != 1 || output[0].ContentBlocks[0].FunctionToolResult == nil {
					t.Fatalf("result=%+v", output)
				}
				result := output[0].ContentBlocks[0].FunctionToolResult
				claims, saved, obs := sink.counts(t)
				if result.CallID != "original-中文" || backend.args != arguments || backend.calls.Load() != 1 || claims != 1 || saved != 1 || obs.Status != "succeeded" || obs.Content != "完成" {
					t.Fatalf("result=%+v args=%q calls=%d claims=%d observations=%d obs=%+v", result, backend.args, backend.calls.Load(), claims, saved, obs)
				}
			})
		}
	}
}

type interfaceAuthorizer struct {
	decision agent.Decision
	err      error
}

func (a interfaceAuthorizer) Authorize(context.Context, agent.FrozenCall) (agent.Decision, error) {
	return a.decision, a.err
}

func TestEnhancedToolRejectsBeforeBackendForSameReasons(t *testing.T) {
	for _, mode := range []string{"invokable", "enhanced-invokable"} {
		for _, tc := range []struct {
			name, failCommit string
			auth             interfaceAuthorizer
			budget           bool
			wantCode         string
			wantClaims       int
			wantSaves        int
			wantStarts       int32
		}{
			{name: "denied", auth: interfaceAuthorizer{decision: agent.DecisionDeny}, wantSaves: 1},
			{name: "ask-unavailable", auth: interfaceAuthorizer{decision: agent.DecisionAsk, err: product.NewError(product.CodeResourceUnavailable, "approval unavailable")}, wantCode: product.CodeResourceUnavailable, wantSaves: 1},
			{name: "ask-without-grant", auth: interfaceAuthorizer{decision: agent.DecisionAsk}, wantCode: product.CodeResourceUnavailable, wantSaves: 1},
			{name: "budget", auth: interfaceAuthorizer{decision: agent.DecisionAllow}, budget: true, wantCode: product.CodeBudgetExhausted, wantSaves: 1},
			{name: "observation-failure", auth: interfaceAuthorizer{decision: agent.DecisionAllow}, failCommit: "tool_observation", wantClaims: 1, wantStarts: 1},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				const args = `{"n":9007199254740993,"text":"中文"}`
				sink := &interfaceFactSink{args: args, fail: tc.failCommit}
				backend := &interfaceProcess{}
				budg := agent.NewBudget(config.DefaultLimits())
				if tc.budget {
					limits := config.DefaultLimits()
					limits.TraceToolCalls = 1
					budg = agent.NewBudget(limits)
					if err := budg.OccupyTool(); err != nil {
						t.Fatal(err)
					}
				}
				def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"},"text":{"type":"string"}},"required":["n","text"]}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"controlled"}, Resources: []agent.ExecutionResource{{Identity: "shared"}}}}
				exec, err := tools.NewExecutor("gen", []tools.Definition{def}, sink, tc.auth, budg, tools.WithOperations(tools.Operations{Process: backend}), tools.WithResourceScheduler(tools.NewResourceScheduler()), tools.WithResourceDomain("memory", "workspace"))
				if err != nil {
					t.Fatal(err)
				}
				base, err := NewPipelineToolForInterface(testkit.ToolInfo("work", "work"), exec, agent.ExecutionScope{SessionID: "session", Generation: "gen"}, mode)
				if err != nil {
					t.Fatal(err)
				}
				node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{base}})
				if err != nil {
					t.Fatal(err)
				}
				_, runErr := node.Invoke(t.Context(), &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "original", Name: "work", Arguments: args})}})
				if tc.wantCode != "" {
					var pe *product.Error
					if !errors.As(runErr, &pe) || pe.Code != tc.wantCode {
						t.Fatalf("error=%v, want %s", runErr, tc.wantCode)
					}
				} else if tc.failCommit != "" {
					if runErr == nil {
						t.Fatal("observation failure was swallowed")
					}
				} else if runErr != nil {
					t.Fatal(runErr)
				}
				claims, saves, obs := sink.counts(t)
				if backend.calls.Load() != tc.wantStarts || claims != tc.wantClaims || saves != tc.wantSaves || (tc.wantSaves == 1 && obs.Executed) {
					t.Fatalf("calls=%d claims=%d saves=%d observation=%+v", backend.calls.Load(), claims, saves, obs)
				}
			})
		}
	}
}

type variantBatchSink struct {
	interfaceFactSink
	observed map[string]*agent.ToolObservation
}

func (s *variantBatchSink) LookupTool(_ context.Context, scope agent.ExecutionScope, id string) (agent.ToolRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "product-" + id, ProviderCallID: id, Name: id, Arguments: `{}`, Generation: "gen"}, Observation: s.observed[id]}, nil
}
func (s *variantBatchSink) CommitFact(ctx context.Context, scope agent.ExecutionScope, fact agent.Fact) error {
	if err := s.interfaceFactSink.CommitFact(ctx, scope, fact); err != nil {
		return err
	}
	if fact.Kind == "tool_observation" {
		var rec agent.ToolRecord
		if err := json.Unmarshal(fact.Payload, &rec); err != nil {
			return err
		}
		s.mu.Lock()
		s.observed[rec.Call.ProviderCallID] = rec.Observation
		s.mu.Unlock()
	}
	return nil
}

type variantBatchAuthorizer struct{}

func (variantBatchAuthorizer) Authorize(_ context.Context, call agent.FrozenCall) (agent.Decision, error) {
	if call.Name == "B" {
		return agent.DecisionAsk, product.NewError(product.CodeResourceUnavailable, "approval unavailable")
	}
	return agent.DecisionAllow, nil
}

func TestEnhancedBatchUnavailableApprovalStaysAnErrorOnRepeatedNodeCall(t *testing.T) {
	for _, mode := range []string{"invokable", "enhanced-invokable"} {
		t.Run(mode, func(t *testing.T) {
			sink := &variantBatchSink{observed: make(map[string]*agent.ToolObservation)}
			backend := &interfaceProcess{}
			defs := []tools.Definition{
				{Name: "A", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"controlled"}}},
				{Name: "B", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"controlled"}}},
			}
			exec, err := tools.NewExecutor("gen", defs, sink, variantBatchAuthorizer{}, agent.NewBudget(config.DefaultLimits()), tools.WithOperations(tools.Operations{Process: backend}), tools.WithResourceScheduler(tools.NewResourceScheduler()))
			if err != nil {
				t.Fatal(err)
			}
			var base []tool.BaseTool
			for _, name := range []string{"A", "B"} {
				wrapped, wrapErr := NewPipelineToolForInterface(testkit.ToolInfo(name, name), exec, agent.ExecutionScope{SessionID: "batch", Generation: "gen"}, mode)
				if wrapErr != nil {
					t.Fatal(wrapErr)
				}
				base = append(base, wrapped)
			}
			node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: base})
			if err != nil {
				t.Fatal(err)
			}
			input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{
				schema.NewContentBlock(&schema.FunctionToolCall{CallID: "A", Name: "A", Arguments: `{}`}),
				schema.NewContentBlock(&schema.FunctionToolCall{CallID: "B", Name: "B", Arguments: `{}`}),
			}}
			for i := 0; i < 2; i++ {
				_, runErr := node.Invoke(t.Context(), input)
				var pe *product.Error
				if !errors.As(runErr, &pe) || pe.Code != product.CodeResourceUnavailable {
					t.Fatalf("batch %d must remain unavailable, error=%v", i, runErr)
				}
				claims, _, _ := sink.counts(t)
				if backend.calls.Load() != 1 || claims != 1 || sink.observed["A"] == nil || sink.observed["A"].Status != "succeeded" || sink.observed["B"] == nil || sink.observed["B"].Status != "denied" {
					t.Fatalf("attempt %d: starts=%d claims=%d A=%+v B=%+v", i, backend.calls.Load(), claims, sink.observed["A"], sink.observed["B"])
				}
			}
		})
	}
}

func TestEnhancedToolRejectsUnknownInterface(t *testing.T) {
	base, err := NewPipelineToolForInterface(testkit.ToolInfo("work", "work"), nil, agent.ExecutionScope{}, "unknown")
	pe, ok := product.AsError(err)
	if base != nil || !ok || pe.Code != product.CodeInvalidArgument {
		t.Fatalf("base=%T err=%v", base, err)
	}
}
