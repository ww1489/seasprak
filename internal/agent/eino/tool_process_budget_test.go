package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/testkit"
	fixture "github.com/ww1489/seasprak/internal/testkit/operations"
)

type processBudgetBackend struct {
	*fixture.Memory
	output      string
	exit, calls int
}

func (p *processBudgetBackend) Execute(ctx context.Context, request agent.AuthorizedProcess, _ agent.ProgressSink) (agent.ProcessObservation, error) {
	if err := request.Authorization.Validate(ctx); err != nil {
		return agent.ProcessObservation{}, err
	}
	p.calls++
	return agent.ProcessObservation{Content: p.output, Started: true, Terminated: true, ExitCode: p.exit, SideEffect: "confirmed"}, nil
}
func (*processBudgetBackend) Stop(context.Context, agent.ExecutionRef) (agent.StopObservation, error) {
	return agent.StopObservation{Terminated: true}, nil
}

type processBudgetArtifacts struct {
	*fixture.Memory
	fail  bool
	calls int
	full  string
	ref   agent.ArtifactRef
}

func (s *processBudgetArtifacts) SaveOutput(ctx context.Context, in agent.OutputArtifactInput) (agent.ArtifactRef, error) {
	s.calls++
	s.full = in.Content
	if s.fail {
		return agent.ArtifactRef{}, errors.New("synthetic output storage failure")
	}
	ref, err := s.Memory.SaveOutput(ctx, in)
	s.ref = ref
	return ref, err
}

type processBudgetSink struct {
	mu         sync.Mutex
	record     agent.ToolRecord
	projection *agent.ToolOutputProjection
	claims     int
}

func (s *processBudgetSink) LookupTool(_ context.Context, scope agent.ExecutionScope, id string) (agent.ToolRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Observation != nil {
		rec := s.record
		rec.Projection = s.projection
		return rec, nil
	}
	return agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: id, ProviderCallID: id, Name: "work", Arguments: `{}`, Generation: "gen"}}, nil
}
func (s *processBudgetSink) CommitFact(_ context.Context, _ agent.ExecutionScope, fact agent.Fact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch fact.Kind {
	case "tool_intent":
		s.claims++
	case "tool_observation":
		return json.Unmarshal(fact.Payload, &s.record)
	case "tool_output_projection":
		return json.Unmarshal(fact.Payload, &s.projection)
	}
	return nil
}

func TestProcessModelJSONBudgetThroughEinoAndRecovery(t *testing.T) {
	for _, unit := range []string{`"`, `\`, `<`, "中🙂\"\\<"} {
		for _, exit := range []int{0, 23} {
			for _, failSave := range []bool{false, true} {
				t.Run(fmt.Sprintf("%q/exit=%d/saveFailure=%v", unit, exit, failSave), func(t *testing.T) {
					output := "HEAD" + strings.Repeat(unit, 60000/len(unit)+1) + "TAIL"
					backend := &processBudgetBackend{Memory: fixture.NewMemory(), output: output, exit: exit}
					artifacts := &processBudgetArtifacts{Memory: fixture.NewMemory(), fail: failSave}
					sink := &processBudgetSink{}
					def := tools.Definition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{BackendID: "process-operations", Effect: "write", Argv: []string{"controlled"}}}
					input := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "budget-call", Name: "work", Arguments: `{}`})}}
					for _, phase := range []string{"initial", "projection-recovery", "observation-recovery"} {
						// Rebuild execution and Eino state from JSON-decoded facts.
						exec, err := tools.NewExecutor("gen", []tools.Definition{def}, sink, allowAll{}, agent.NewBudget(config.DefaultLimits()), tools.WithOperations(tools.Operations{Process: backend, Artifacts: artifacts, OutputRedactor: func(_ context.Context, text string) (string, error) { return text, nil }}), tools.WithResourceScheduler(tools.NewResourceScheduler()), tools.WithResourceDomain("memory", "workspace"))
						if err != nil {
							t.Fatal(err)
						}
						base := NewPipelineTool(testkit.ToolInfo("work", "work"), exec, agent.ExecutionScope{SessionID: "session", Generation: "gen"})
						node, err := compose.NewAgenticToolsNode(t.Context(), &compose.ToolsNodeConfig{Tools: []tool.BaseTool{base}})
						if err != nil {
							t.Fatal(err)
						}
						if phase == "observation-recovery" {
							sink.projection = nil
						}
						messages, err := node.Invoke(t.Context(), input)
						if err != nil {
							t.Fatal(err)
						}
						result := messages[0].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text
						assertProcessBudgetJSON(t, phase, result)
						var envelope struct {
							Content                       string
							Truncated                     bool
							Artifact                      agent.ArtifactRef
							LogError                      string
							ExitCode                      int
							Status, SideEffect            string
							Executed, Process, Terminated bool
						}
						if err := json.Unmarshal([]byte(result), &envelope); err != nil {
							t.Fatal(err)
						}
						if !envelope.Truncated || !utf8.ValidString(envelope.Content) || !strings.HasPrefix(envelope.Content, "HEAD") || !strings.HasSuffix(envelope.Content, "TAIL") {
							t.Errorf("%s: preview lost UTF-8 head/tail or truncation", phase)
						}
						if phase != "observation-recovery" && !failSave {
							if envelope.Artifact != artifacts.ref {
								t.Error("artifact identity changed")
							}
						} else if envelope.Artifact.ID != "" || !strings.HasPrefix(envelope.LogError, "resource_unavailable:") {
							t.Errorf("%s: missing artifact must retain unavailable code", phase)
						}
						if exit != 0 && (envelope.ExitCode != exit || envelope.Status != "failed" || envelope.SideEffect != "confirmed" || !envelope.Executed || !envelope.Process || !envelope.Terminated) {
							t.Errorf("%s: failed model envelope lost execution facts", phase)
						}
						obs := sink.record.Observation
						wantStatus := "succeeded"
						if exit != 0 {
							wantStatus = "failed"
						}
						if obs.Status != wantStatus || obs.ExitCode != exit || !obs.Executed || !obs.Process || !obs.Terminated || obs.SideEffect != "confirmed" {
							t.Errorf("%s: durable execution facts changed", phase)
						}
						assertProcessBudgetJSON(t, phase+"/observation", obs.ModelContent())
						if sink.projection != nil {
							assertProcessBudgetJSON(t, phase+"/projection", sink.projection.ModelContent())
						}
						if backend.calls != 1 || artifacts.calls != 1 || sink.claims != 1 || artifacts.full != output {
							t.Errorf("%s: calls=%d saves=%d claims=%d fullEqual=%v", phase, backend.calls, artifacts.calls, sink.claims, artifacts.full == output)
						}
					}
				})
			}
		}
	}
}

func assertProcessBudgetJSON(t *testing.T, phase, text string) {
	t.Helper()
	if len(text) > 51200 || !json.Valid([]byte(text)) {
		t.Errorf("%s: model JSON bytes=%d valid=%v; want <=51200", phase, len(text), json.Valid([]byte(text)))
	}
}
