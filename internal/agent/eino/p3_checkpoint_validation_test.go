package eino

import (
	"bytes"
	"encoding/gob"
	"reflect"
	"testing"

	"github.com/cloudwego/eino/adk"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

func p3EncodePausedEnvelope(t *testing.T, native []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := gob.NewEncoder(&out).Encode(p2CheckpointEnvelope{RunnerCheckpoint: native, HasRunnerState: true, CanceledItems: []agent.InputRef{checkpointPrompt}}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestP3CheckpointValidationRejectsUnsafeNativeState(t *testing.T) {
	for _, missing := range []string{"corrupt", "RunCtx", "Session", "interrupt_state"} {
		t.Run(missing, func(t *testing.T) {
			native := []byte("invalid synthetic native checkpoint")
			if missing != "corrupt" {
				shape := p3NativeCheckpointShape{Info: &adk.InterruptInfo{}}
				if missing != "RunCtx" {
					shape.RunCtx = &p3NativeRunContextShape{}
				}
				if missing == "interrupt_state" {
					shape.RunCtx.Session = &p3NativeSessionShape{Values: map[string]any{}}
				}
				var out bytes.Buffer
				if err := gob.NewEncoder(&out).Encode(shape); err != nil {
					t.Fatal(err)
				}
				native = out.Bytes()
			}
			p := newP3ChildProbe()
			before := p.counts()
			err := ValidatePausedCheckpoint(p3EncodePausedEnvelope(t, native), checkpointPrompt, "main")
			pe, ok := product.AsError(err)
			if !ok || pe.Code != product.CodeIncompatibleResume {
				t.Fatalf("unsafe %s native state accepted by the product checkpoint gate: %v", missing, err)
			}
			if !reflect.DeepEqual(p.counts(), before) {
				t.Fatal("checkpoint validation admitted model, tool or effect work")
			}
		})
	}
}

func TestP3CheckpointValidationPreservesActualPausedExecution(t *testing.T) {
	h := newCheckpointHarness(true)
	t.Cleanup(h.closeRelease)
	loop, cancel := h.startLoop(t, false, "")
	if ok, _ := loop.Push(checkpointPrompt); !ok {
		t.Fatal("Push rejected")
	}
	h.waitSignal(t, loop, cancel, h.model.started, "model did not enter")
	loop.Stop(adk.WithGraceful())
	exit := h.waitLoop(t, loop, cancel)
	assertCancelCheckpoint(t, exit, h.store, h.cpID)
	data, ok, err := h.store.Get(t.Context(), h.cpID)
	if err != nil || !ok {
		t.Fatal("actual paused checkpoint is missing")
	}
	beforeBudget := h.budget.Snapshot()
	beforePrepared, beforeFinished, beforeScopes := h.boundary.snapshot()
	if err := ValidatePausedCheckpoint(data, checkpointPrompt, "main"); err != nil {
		t.Fatalf("actual native checkpoint rejected: %v", err)
	}
	for _, name := range []string{"", "another-agent"} {
		err := ValidatePausedCheckpoint(data, checkpointPrompt, name)
		pe, ok := product.AsError(err)
		if !ok || pe.Code != product.CodeIncompatibleResume {
			t.Fatalf("checkpoint accepted without its original target: name=%q error=%v", name, err)
		}
	}
	afterPrepared, afterFinished, afterScopes := h.boundary.snapshot()
	if h.fake.Calls() != 1 || h.model.before.Load() != 1 || h.model.later.Load() != 0 || h.tool.total() != 0 || h.budget.Snapshot() != beforeBudget || !reflect.DeepEqual(beforePrepared, afterPrepared) || !reflect.DeepEqual(beforeFinished, afterFinished) || !reflect.DeepEqual(beforeScopes, afterScopes) {
		t.Fatal("checkpoint validation executed the original agent or changed its budget/boundary")
	}
	after, ok, err := h.store.Get(t.Context(), h.cpID)
	if err != nil || !ok || !bytes.Equal(data, after) {
		t.Fatal("checkpoint validation modified the original store")
	}
}
