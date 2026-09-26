package eino

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/testkit"
)

// This test-only shape reads the pinned framework's gob envelope. It is not a
// product compatibility decoder: Step 12 must keep that versioned in the adapter.
type p2CheckpointEnvelope struct {
	RunnerCheckpoint []byte
	HasRunnerState   bool
	UnhandledItems   []agent.InputRef
	CanceledItems    []agent.InputRef
}

func p2ReadEnvelope(t *testing.T, store *memoryStore, id string) p2CheckpointEnvelope {
	t.Helper()
	data, ok, err := store.Get(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("checkpoint missing: exists=%v err=%v", ok, err)
	}
	var cp p2CheckpointEnvelope
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestP2FrameworkStopModes(t *testing.T) {
	for _, mode := range []string{"context", "graceful", "immediate"} {
		t.Run(mode, func(t *testing.T) {
			h := newCheckpointHarness(true)
			loop, cancel := h.startLoop(t, false, "")
			if ok, _ := loop.Push(checkpointPrompt); !ok {
				t.Fatal("Push rejected")
			}
			h.waitSignal(t, loop, cancel, h.model.started, "model did not start")
			switch mode {
			case "context":
				cancel()
			case "graceful":
				loop.Stop(adk.WithGraceful())
			case "immediate":
				loop.Stop(adk.WithImmediate())
			}
			exit := h.waitLoop(t, loop, cancel)
			if exit.CheckpointErr != nil {
				t.Fatal(exit.CheckpointErr)
			}
			if h.tool.total() != 0 {
				t.Fatalf("stop started a tool: %d", h.tool.total())
			}
			if h.genInput.Load() != 1 || h.genResume.Load() != 0 {
				t.Fatal("unexpected input lifecycle")
			}
			if mode == "context" {
				if !errors.Is(exit.ExitReason, context.Canceled) {
					t.Fatalf("exit=%v", exit.ExitReason)
				}
				if exit.CheckpointAttempted {
					t.Fatal("parent cancellation unexpectedly guaranteed a checkpoint")
				}
				return
			}
			var cancelled *adk.CancelError
			if !errors.As(exit.ExitReason, &cancelled) || !exit.CheckpointAttempted {
				t.Fatalf("exit=%v attempted=%v", exit.ExitReason, exit.CheckpointAttempted)
			}
			cp := p2ReadEnvelope(t, h.store, h.cpID)
			assertInterruptedRef(t, cp.CanceledItems, checkpointPrompt)
			if mode == "graceful" && (!cp.HasRunnerState || len(cp.RunnerCheckpoint) == 0) {
				t.Fatal("graceful safe point lost runner state")
			}
			// Immediate Stop may interrupt before the model returns a framework
			// signal. The presence of an outer blob alone must not authorize resume.
			if cp.HasRunnerState != (len(cp.RunnerCheckpoint) != 0) {
				t.Fatal("inconsistent runner envelope")
			}
		})
	}
}

func TestP2FrameworkCheckpointWithoutRunnerUsesGenInput(t *testing.T) {
	h := newCheckpointHarness(true)
	// This is a valid between-turns checkpoint, not an interrupted runner.
	var blob bytes.Buffer
	if err := gob.NewEncoder(&blob).Encode(p2CheckpointEnvelope{UnhandledItems: []agent.InputRef{checkpointPrompt}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Set(t.Context(), h.cpID, blob.Bytes()); err != nil {
		t.Fatal(err)
	}
	// The harness deliberately rejects GenInput on a resume. This proves why
	// product Resume must check runner state before starting the loop.
	loop, cancel := h.startLoop(t, true, "turn-1")
	exit := h.waitLoop(t, loop, cancel)
	if exit.ExitReason == nil {
		t.Fatal("runner-less checkpoint unexpectedly resumed")
	}
	if h.genResume.Load() != 0 || h.genInput.Load() != 1 {
		t.Fatalf("GenResume=%d GenInput=%d", h.genResume.Load(), h.genInput.Load())
	}
	if h.fake.Calls() != 0 || h.tool.total() != 0 {
		t.Fatal("runner-less resume repeated an action")
	}
}

type p2DeletingStore struct {
	memoryStore
	deleted []string // inspected only after Wait
}

func (s *p2DeletingStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, key)
	delete(s.m, key)
	return nil
}

func TestP2FrameworkCleanExitDeletesLoadedAlias(t *testing.T) {
	s := &p2DeletingStore{}
	var blob bytes.Buffer
	if err := gob.NewEncoder(&blob).Encode(p2CheckpointEnvelope{UnhandledItems: []agent.InputRef{checkpointPrompt}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(t.Context(), "between", blob.Bytes()); err != nil {
		t.Fatal(err)
	}
	h := newCheckpointHarness(true)
	var inputs int
	loop := adk.NewTurnLoop(adk.TurnLoopConfig[agent.InputRef, *schema.AgenticMessage]{
		Store: s, CheckpointID: "between",
		PrepareAgent: func(ctx context.Context, _ *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], _ []agent.InputRef) (adk.TypedAgent[*schema.AgenticMessage], error) {
			return NewAgent(ctx, Deps{Model: testkit.NewFake(testkit.Step{Text: "done"}), Sink: h.sink, Scope: h.scope})
		},
		OnAgentEvents: func(_ context.Context, tc *adk.TurnContext[agent.InputRef, *schema.AgenticMessage], events *adk.AsyncIterator[*adk.TypedAgentEvent[*schema.AgenticMessage]]) error {
			err := drainCancelAware(events)
			tc.Loop.Stop()
			return err
		},
		GenInput: func(_ context.Context, loop *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], items []agent.InputRef) (*adk.GenInputResult[agent.InputRef, *schema.AgenticMessage], error) {
			inputs++
			if err := inputRefIdentity(items, checkpointPrompt); err != nil {
				return nil, err
			}
			return &adk.GenInputResult[agent.InputRef, *schema.AgenticMessage]{
				Input:    &adk.TypedAgentInput[*schema.AgenticMessage]{Messages: []*schema.AgenticMessage{schema.UserAgenticMessage(items[0].InputID)}},
				Consumed: items,
			}, nil
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { h.recycle(t, loop, cancel, nil) })
	loop.Run(ctx)
	exit := h.waitLoop(t, loop, cancel)
	if exit.ExitReason != nil || exit.CheckpointErr != nil || exit.CheckpointAttempted {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	if inputs != 1 || len(s.deleted) != 1 || s.deleted[0] != "between" {
		t.Fatalf("inputs=%d deleted=%v", inputs, s.deleted)
	}
	if _, ok, _ := s.Get(t.Context(), "between"); ok {
		t.Fatal("alias not deleted")
	}
}
