package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/state"
	"github.com/ww1489/seasprak/internal/sessions/store"
	"github.com/ww1489/seasprak/internal/sessions/store/memory"
	"github.com/ww1489/seasprak/internal/testkit"
)

type stoppedCommitFailure struct {
	store.Store
}

func (s stoppedCommitFailure) Append(ctx context.Context, id string, expected store.ExpectedCommit, commit store.Commit) (store.CommitReceipt, error) {
	for _, record := range commit.ControlRecords {
		if record.Type != "trace" {
			continue
		}
		var tr state.TraceState
		if err := json.Unmarshal(record.Payload, &tr); err != nil {
			return store.CommitReceipt{}, err
		}
		if tr.ExecutionStopped {
			return store.CommitReceipt{}, errors.New("injected stopped confirmation failure")
		}
	}
	return s.Store.Append(ctx, id, expected, commit)
}

func TestRuntimeStoppedConfirmationFailureCannotSettleCancel(t *testing.T) {
	ctx := context.Background()
	backend, err := memory.Open("stop-confirmation-failure", store.Header{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := stoppedCommitFailure{Store: backend}
	manager, err := state.NewManager(wrapped, "stop-confirmation-failure")
	if err != nil {
		t.Fatal(err)
	}
	model := &controlledModel{entered: make(chan context.Context, 1), gate: make(chan struct{})}
	s, err := sessions.Start(sessions.Options{SessionID: "stop-confirmation-failure", Profile: sessions.ProfileMemory, Model: model, Store: wrapped}, manager, "gen")
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, s)
	r := submit(t, s)
	awaitStart(t, model)
	err = s.Cancel(ctx, r.TraceID)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeStorageUnavailable {
		t.Fatalf("cancel reported success despite failed stop confirmation: %v", err)
	}
	view := manager.View()
	tr := view.Traces[r.TraceID]
	if tr.State != "cancelling" || tr.Settled || tr.ExecutionStopped {
		t.Fatalf("failed stopped commit fabricated terminal proof: %+v", tr)
	}
	for _, ev := range view.Events {
		if ev.Type == "trace.settled" {
			t.Fatal("failed stop confirmation published settled")
		}
	}
	replayed, err := state.NewManager(backend, "stop-confirmation-failure")
	if err != nil || !reflect.DeepEqual(view, replayed.View()) {
		t.Fatalf("failed stop confirmation leaked into the journal: %v", err)
	}
}

func TestRuntimeCancelPausedModelWithoutCheckpoint(t *testing.T) {
	ctx := context.Background()
	model := &controlledModel{entered: make(chan context.Context, 1), gate: make(chan struct{})}
	opts := sessions.Options{SessionID: "cancel-paused-model", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: model}
	s, err := sessions.CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSession(t, s) })
	first := submit(t, s)
	awaitStart(t, model)
	queued := submit(t, s)
	follow, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "follow_up", TargetTraceID: first.TraceID, Content: json.RawMessage(`{"text":"follow"}`)})
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)

	fake := testkit.NewFake(testkit.Step{Text: "queued answer"})
	opts.Model = fake
	reopened, err := sessions.OpenAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(t, reopened)
	before, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Cancel(ctx, first.TraceID); err != nil {
		t.Fatalf("stopped model-only trace cannot be cancelled: %v", err)
	}
	after, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	old, current := before.Traces[first.TraceID], after.Traces[first.TraceID]
	if current.State != "cancelled" || !current.Settled || after.ActiveTrace != "" {
		t.Fatalf("cancel did not settle the paused trace: %+v", current)
	}
	if current.InvocationID != old.InvocationID || current.Generation != old.Generation || current.Usage != old.Usage {
		t.Fatal("cancel replaced identity or reset the budget")
	}
	if !after.Traces[queued.TraceID].Hold || after.Inputs[follow.InputID].State != "undelivered" || fake.Calls() != 0 {
		t.Fatal("cancel ran queued work or lost pending input disposition")
	}
	for _, turn := range after.Turns {
		if turn.TraceID == first.TraceID && !turn.Ended {
			t.Fatal("cancel left a turn unfinished")
		}
	}
	if err := reopened.Cancel(ctx, first.TraceID); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Snapshot(ctx)
	if err != nil || !reflect.DeepEqual(after, again) {
		t.Fatalf("duplicate cancel changed committed facts: %v", err)
	}
	if err := reopened.ContinueQueue(ctx, queued.TraceID); err != nil {
		t.Fatal(err)
	}
	waitState(t, reopened, queued.TraceID, "completed")
	if fake.Calls() != 1 {
		t.Fatalf("queued model calls=%d, want one explicit continuation", fake.Calls())
	}
}

func TestRuntimeCancelPausedCrashPrefix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claimed     bool
		stopped     bool
		observation *agent.ToolObservation
		blocked     bool
	}{
		{name: "unclaimed"},
		{name: "known_result", claimed: true, observation: &agent.ToolObservation{Status: "succeeded", Content: "saved", SideEffect: "none", Executed: true}},
		{name: "claimed_no_result", claimed: true, blocked: true},
		{name: "stopped_claim_no_result", claimed: true, stopped: true},
		{name: "unknown_result", claimed: true, observation: &agent.ToolObservation{Status: "outcome_unknown", Content: "uncertain", SideEffect: "unknown", Executed: true}, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			backend, err := memory.Open("paused-prefix", store.Header{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			manager, err := state.NewManager(backend, "paused-prefix")
			if err != nil {
				t.Fatal(err)
			}
			r, err := manager.Accept(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"original"}`)}, agent.TargetAgent{Name: "main", Version: "main-v1", Generation: "gen"})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.SetTraceState(ctx, r.TraceID, "running", false); err != nil {
				t.Fatal(err)
			}
			if err := manager.Consume(ctx, r.InputID); err != nil {
				t.Fatal(err)
			}
			scope := agent.ExecutionScope{SessionID: "paused-prefix", TraceID: r.TraceID, InvocationID: manager.View().Traces[r.TraceID].InvocationID, TurnID: "original-turn", ExecutionID: "original-execution", Generation: "gen"}
			if err := manager.SaveTurn(ctx, agent.TurnRecord{ID: scope.TurnID, TraceID: r.TraceID, InvocationID: scope.InvocationID}); err != nil {
				t.Fatal(err)
			}
			call := agent.ToolRecord{Scope: scope, Call: agent.FrozenCall{CallID: "original-call", ProviderCallID: "provider-call", Name: "add", Arguments: `{}`, Generation: "gen", Hash: "original-hash"}}
			msg := agent.AgentMessage{ID: "assistant", Kind: agent.KindAssistant, Status: agent.StatusComplete, Source: agent.SourceRef{Kind: agent.SourceModel}, Scope: agent.MessageScope{SessionID: scope.SessionID, TraceID: scope.TraceID, TurnID: scope.TurnID, InvocationID: scope.InvocationID}, Standard: &schema.AgenticMessage{Role: schema.AgenticRoleTypeAssistant, ContentBlocks: []*schema.ContentBlock{schema.NewContentBlock(&schema.FunctionToolCall{CallID: call.Call.ProviderCallID, Name: call.Call.Name, Arguments: call.Call.Arguments})}}}
			if err := manager.SaveAssistant(ctx, msg, []agent.ToolRecord{call}); err != nil {
				t.Fatal(err)
			}
			call.Claimed, call.Observation = tc.claimed, tc.observation
			if tc.claimed || tc.observation != nil {
				if err := manager.SaveCall(ctx, call); err != nil {
					t.Fatal(err)
				}
			}
			if tc.stopped {
				// Model the durable prefix after the coordinator has observed exit,
				// but before interrupted-turn cleanup has saved missing results.
				if err := manager.ConfirmExecutionStopped(ctx, r.TraceID); err != nil {
					t.Fatal(err)
				}
			}
			manager, err = state.NewManager(backend, "paused-prefix")
			if err != nil {
				t.Fatal(err)
			}
			fake := testkit.NewFake(testkit.Step{Text: "must not run"})
			s, err := sessions.Start(sessions.Options{SessionID: "paused-prefix", Profile: sessions.ProfileMemory, Model: fake, Store: backend}, manager, "gen")
			if err != nil {
				t.Fatal(err)
			}
			defer closeSession(t, s)
			before := manager.View()
			err = s.Cancel(ctx, r.TraceID)
			if tc.blocked {
				if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
					t.Fatalf("unproven stopped execution must remain blocked: %v", err)
				}
				if !reflect.DeepEqual(before, manager.View()) {
					t.Fatal("rejected cancel changed crash facts")
				}
			} else {
				if err != nil {
					t.Fatalf("safe interrupted trace cannot be cancelled: %v", err)
				}
				after := manager.View()
				if after.Traces[r.TraceID].ExecutionStopped != before.Traces[r.TraceID].ExecutionStopped {
					t.Fatal("safe cancellation fabricated execution-exit evidence")
				}
				if after.Traces[r.TraceID].State != "cancelled" || !after.Turns[scope.TurnID].Ended {
					t.Fatal("safe cancel did not finish the original turn")
				}
				got := after.Calls[call.Call.CallID]
				if got.Call != call.Call || got.Scope != call.Scope || got.Claimed != call.Claimed {
					t.Fatal("cancel changed accepted call identity or occupancy")
				}
				if tc.observation != nil && !reflect.DeepEqual(got.Observation, tc.observation) {
					t.Fatal("cancel replaced the saved result")
				}
				if tc.observation == nil {
					status, effect := "skipped", "none"
					if tc.claimed {
						status, effect = "outcome_unknown", "unknown"
					}
					if got.Observation == nil || got.Observation.Status != status || got.Observation.Executed != tc.claimed || got.Observation.SideEffect != effect {
						t.Fatalf("interrupted call cleanup changed execution facts: %+v", got.Observation)
					}
					if tc.claimed {
						if !after.HasUnresolvedEffects() {
							t.Fatal("stopped execution lost the unresolved-effect restriction")
						}
						_, err := s.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"must remain blocked"}`)})
						if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
							t.Fatalf("missing result after confirmed exit allowed new work: %v", err)
						}
					}
				}
				if err := s.Cancel(ctx, r.TraceID); err != nil {
					t.Fatal(err)
				}
				settled := 0
				for _, ev := range manager.View().Events {
					if ev.Type == "trace.settled" && ev.Scope.TraceID == r.TraceID {
						settled++
					}
				}
				if settled != 1 {
					t.Fatalf("settled events=%d", settled)
				}
			}
			if fake.Calls() != 0 {
				t.Fatal("cancel invoked the model")
			}
		})
	}
}

func TestRuntimeCancelWaitsForToolBeforeStoppedProof(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "blocking", Name: "add", Arguments: `{"n":1}`}}})
	s := openSession(t, fake, func(ctx context.Context, _ json.RawMessage) (string, error) {
		close(entered)
		<-release // Deliberately ignore cancellation until the executor really exits.
		return "", ctx.Err()
	})
	t.Cleanup(func() { closeSession(t, s) })
	r := submit(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Cancel(ctx, r.TraceID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel returned before tool exit: %v", err)
	}
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr := snap.Traces[r.TraceID]; tr.State != "cancelling" || tr.ExecutionStopped || tr.Settled {
		t.Fatalf("caller timeout fabricated stopped proof: %+v", tr)
	}
	released = true
	close(release)
	if err := s.Cancel(context.Background(), r.TraceID); err != nil {
		t.Fatal(err)
	}
	snap, err = s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tr := snap.Traces[r.TraceID]; tr.State != "cancelled" || !tr.ExecutionStopped || !tr.Settled {
		t.Fatalf("exit did not persist stopped proof: %+v", tr)
	}
	if fake.Calls() != 1 {
		t.Fatal("cancellation invoked another model")
	}
}

func TestRuntimeUnknownEffectStopsModelAndQueue(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "uncertain", Name: "add", Arguments: `{"n":1}`}}}, testkit.Step{Text: "must not continue"})
	s := openSession(t, fake, func(ctx context.Context, _ json.RawMessage) (string, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		return "", errors.New("effect could not be confirmed")
	})
	defer closeSession(t, s)
	first := submit(t, s)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	queued := submit(t, s)
	close(release)
	waitState(t, s, first.TraceID, "failed")
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Traces[queued.TraceID].Hold || fake.Calls() != 1 || calls.Load() != 1 {
		t.Fatalf("unknown result allowed continued execution: held=%v models=%d tools=%d", snap.Traces[queued.TraceID].Hold, fake.Calls(), calls.Load())
	}
}

func TestRuntimeCancelPausedStoppedToolRetainsUnknownEffect(t *testing.T) {
	ctx := context.Background()
	entered := make(chan struct{})
	var toolCalls atomic.Int32
	fake := testkit.NewFake(testkit.Step{ToolCalls: []schema.FunctionToolCall{{CallID: "original", Name: "add", Arguments: `{}`}}})
	opts := sessions.Options{SessionID: "cancel-paused-unknown", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: fake, Tools: []tools.Definition{{Name: "add", Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, _ json.RawMessage) (string, error) {
		toolCalls.Add(1)
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}}}}
	s, err := sessions.CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSession(t, s) })
	original := agent.InputCommand{Kind: "prompt", IdempotencyKey: "original", Content: json.RawMessage(`{"text":"run"}`)}
	first, err := s.SubmitInput(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	queued := submit(t, s)
	closeSession(t, s)
	fresh := testkit.NewFake(testkit.Step{Text: "must not run"})
	opts.Model = fresh
	for attempt := 0; attempt < 2; attempt++ {
		reopened, err := sessions.OpenAgentSession(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeSession(t, reopened) })
		before, err := reopened.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := reopened.Cancel(ctx, first.TraceID); err != nil {
			t.Fatalf("confirmed stopped execution could not be ended: %v", err)
		}
		after, err := reopened.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.Traces[first.TraceID].State != "cancelled" || !after.Traces[first.TraceID].Settled || !reflect.DeepEqual(before.Calls, after.Calls) || before.Traces[first.TraceID].Usage != after.Traces[first.TraceID].Usage {
			t.Fatal("safe end lost unknown facts or reset usage")
		}
		if len(after.Calls) != 1 {
			t.Fatal("accepted call missing")
		}
		for _, call := range after.Calls {
			if !call.Claimed || call.Observation == nil || call.Observation.SideEffect != "unknown" {
				t.Fatal("cancel erased uncertain effect")
			}
		}
		replayed, err := reopened.SubmitInput(ctx, original)
		if err != nil || replayed != first {
			t.Fatalf("unknown effect blocked original receipt replay: %v", err)
		}
		_, err = reopened.SubmitInput(ctx, agent.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"new"}`)})
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
			t.Fatalf("unknown terminal effect allowed new work: %v", err)
		}
		if err := reopened.ContinueQueue(ctx, queued.TraceID); err == nil {
			t.Fatal("unknown terminal effect allowed queue continuation")
		} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
			t.Fatal(err)
		}
		if err := reopened.Cancel(ctx, first.TraceID); err != nil {
			t.Fatal(err)
		}
		unchanged, err := reopened.Snapshot(ctx)
		if err != nil || !reflect.DeepEqual(after, unchanged) {
			t.Fatalf("blocked commands or duplicate cancel changed facts: %v", err)
		}
		closeSession(t, reopened)
	}
	if fake.Calls() != 1 || fresh.Calls() != 0 || toolCalls.Load() != 1 {
		t.Fatalf("unexpected replay: initial models=%d new models=%d tools=%d", fake.Calls(), fresh.Calls(), toolCalls.Load())
	}
}
