package workflowagent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

type handoffView struct {
	InstanceID   string                               `json:"instanceId"`
	Models       map[string]agent.ModelStreamSnapshot `json:"models"`
	Tools        map[string]WorkflowToolPreview       `json:"tools"`
	Interactions map[string]WorkflowInteraction       `json:"interactions"`
}

func receiveHandoff(t *testing.T, sub *WorkflowSubscription, kind string) handoffView {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription closed before handoff: %v", sub.Err())
			}
			if ev.DurableSeq != nil {
				if *ev.DurableSeq > sub.Handoff {
					t.Fatal("post-handoff durable event arrived before view")
				}
				continue
			}
			if ev.Type != kind {
				t.Fatalf("live event %s arrived before handoff view %s", ev.Type, kind)
			}
			if ev.StreamID == "" || ev.ChunkSeq == nil {
				t.Fatal("handoff pretended to be durable")
			}
			view := handoffView{Models: map[string]agent.ModelStreamSnapshot{}, Tools: map[string]WorkflowToolPreview{}, Interactions: map[string]WorkflowInteraction{}}
			switch kind {
			case "message.snapshot":
				var snapshot agent.ModelStreamSnapshot
				if err := json.Unmarshal(ev.Payload, &snapshot); err != nil {
					t.Fatal(err)
				}
				view.Models[ev.StreamID] = snapshot
			case "tool.output.snapshot":
				var preview WorkflowToolPreview
				if err := json.Unmarshal(ev.Payload, &preview); err != nil {
					t.Fatal(err)
				}
				view.Tools[preview.ToolCallID] = preview
			case "interaction.requested":
				var question WorkflowInteraction
				if err := json.Unmarshal(ev.Payload, &question); err != nil {
					t.Fatal(err)
				}
				view.InstanceID = question.InstanceID
				view.Interactions[question.ID] = question
			}
			return view
		case <-deadline.C:
			t.Fatal("registration lost the only pre-existing transient view")
		}
	}
}

func TestWorkflowSubscribeAtomicTransientHandoff(t *testing.T) {
	t.Run("tool", func(t *testing.T) {
		var effects atomic.Int32
		opts := testOptions(t, toolOnly(), nil, &effects)
		emitted, gate, post := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(gate) })
		defer release()
		opts.Tools[0].Run = nil
		opts.Tools[0].RunWithOutput = func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
			effects.Add(1)
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "before"}); err != nil {
				return "", err
			}
			close(emitted)
			<-gate
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "after"}); err != nil {
				return "", err
			}
			close(post)
			return "done", nil
		}
		w := newWorkflow(t, opts)
		before, _ := w.Snapshot(t.Context())
		submit(t, w)
		<-emitted
		sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: before.DurableSeq})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		release()
		<-post
		waitStopped(t, w)
		view := receiveHandoff(t, sub, "tool.output.snapshot")
		if len(view.Tools) != 1 {
			t.Fatalf("wrong owned handoff: %+v", view)
		}
		for _, preview := range view.Tools {
			if preview.Text != "before" {
				t.Fatalf("handoff changed after registration: %q", preview.Text)
			}
		}
		foundAfter, terminalSeen := false, false
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		for !terminalSeen {
			select {
			case ev := <-sub.Events:
				if ev.Type == "tool.output.delta" {
					var d agent.ToolOutputDelta
					json.Unmarshal(ev.Payload, &d)
					if d.Text != "after" || ev.DurableSeq != nil {
						t.Fatalf("wrong B-after delta %+v", d)
					}
					foundAfter = true
				}
				if ev.Type == "workflow.state_changed" {
					var p struct{ State string }
					json.Unmarshal(ev.Payload, &p)
					terminalSeen = p.State == "completed"
				}
			case <-deadline.C:
				t.Fatal("live handoff lost post-registration events")
			}
		}
		if !foundAfter || effects.Load() != 1 {
			t.Fatal("handoff duplicated/lost effects or output")
		}
	})
	t.Run("model", func(t *testing.T) {
		var effects atomic.Int32
		m := &handoffStreamModel{gate: make(chan struct{})}
		release := sync.OnceFunc(func() { close(m.gate) })
		defer release()
		w := newWorkflow(t, testOptions(t, modelThenTool(), m, &effects))
		before, _ := w.Snapshot(t.Context())
		submit(t, w)
		deadline := time.Now().Add(2 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			s, _ := w.Snapshot(t.Context())
			if len(s.Transient.Models) == 1 {
				ready = true
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !ready {
			t.Fatal("stream fixture did not publish its first real chunk")
		}
		sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: before.DurableSeq})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		view := receiveHandoff(t, sub, "message.snapshot")
		if len(view.Models) != 1 || effects.Load() != 0 || m.calls.Load() != 1 {
			t.Fatalf("missing model view or early tool: %+v", view)
		}
		for _, snapshot := range view.Models {
			if len(snapshot.Blocks) != 1 || snapshot.Blocks[0].Text != "before" {
				t.Fatalf("wrong model snapshot %+v", snapshot)
			}
		}
		release()
		if s := waitStopped(t, w); s.State != "completed" || effects.Load() != 1 {
			t.Fatalf("stream final %s tool=%d", s.State, effects.Load())
		}
	})
	t.Run("approval", func(t *testing.T) {
		var effects atomic.Int32
		opts := testOptions(t, toolOnly(), nil, &effects)
		opts.Tools[0].Execution.RequestedGrantRef = "once"
		w := newWorkflow(t, opts)
		before, _ := w.Snapshot(t.Context())
		submit(t, w)
		waiting := waitStopped(t, w)
		sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: before.DurableSeq})
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
		view := receiveHandoff(t, sub, "interaction.requested")
		if len(view.Interactions) != 1 || effects.Load() != 0 {
			t.Fatalf("approval lost %+v", view)
		}
		for id, q := range view.Interactions {
			if q.State != "ready" || q.InstanceID != waiting.InstanceID || waiting.Interactions[id].ID != id {
				t.Errorf("stale approval %+v", q)
			}
		}
	})
}

type handoffStreamModel struct {
	gate  chan struct{}
	calls atomic.Int32
}

func (*handoffStreamModel) Configuration() llm.ModelConfig {
	return llm.ModelConfig{Version: "handoff-v1", Capabilities: llm.ModelCapabilities{Items: map[llm.CapabilityName]llm.Capability{llm.CapTextStream: {Status: llm.Verified}}}}
}
func (*handoffStreamModel) Generate(context.Context, []*schema.AgenticMessage, ...model.Option) (*schema.AgenticMessage, error) {
	return nil, product.NewError(product.CodeInternal, "unexpected Generate")
}
func (m *handoffStreamModel) Stream(ctx context.Context, _ []*schema.AgenticMessage, _ ...model.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	m.calls.Add(1)
	reader, writer := schema.Pipe[*schema.AgenticMessage](1)
	go func() {
		defer writer.Close()
		chunk := func(text string, finish bool) *schema.AgenticMessage {
			block := schema.NewContentBlock(&schema.AssistantGenText{Text: text})
			block.StreamingMeta = &schema.StreamingMeta{Index: 0}
			msg := &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{block}}
			if finish {
				msg.Extra = map[string]any{"seasprak.finish": "stop"}
			}
			return msg
		}
		writer.Send(chunk("before", false), nil)
		select {
		case <-m.gate:
			writer.Send(chunk("after", true), nil)
		case <-ctx.Done():
			writer.Send(nil, ctx.Err())
		}
	}()
	return reader, nil
}

func TestWorkflowTransientHandoffHonorsByteLimit(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	opts.Tools[0].Execution.RequestedGrantRef = "once"
	w := newWorkflow(t, opts)
	submit(t, w)
	waiting := waitStopped(t, w)
	sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: waiting.DurableSeq, Limits: config.Limits{SubscriptionBytes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case _, ok := <-sub.Events:
		if ok {
			t.Fatal("oversized handoff escaped byte bound")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oversized handoff did not stop")
	}
	requireCode(t, sub.Err(), product.CodeResyncRequired)
}
