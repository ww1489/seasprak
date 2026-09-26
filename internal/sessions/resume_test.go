package sessions

import (
	"context"
	"encoding/json"
	"reflect"
	goruntime "runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type changingResumeModel struct {
	*testkit.FakeModel
	changed atomic.Bool
}

func (m *changingResumeModel) Configuration() llm.ModelConfig {
	version := "model-config-v1"
	if m.changed.Load() {
		version = "model-config-v2"
	}
	return llm.ModelConfig{Version: version}
}
func (m *changingResumeModel) Generate(ctx context.Context, input []*schema.AgenticMessage, opts ...model.Option) (*schema.AgenticMessage, error) {
	message, err := m.FakeModel.Generate(ctx, input, opts...)
	m.changed.Store(true)
	return message, err
}

type explicitResumer interface {
	Resume(context.Context, ResumeCommand) (state.OperationReceipt, error)
}

type resumeFixture struct {
	s           *AgentSession
	manager     *state.Manager
	opts        Options
	input       agent.InputReceipt
	pause       state.OperationReceipt
	model       versionedPauseModel
	runs        *atomic.Int32
	toolEntered chan struct{}
	toolRelease chan struct{}
}

type resumeFixtureOptions struct {
	Disk             bool
	PendingFollow    bool
	Effect           string
	ToolInterface    string
	BlockResumedTool bool
	ChangingVersion  bool
	AdditionalTool   bool
	ToolInFollowUp   bool
}

func pausedResumeFixture(t *testing.T, afterTool bool, options ...resumeFixtureOptions) resumeFixture {
	t.Helper()
	settings := resumeFixtureOptions{Effect: "read"}
	if len(options) != 0 {
		settings = options[0]
		if settings.Effect == "" {
			settings.Effect = "read"
		}
	}
	id := agent.MustID()
	backend, err := memory.Open(id, store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := state.NewManager(backend, id)
	if err != nil {
		t.Fatal(err)
	}
	gate, entered := make(chan struct{}), make(chan struct{})
	toolEntered, toolRelease := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-toolRelease:
		default:
			close(toolRelease)
		}
	})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	runs := &atomic.Int32{}
	step := testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "provider", Name: "work", Arguments: `{}`}}}
	if !afterTool {
		step.Gate = gate
	}
	steps := []testkit.Step{step, {Text: "finished"}}
	if settings.AdditionalTool {
		steps = []testkit.Step{step, {ToolCalls: []schema.FunctionToolCall{{CallID: "next-provider", Name: "work", Arguments: `{}`}}}, {Text: "finished"}}
	}
	if settings.ToolInFollowUp {
		steps = []testkit.Step{step, {Text: "original finished"}, {ToolCalls: []schema.FunctionToolCall{{CallID: "follow-provider", Name: "work", Arguments: `{}`}}}, {Text: "follow finished"}}
	}
	model := versionedPauseModel{testkit.NewFake(steps...)}
	opts := Options{SessionID: id, Profile: ProfileMemory, Store: backend, Model: model,
		GenerationFingerprint: "trusted-resume-bundle-v1", Workspace: t.TempDir(),
		Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), ToolInterface: settings.ToolInterface, Execution: tools.ExecutionDescription{Effect: settings.Effect}, Run: func(context.Context, json.RawMessage) (string, error) {
			invocation := runs.Add(1)
			if afterTool && invocation == 1 {
				close(entered)
				<-gate
			}
			if settings.BlockResumedTool {
				close(toolEntered)
				<-toolRelease
			}
			return "saved result", nil
		}}},
	}
	if settings.ChangingVersion {
		opts.Model = &changingResumeModel{FakeModel: model.FakeModel}
	}
	if _, err := alignTools(&opts); err != nil {
		t.Fatal(err)
	}
	var s *AgentSession
	if settings.Disk {
		opts.StateRoot, opts.Store = t.TempDir(), nil
		s, err = CreateAgentSession(t.Context(), opts)
		if err == nil {
			manager = s.rt.manager
		}
	} else {
		s, err = Start(opts, manager, "gen")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	waitResumeCondition(t, func() bool {
		if afterTool {
			select {
			case <-entered:
				return true
			default:
				return false
			}
		}
		return model.Calls() == 1
	})
	if settings.PendingFollow {
		if _, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "follow_up", TargetTraceID: input.TraceID, Content: json.RawMessage(`{"text":"follow"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	type pauseResult struct {
		receipt state.OperationReceipt
		err     error
	}
	result := make(chan pauseResult, 1)
	go func() {
		receipt, err := s.Pause(context.Background(), input.TraceID)
		result <- pauseResult{receipt, err}
	}()
	waitResumeCondition(t, func() bool { return len(manager.View().Operations) == 1 })
	// The mailbox barrier also observes registration of the loop's stop request.
	if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(gate)
	select {
	case p := <-result:
		if p.err != nil {
			t.Fatal(p.err)
		}
		return resumeFixture{s: s, manager: manager, opts: opts, input: input, pause: p.receipt, model: model, runs: runs, toolEntered: toolEntered, toolRelease: toolRelease}
	case <-time.After(5 * time.Second):
		t.Fatal("pause did not complete")
	}
	return resumeFixture{}
}

func waitResumeCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for !condition() {
		select {
		case <-deadline:
			t.Fatal("resume test condition not reached")
		default:
			goruntime.Gosched()
		}
	}
}

func TestExplicitResumeKeepsOriginalToolIdentityAndBudget(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		afterTool  bool
	}{
		{"invokable/after-model", "invokable", false},
		{"invokable/after-tool", "invokable", true},
		{"enhanced-invokable/after-model", "enhanced-invokable", false},
		{"enhanced-invokable/after-tool", "enhanced-invokable", true},
	} {
		afterTool := tc.afterTool
		t.Run(tc.name, func(t *testing.T) {
			f := pausedResumeFixture(t, afterTool, resumeFixtureOptions{ToolInterface: tc.kind})
			before := f.manager.View()
			resumer, ok := any(f.s).(explicitResumer)
			if !ok {
				t.Fatal("session cannot explicitly resume its durable checkpoint")
			}
			receipt, err := resumer.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: before.LastSeq})
			if err != nil || receipt.OperationID == "" || receipt.State != "accepted" || receipt.Target != f.input.TraceID {
				t.Fatalf("resume receipt=%+v err=%v", receipt, err)
			}
			waitResumeCondition(t, func() bool { return terminal(f.manager.View().Traces[f.input.TraceID].State) })
			after := f.manager.View()
			tr := after.Traces[f.input.TraceID]
			if tr.State != "completed" || !tr.Settled || f.model.Calls() != 2 || f.runs.Load() != 1 || tr.InvocationID != before.Traces[tr.ID].InvocationID {
				t.Fatalf("trace=%+v model=%d runs=%d", tr, f.model.Calls(), f.runs.Load())
			}
			if tr.Usage.LogicalModelCalls != 2 || tr.Usage.TransportRequests != 2 || tr.Usage.ToolExecutions != 1 {
				t.Fatalf("restored budget=%+v", tr.Usage)
			}
			if len(after.Turns) != 2 || len(after.Calls) != 1 {
				t.Fatalf("turns=%d calls=%d", len(after.Turns), len(after.Calls))
			}
			for id, old := range before.Calls {
				call := after.Calls[id]
				if call.Scope != old.Scope || call.Call != old.Call || call.Observation == nil || call.Observation.Content != "saved result" {
					t.Fatalf("original call changed: before=%+v after=%+v", old, call)
				}
				if afterTool && !reflect.DeepEqual(old, call) {
					t.Fatal("completed call was modified during resume")
				}
				var newExecution string
				for _, attempt := range after.ModelAttempts {
					if attempt.Scope.TurnID != call.Scope.TurnID {
						newExecution = attempt.Scope.ExecutionID
					}
				}
				if newExecution == "" || newExecution == call.Scope.ExecutionID {
					t.Fatal("resume did not allocate a new execution identity")
				}
			}
			status, err := f.s.GetOperation(t.Context(), receipt.OperationID)
			if err != nil || status.State != "completed" {
				t.Fatalf("resume operation=%+v err=%v", status, err)
			}
			_, err = resumer.Resume(t.Context(), ResumeCommand{TraceID: f.input.TraceID, ExpectedRevision: after.LastSeq})
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeIncompatibleResume || f.model.Calls() != 2 || f.runs.Load() != 1 {
				t.Fatalf("terminal resume=%v model=%d runs=%d", err, f.model.Calls(), f.runs.Load())
			}
		})
	}
}
