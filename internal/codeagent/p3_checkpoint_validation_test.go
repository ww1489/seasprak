package codeagent

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

// The fault is injected only into a blob produced by actual Pause. No journal
// state is fabricated. Partial native shapes are test-only, never a product codec.
type p3SemanticCheckpointStore struct {
	store.Store
	store.CheckpointBlobs
	missing string
	puts    atomic.Int32
}

type p3SemanticCheckpointEnvelope struct {
	RunnerCheckpoint []byte
	HasRunnerState   bool
	UnhandledItems   []agent.InputRef
	CanceledItems    []agent.InputRef
}

type p3SemanticNativeCheckpoint struct {
	RunCtx *struct {
		Session *struct{ Values map[string]any }
	}
	Info *adk.InterruptInfo
}

func (s *p3SemanticCheckpointStore) Put(ctx context.Context, id string, data []byte) (store.BlobRef, error) {
	var envelope p3SemanticCheckpointEnvelope
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&envelope); err != nil {
		return store.BlobRef{}, err
	}
	if !envelope.HasRunnerState || len(envelope.RunnerCheckpoint) == 0 || len(envelope.CanceledItems) != 1 {
		return store.BlobRef{}, product.NewError(product.CodeStorageUnavailable, "synthetic fault requires an actual interrupted checkpoint")
	}
	native := p3SemanticNativeCheckpoint{Info: &adk.InterruptInfo{}}
	if s.missing != "RunCtx" {
		native.RunCtx = &struct {
			Session *struct{ Values map[string]any }
		}{}
	}
	if s.missing == "interrupt_state" {
		native.RunCtx.Session = &struct{ Values map[string]any }{Values: map[string]any{}}
	}
	var out bytes.Buffer
	if err := gob.NewEncoder(&out).Encode(native); err != nil {
		return store.BlobRef{}, err
	}
	envelope.RunnerCheckpoint = bytes.Clone(out.Bytes())
	out.Reset()
	if err := gob.NewEncoder(&out).Encode(envelope); err != nil {
		return store.BlobRef{}, err
	}
	s.puts.Add(1)
	return s.CheckpointBlobs.Put(ctx, id, out.Bytes())
}

func TestP3CheckpointValidationPauseNeverPublishesUnsafeNativeState(t *testing.T) {
	for _, missing := range []string{"RunCtx", "Session", "interrupt_state"} {
		t.Run(missing, func(t *testing.T) {
			id := agent.MustID()
			backend, err := memory.Open(id, store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			faults := &p3SemanticCheckpointStore{Store: backend, CheckpointBlobs: backend, missing: missing}
			manager, err := state.NewManager(faults, id)
			if err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var effects atomic.Int32
			model := versionedPauseModel{testkit.NewFake(testkit.Step{Gate: release, ToolCalls: []schema.FunctionToolCall{{CallID: "original-provider", Name: "work", Arguments: `{}`}}}, testkit.Step{Text: "must not continue"})}
			opts := Options{SessionID: id, Profile: ProfileMemory, Store: faults, Model: model, Workspace: t.TempDir(), GenerationFingerprint: "p3-semantic-checkpoint-v1",
				Tools: []tools.Definition{{Name: "work", Version: "1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "none"}, Run: func(context.Context, json.RawMessage) (string, error) {
					effects.Add(1)
					return "must not execute", nil
				}}},
			}
			if _, err := alignTools(&opts); err != nil {
				t.Fatal(err)
			}
			s, err := Start(opts, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close(context.Background()) }()
			input, err := s.SubmitInput(t.Context(), agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"checkpoint validation"}`)})
			if err != nil {
				t.Fatal(err)
			}
			waitResumeCondition(t, func() bool { return model.Calls() == 1 })
			type pauseResult struct {
				receipt state.OperationReceipt
				err     error
			}
			done := make(chan pauseResult, 1)
			go func() {
				receipt, err := s.Pause(t.Context(), input.TraceID)
				done <- pauseResult{receipt, err}
			}()
			waitResumeCondition(t, func() bool { return len(manager.View().Operations) == 1 })
			if err := s.rt.do(t.Context(), func(*runtime) error { return nil }); err != nil {
				t.Fatal(err)
			}
			close(release)
			var result pauseResult
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("actual Pause did not exit")
			}
			pe, ok := product.AsError(result.err)
			if !ok || pe.Code != product.CodeStateConflict {
				t.Fatalf("Pause published success for unsafe %s native state: %v", missing, result.err)
			}
			view := manager.View()
			tr := view.Traces[input.TraceID]
			op := view.Operations[result.receipt.OperationID]
			if faults.puts.Load() != 1 || len(view.Checkpoints) != 0 || tr.State != "failed" || !tr.ExecutionStopped || !tr.Settled || op.State != "failed" || model.Calls() != 1 || effects.Load() != 0 || tr.Usage.LogicalModelCalls != 1 || tr.Usage.TransportRequests != 1 || tr.Usage.ToolExecutions != 0 {
				t.Fatalf("unsafe checkpoint retained: trace=%+v checkpoints=%d operation=%+v puts=%d models=%d effects=%d", tr, len(view.Checkpoints), op, faults.puts.Load(), model.Calls(), effects.Load())
			}
			snapshot, err := s.Snapshot(t.Context())
			if err != nil || snapshot.Resume[input.TraceID].CanResume || snapshot.Resume[input.TraceID].Code != product.CodeIncompatibleResume {
				t.Fatalf("unsafe checkpoint advertised resumable: %v %v", snapshot.Resume[input.TraceID], err)
			}
			_, err = s.Resume(t.Context(), ResumeCommand{TraceID: input.TraceID, ExpectedRevision: view.LastSeq})
			pe, ok = product.AsError(err)
			if !ok || pe.Code != product.CodeIncompatibleResume || !reflect.DeepEqual(manager.View(), view) || model.Calls() != 1 || effects.Load() != 0 {
				t.Fatal("rejected Resume modified state or started work")
			}
			loaded, err := state.NewManager(backend, id)
			if err != nil || !reflect.DeepEqual(loaded.View(), view) || model.Calls() != 1 || effects.Load() != 0 {
				t.Fatalf("load-only fabricated a checkpoint or execution: %v", err)
			}
		})
	}
}
