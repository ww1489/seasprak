package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/testkit"
)

type streamWorkflowModel struct {
	fake             *testkit.FakeModel
	generate, stream atomic.Int32
}

func (*streamWorkflowModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "workflow-stream-v1", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapTextStream: {Status: llm.Verified}}}}
}
func (m *streamWorkflowModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	m.generate.Add(1)
	return nil, product.NewError(product.CodeUnsupportedCapability, "stream fixture requires verified streaming")
}
func (m *streamWorkflowModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.stream.Add(1)
	return m.fake.Stream(ctx, in, opts...)
}
func TestWorkflowVerifiedStreamUsesFullValidationAndOwnFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		step testkit.Step
		ok   bool
	}{{"complete", testkit.Step{Text: "complete"}, true}, {"truncated", testkit.Step{Text: "partial", Truncated: true}, false}, {"missing_finish", testkit.Step{Text: "partial", NoFinish: true}, false}, {"unexpected_tool", testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "early", Name: "echo", Arguments: `{}`}}}, false}, {"wrong_role", testkit.Step{Text: "partial", Role: schema.AgenticRoleTypeUser}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			m := &streamWorkflowModel{fake: testkit.NewFake(tc.step)}
			var tool atomic.Int32
			w := newWorkflow(t, testOptions(t, modelThenTool(), m, &tool))
			before, _ := w.Snapshot(t.Context())
			sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: before.DurableSeq})
			if err != nil {
				t.Fatal(err)
			}
			defer sub.Close()
			events := make(chan []agent.Event, 1)
			go func() {
				var all []agent.Event
				for ev := range sub.Events {
					all = append(all, ev)
					if ev.Type == "workflow.state_changed" {
						var p struct{ State string }
						json.Unmarshal(ev.Payload, &p)
						if p.State == "completed" || p.State == "failed" {
							events <- all
							return
						}
					}
				}
			}()
			submit(t, w)
			s := waitStopped(t, w)
			if m.generate.Load() != 0 || m.stream.Load() != 1 || m.fake.Calls() != 1 {
				t.Errorf("wrong model path Generate=%d Stream=%d actual=%d", m.generate.Load(), m.stream.Load(), m.fake.Calls())
			}
			if tc.ok {
				if s.State != "completed" || tool.Load() != 1 {
					t.Errorf("valid stream %+v tool=%d", s, tool.Load())
				}
			} else if s.State != "failed" || s.ErrorCode != product.CodeInvalidArgument || tool.Load() != 0 {
				t.Errorf("invalid stream state=%s code=%s tool=%d", s.State, s.ErrorCode, tool.Load())
			}
			all := <-events
			snapshots := 0
			for _, ev := range all {
				if ev.Type == "message.snapshot" {
					snapshots++
					if ev.DurableSeq != nil || ev.Scope.SessionID != "" || ev.Scope.WorkflowRunID != s.RunID {
						t.Errorf("temporary stream scope %+v", ev)
					}
				}
			}
			if snapshots == 0 {
				t.Error("stream did not publish a temporary snapshot")
			}
			if s.Usage.TransportRequests != 1 || s.Usage.LogicalModelCalls != 1 || len(s.Transient.Models) != 0 {
				t.Errorf("stream occupancy/terminal %+v", s)
			}
		})
	}
}
