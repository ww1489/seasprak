package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestWorkflowFrozenRecoveryReopenAndResourceRecheck(t *testing.T) {
	for _, scenario := range []string{"reopen", "resource_changed"} {
		t.Run(scenario, func(t *testing.T) {
			var effects, preparations, resolutions, hooks atomic.Int32
			var changed atomic.Bool
			opts := testOptions(t, toolOnly(), nil, &effects)
			opts.Tools[0].Execution.RequestedGrantRef = "once"
			opts.Tools[0].PrepareArguments = []func(context.Context, json.RawMessage) (json.RawMessage, error){func(context.Context, json.RawMessage) (json.RawMessage, error) {
				if preparations.Add(1) == 1 {
					return json.RawMessage(`{"q":"A"}`), nil
				}
				return json.RawMessage(`{"q":"B"}`), nil
			}}
			opts.Tools[0].ResolveExecution = func(_ context.Context, _ json.RawMessage, d tools.ExecutionDescription) (tools.ExecutionDescription, error) {
				resolutions.Add(1)
				d.Resources = []agent.ExecutionResource{{Identity: "business-record", ExpectedVersion: "A"}}
				return d, nil
			}
			opts.Tools[0].BeforeCall = []func(context.Context, agent.FrozenExecution) error{func(_ context.Context, f agent.FrozenExecution) error {
				hooks.Add(1)
				if string(f.FinalArguments) != `{"q":"A"}` || len(f.Resources) != 1 || f.Resources[0].ExpectedVersion != "A" {
					t.Error("hook did not receive original descriptor")
				}
				if changed.Load() {
					return product.NewError(product.CodePermissionDenied, "business resource version changed")
				}
				// A hook owns its input; mutation never changes the frozen source.
				f.FinalArguments[0] = 'x'
				f.Resources[0].ExpectedVersion = "mutated-hook-copy"
				return nil
			}}
			opts.Tools[0].Run = func(_ context.Context, raw json.RawMessage) (string, error) {
				effects.Add(1)
				if string(raw) != `{"q":"A"}` {
					t.Error("recovery replaced frozen A")
				}
				return "original-A", nil
			}
			w := newWorkflow(t, opts)
			submit(t, w)
			waiting := waitStopped(t, w)
			if waiting.State != "paused" || effects.Load() != 0 {
				t.Fatal("original call did not wait")
			}
			var originalID string
			for _, n := range waiting.WorkflowNodes {
				originalID = n.ID
			}
			if scenario == "reopen" {
				if err := w.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				var err error
				w, err = OpenWorkflowAgent(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Close(context.Background())
				_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
				if err != nil {
					t.Fatal(err)
				}
				waiting = waitStopped(t, w)
				if waiting.State != "paused" || effects.Load() != 0 || preparations.Load() != 1 || resolutions.Load() != 1 {
					t.Fatal("reopen re-prepared or implicitly executed original call")
				}
			}
			var question WorkflowInteraction
			for _, question = range waiting.Interactions {
			}
			_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: waiting.Revision, Principal: "local"})
			if err != nil {
				t.Fatal(err)
			}
			changed.Store(scenario == "resource_changed")
			_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
			if err != nil {
				t.Fatal(err)
			}
			final := waitStopped(t, w)
			if preparations.Load() != 1 || resolutions.Load() != 1 || hooks.Load() < 2 || final.WorkflowNodes[originalID].ID != originalID {
				t.Fatal("frozen preparation, hooks or stable identity changed")
			}
			if scenario == "reopen" {
				if final.State != "completed" || effects.Load() != 1 {
					t.Fatalf("frozen reopen state=%s code=%s effects=%d", final.State, final.ErrorCode, effects.Load())
				}
			} else if final.State != "failed" || final.ErrorCode != product.CodePermissionDenied || effects.Load() != 0 || final.Usage.ToolExecutions != 0 {
				t.Fatalf("current resource recheck bypassed: %s %s effects=%d", final.State, final.ErrorCode, effects.Load())
			}
		})
	}
}

type mutableWorkflowProcess struct {
	workflowProcess
	changed atomic.Bool
}

func (p *mutableWorkflowProcess) ExecutionCapabilities(ctx context.Context) (agent.BackendCapabilities, error) {
	c, err := p.workflowProcess.ExecutionCapabilities(ctx)
	if p.changed.Load() {
		c.Version = "v2"
	}
	return c, err
}
func TestWorkflowFrozenRecoveryRejectsChangedBackend(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	process := &mutableWorkflowProcess{}
	opts.Tools[0].Run = nil
	opts.Tools[0].Execution = tools.ExecutionDescription{BackendID: "process-operations", Effect: "read", Argv: []string{"synthetic"}, RequestedGrantRef: "once"}
	opts.Operations.Process = process
	w := newWorkflow(t, opts)
	submit(t, w)
	waiting := waitStopped(t, w)
	var question WorkflowInteraction
	for _, question = range waiting.Interactions {
	}
	_, err := w.RespondInteraction(t.Context(), WorkflowInteractionResponse{InteractionID: question.ID, Decision: "allowed-once", ExpectedRevision: waiting.Revision, Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	process.changed.Store(true)
	_, err = w.Resume(t.Context(), WorkflowControlCommand{Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	final := waitStopped(t, w)
	if final.State != "failed" || final.ErrorCode != product.CodeResourceUnavailable || final.Usage.ToolExecutions != 0 || process.calls.Load() != 0 {
		t.Fatalf("changed backend executed: %s %s claims=%d effects=%d", final.State, final.ErrorCode, final.Usage.ToolExecutions, process.calls.Load())
	}
}
