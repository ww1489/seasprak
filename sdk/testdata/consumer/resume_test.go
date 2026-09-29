package consumer_test

import (
	"context"
	"encoding/json"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type consumerResumeModel struct {
	calls atomic.Int32
	final bool
}

func (*consumerResumeModel) Configuration() sdk.ModelConfig {
	return sdk.ModelConfig{Version: "consumer-model-v1"}
}
func (m *consumerResumeModel) Generate(_ context.Context, _ []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	if !m.final {
		return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: "provider", Name: "work", Arguments: `{}`})}, Extra: map[string]any{"seasprak.finish": "tool_calls"}}, nil
	}
	return &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.AssistantGenText{Text: "done"})}, Extra: map[string]any{"seasprak.finish": "stop"}}, nil
}
func (m *consumerResumeModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestSDKConsumerExplicitlyResumesAfterReopen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	def := sdk.ToolDefinition{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(context.Context, json.RawMessage) (string, error) {
		runs.Add(1)
		close(entered)
		<-release
		return "saved", nil
	}}
	def.Execution.Effect = "read"
	model := &consumerResumeModel{}
	opts := sdk.SessionOptions{SessionID: "consumer-resume", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model, Tools: []sdk.ToolDefinition{def}, GenerationFingerprint: "consumer-compiled-bundle-v1"}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	// Release the uncooperative callback before Close, including Fatal paths.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not enter")
	}
	// Recovery is a successful Pause/Close/Open/Resume contract. Acceptance
	// has no 80ms SLA. Post-receipt wait cancellation is tested independently
	// in sessions with a context that observes Pause's actual waiting select.
	type pauseResult struct {
		receipt sdk.OperationReceipt
		err     error
	}
	paused := make(chan pauseResult, 1)
	go func() {
		receipt, err := s.Pause(t.Context(), input.TraceID)
		paused <- pauseResult{receipt, err}
	}()
	var pause sdk.OperationReceipt
	deadline := time.After(5 * time.Second)
	for pause.OperationID == "" {
		snapshot, err := s.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range snapshot.Operations {
			if op.Kind == "pause" && op.Receipt.Target == input.TraceID {
				pause = op.Receipt
				if op.State != "accepted" || snapshot.Traces[input.TraceID].ExecutionStopped || snapshot.Traces[input.TraceID].State != "running" || runs.Load() != 1 || model.calls.Load() != 1 {
					t.Fatalf("pause settled before tool exit: operation=%+v trace=%+v", op, snapshot.Traces[input.TraceID])
				}
			}
		}
		select {
		case <-deadline:
			t.Fatal("pause not accepted")
		default:
			runtime.Gosched()
		}
	}
	select {
	case result := <-paused:
		t.Fatalf("pause returned before tool exit: %+v err=%v", result.receipt, result.err)
	default:
	}
	close(release)
	select {
	case result := <-paused:
		if result.err != nil || result.receipt != pause || pause.OperationID == "" {
			t.Fatalf("pause receipt=%+v accepted=%+v err=%v", result.receipt, pause, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not return after tool exit")
	}
	deadline = time.After(5 * time.Second)
	for {
		status, err := s.GetOperation(t.Context(), pause.OperationID)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("pause did not complete")
		default:
			runtime.Gosched()
		}
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumedModel := &consumerResumeModel{final: true}
	opts.Model = resumedModel
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	snap, err := opened.Snapshot(t.Context())
	if err != nil || !snap.Resume[input.TraceID].CanResume || resumedModel.calls.Load() != 0 || runs.Load() != 1 {
		t.Fatalf("open snapshot=%+v err=%v", snap.Resume, err)
	}
	cmd := sdk.ResumeCommand{TraceID: input.TraceID, ExpectedRevision: snap.Revision, IdempotencyKey: "retry"}
	receipt, err := opened.Resume(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.After(5 * time.Second)
	for {
		snap, err = opened.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if snap.Traces[input.TraceID].State == "completed" {
			break
		}
		select {
		case <-deadline:
			t.Fatal("resume did not complete")
		default:
			runtime.Gosched()
		}
	}
	again, err := opened.Resume(t.Context(), cmd)
	if err != nil || again != receipt || resumedModel.calls.Load() != 1 || model.calls.Load() != 1 || runs.Load() != 1 || snap.Traces[input.TraceID].Usage.ToolExecutions != 1 {
		t.Fatalf("resume receipt=%+v again=%+v err=%v model=%d/%d tools=%d", receipt, again, err, model.calls.Load(), resumedModel.calls.Load(), runs.Load())
	}
}
