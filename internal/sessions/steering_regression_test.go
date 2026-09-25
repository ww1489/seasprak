package sessions_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

type signaledModel struct {
	einomodel.AgenticModel
	entered chan struct{}
}

func (m *signaledModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	select {
	case m.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return m.AgenticModel.Generate(ctx, in, opts...)
}

func (m *signaledModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	msg, err := m.Generate(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.AgenticMessage{msg}), nil
}

func TestRuntimeNoToolSteeringPrecedesFollowUp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		count  int
		follow bool
	}{{"steering_only", 1, false}, {"steering_before_follow", 1, true}, {"consecutive_steering", 2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			gate := make(chan struct{})
			fake := testkit.NewFake(testkit.Step{Gate: gate, Text: "first final"})
			capture := &capturingModel{inner: fake}
			model := &signaledModel{AgenticModel: capture, entered: make(chan struct{}, 8)}
			s, manager := runtimeSession(t, model)
			first := submit(t, s)
			select {
			case <-model.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("first model did not start")
			}
			before := manager.View().Traces[first.TraceID]
			// Accept follow-up first to prove steering priority is not arrival order.
			var follow agent.InputReceipt
			if tc.follow {
				var err error
				follow, err = s.SubmitInput(context.Background(), agent.InputCommand{Kind: "follow_up", TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"follow"}`)})
				if err != nil {
					t.Fatal(err)
				}
			}
			inputs := []agent.InputReceipt{first}
			wantTexts := []string{"hi"}
			for i := 0; i < tc.count; i++ {
				text := "steering " + string(rune('A'+i))
				body, _ := json.Marshal(map[string]string{"text": text})
				r, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "steering", TargetTraceID: first.TraceID, Content: body})
				if err != nil {
					t.Fatal(err)
				}
				inputs = append(inputs, r)
				wantTexts = append(wantTexts, text)
			}
			if tc.follow {
				inputs = append(inputs, follow)
				wantTexts = append(wantTexts, "follow")
			}
			close(gate)
			waitState(t, s, first.TraceID, "completed")
			view := manager.View()
			for _, r := range inputs {
				if r.TraceID != first.TraceID || view.Inputs[r.InputID].State != "consumed" {
					t.Errorf("input %s trace=%s state=%s", r.ActualKind, r.TraceID, view.Inputs[r.InputID].State)
				}
			}
			requests := capture.requests()
			if fake.Calls() != len(wantTexts) || len(requests) != len(wantTexts) {
				t.Fatalf("model calls=%d requests=%d want=%d", fake.Calls(), len(requests), len(wantTexts))
			}
			for i, req := range requests {
				var texts []string
				for _, msg := range req {
					if msg.Role != schema.AgenticRoleTypeUser {
						continue
					}
					for _, block := range msg.ContentBlocks {
						if block.UserInputText != nil {
							texts = append(texts, block.UserInputText.Text)
						}
					}
				}
				if !reflect.DeepEqual(texts, wantTexts[:i+1]) {
					t.Errorf("request %d user texts=%v want=%v", i, texts, wantTexts[:i+1])
				}
			}
			trace := view.Traces[first.TraceID]
			if len(view.Traces) != 1 || trace.InvocationID != before.InvocationID || trace.Generation != before.Generation || trace.Usage.LogicalModelCalls != len(wantTexts) || trace.Usage.TransportRequests != len(wantTexts) {
				t.Fatalf("continuation changed trace identity or budget: %+v", trace)
			}
			if len(view.Turns) != len(wantTexts) {
				t.Fatalf("turn count=%d", len(view.Turns))
			}
			for _, turn := range view.Turns {
				if !turn.Ended || turn.TraceID != first.TraceID || turn.InvocationID != before.InvocationID {
					t.Errorf("unfinished or unrelated turn: %+v", turn)
				}
			}
			settled := 0
			for _, ev := range view.Events {
				if ev.Type == "trace.settled" && ev.Scope.TraceID == first.TraceID {
					settled++
				}
			}
			if settled != 1 {
				t.Fatalf("settled events=%d", settled)
			}
		})
	}
}

func TestRuntimeNoToolSteeringPreservesBudgetLimit(t *testing.T) {
	for _, limit := range []struct {
		name   string
		limits config.Limits
	}{{"logical", config.Limits{TraceLogicalModelCalls: 1}}, {"transport", config.Limits{TraceTransportRequests: 1}}} {
		for _, kind := range []string{"steering", "follow_up"} {
			t.Run(limit.name+"/"+kind, func(t *testing.T) {
				gate := make(chan struct{})
				fake := testkit.NewFake(testkit.Step{Gate: gate, Text: "final"})
				model := &signaledModel{AgenticModel: fake, entered: make(chan struct{}, 2)}
				s, err := sessions.CreateAgentSession(context.Background(), sessions.Options{Workspace: t.TempDir(), StateRoot: "memory", Profile: sessions.ProfileMemory, Model: model, Limits: limit.limits})
				if err != nil {
					t.Fatal(err)
				}
				defer closeSession(t, s)
				first := submit(t, s)
				select {
				case <-model.entered:
				case <-time.After(3 * time.Second):
					t.Fatal("first model did not start")
				}
				directed, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: kind, TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"cannot exceed budget"}`)})
				if err != nil {
					t.Fatal(err)
				}
				close(gate)
				waitState(t, s, first.TraceID, "failed")
				snapshot, err := s.Snapshot(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				trace := snapshot.Traces[first.TraceID]
				if fake.Calls() != 1 || trace.Usage.LogicalModelCalls != 1 || trace.Usage.TransportRequests != 1 || snapshot.Inputs[directed.InputID].State != "undelivered" {
					t.Fatalf("continuation exceeded budget or consumed input: calls=%d usage=%+v input=%s", fake.Calls(), trace.Usage, snapshot.Inputs[directed.InputID].State)
				}
			})
		}
	}
}

func TestRuntimeCancelDoesNotConsumePendingSteering(t *testing.T) {
	gate := make(chan struct{})
	fake := testkit.NewFake(testkit.Step{Gate: gate, Text: "cancelled"})
	model := &signaledModel{AgenticModel: fake, entered: make(chan struct{}, 2)}
	s, manager := runtimeSession(t, model)
	first := submit(t, s)
	select {
	case <-model.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first model did not start")
	}
	steering, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: "steering", TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"pending"}`)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Cancel(ctx, first.TraceID); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, first.TraceID); err != nil {
		t.Fatal(err)
	}
	view := manager.View()
	if fake.Calls() != 1 || view.Traces[first.TraceID].State != "cancelled" || view.Inputs[steering.InputID].State != "undelivered" {
		t.Fatalf("cancel continued or consumed input: calls=%d trace=%s input=%s", fake.Calls(), view.Traces[first.TraceID].State, view.Inputs[steering.InputID].State)
	}
	settled := 0
	for _, ev := range view.Events {
		if ev.Type == "trace.settled" && ev.Scope.TraceID == first.TraceID {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("repeated cancel produced %d settled events", settled)
	}
}

func TestRuntimeDirectedInputAfterFinalIsRejected(t *testing.T) {
	fake := testkit.NewFake(testkit.Step{Text: "done"})
	s, manager := runtimeSession(t, fake)
	r := submit(t, s)
	waitState(t, s, r.TraceID, "completed")
	before := manager.View()
	for _, kind := range []string{"steering", "follow_up"} {
		_, err := s.SubmitInput(context.Background(), agent.InputCommand{Kind: kind, TargetTraceID: r.TraceID, Content: json.RawMessage(`{"text":"too late"}`)})
		pe, ok := product.AsError(err)
		if !ok || pe.Code != product.CodeStateConflict {
			t.Fatalf("%s error=%v", kind, err)
		}
	}
	if fake.Calls() != 1 || !reflect.DeepEqual(before, manager.View()) {
		t.Fatal("late input changed completed state or started another model call")
	}
}
