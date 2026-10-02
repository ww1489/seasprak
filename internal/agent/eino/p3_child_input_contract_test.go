package eino

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
)

// Negative checks certify the probe's own acceptance oracle. The positive
// framework tests separately exercise NewAgent, AgentTool and native resume.
func TestP3ChildResumeContractInputCertificationRejectsCorruption(t *testing.T) {
	mutations := map[string]func([]*schema.AgenticMessage) []*schema.AgenticMessage{
		"missing_task": func(in []*schema.AgenticMessage) []*schema.AgenticMessage { return in[1:] },
		"duplicate_task": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			return append([]*schema.AgenticMessage{in[0]}, in...)
		},
		"foreign_task": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[0] = schema.UserAgenticMessage("task:foreign")
			return in
		},
		"missing_call": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[1].ContentBlocks = in[1].ContentBlocks[1:]
			return in
		},
		"call_id": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[1].ContentBlocks[0].FunctionToolCall.CallID = "foreign"
			return in
		},
		"call_name": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[1].ContentBlocks[0].FunctionToolCall.Name = "foreign"
			return in
		},
		"call_arguments": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[1].ContentBlocks[0].FunctionToolCall.Arguments = `{}`
			return in
		},
		"call_order": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[1].ContentBlocks[0], in[1].ContentBlocks[1] = in[1].ContentBlocks[1], in[1].ContentBlocks[0]
			return in
		},
		"missing_result":   func(in []*schema.AgenticMessage) []*schema.AgenticMessage { return in[:len(in)-1] },
		"duplicate_result": func(in []*schema.AgenticMessage) []*schema.AgenticMessage { return append(in, in[2]) },
		"result_id": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[2].ContentBlocks[0].FunctionToolResult.CallID = "foreign"
			return in
		},
		"result_name": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[2].ContentBlocks[0].FunctionToolResult.Name = "foreign"
			return in
		},
		"result_content": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			in[2].ContentBlocks[0].FunctionToolResult.Content[0].Text.Text = "foreign"
			return in
		},
		"result_order": func(in []*schema.AgenticMessage) []*schema.AgenticMessage { in[2], in[3] = in[3], in[2]; return in },
		"foreign_result": func(in []*schema.AgenticMessage) []*schema.AgenticMessage {
			r := p3ToolResult("foreign", "approve", "foreign")
			in[2].ContentBlocks[0] = schema.NewContentBlock(&r)
			return in
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := newP3ChildProbe()
			m := p.models["root"]
			ctx := WithExecutionScope(t.Context(), p.scope)
			first, err := m.FakeModel.Generate(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			first.Extra["p3-child-contract"] = p3ChildMetadata(p.scope.InvocationID)
			in := []*schema.AgenticMessage{schema.UserAgenticMessage(m.task), first}
			for _, expected := range m.results {
				// Build independent values so corruption cannot mutate the oracle.
				r := p3ToolResult(expected.CallID, expected.Name, expected.Content[0].Text.Text)
				in = append(in, &schema.AgenticMessage{Role: schema.AgenticRoleTypeUser, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&r)}})
			}
			if err := m.checkInput(ctx, in); err != nil {
				t.Fatalf("positive acceptance oracle is invalid: %v", err)
			}
			if _, err := m.Generate(ctx, mutate(in)); err == nil || m.Calls() != 1 || m.generates.Load() != 1 {
				t.Fatal("corrupted input passed certification or reached another fake request")
			}
		})
	}
}

func TestP3ChildResumeContractFullScopeCertification(t *testing.T) {
	mutations := map[string]func(*agent.ExecutionScope){
		"session":    func(s *agent.ExecutionScope) { s.SessionID = "foreign" },
		"branch":     func(s *agent.ExecutionScope) { s.BranchID = "foreign" },
		"trace":      func(s *agent.ExecutionScope) { s.TraceID = "foreign" },
		"invocation": func(s *agent.ExecutionScope) { s.InvocationID = "foreign" },
		"parent":     func(s *agent.ExecutionScope) { s.ParentInvocationID = "foreign" },
		"generation": func(s *agent.ExecutionScope) { s.Generation = "foreign" },
		"execution":  func(s *agent.ExecutionScope) { s.ExecutionID = "foreign" },
		"turn":       func(s *agent.ExecutionScope) { s.TurnID = "foreign" },
		"selection":  func(s *agent.ExecutionScope) { s.SelectionRevision++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := newP3ChildProbe()
			scope := p.scope
			mutate(&scope)
			m := p.models["root"]
			if _, err := m.Generate(WithExecutionScope(t.Context(), scope), []*schema.AgenticMessage{schema.UserAgenticMessage(m.task)}); err == nil || m.Calls() != 0 || m.generates.Load() != 1 {
				t.Fatal("foreign scope passed certification or invoked the fake model")
			}
		})
	}
}
