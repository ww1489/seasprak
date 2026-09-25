package sessions_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/testkit"
)

func TestRuntimePausedReopenRejectsNewWorkWithoutChangingFacts(t *testing.T) {
	ctx := context.Background()
	model := &controlledModel{entered: make(chan context.Context, 1), gate: make(chan struct{})}
	opts := sessions.Options{SessionID: "paused-work", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sessions.ProfileMemory, Model: model}
	s, err := sessions.CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeSession(t, s) })
	original := agent.InputCommand{Kind: "prompt", IdempotencyKey: "original", Content: json.RawMessage(`{"text":"first"}`)}
	first, err := s.SubmitInput(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	awaitStart(t, model)
	queued := submit(t, s)
	closeSession(t, s)

	fake := testkit.NewFake(testkit.Step{Text: "must not run"})
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
	if before.Traces[first.TraceID].State != "paused" || before.ActiveTrace != first.TraceID || !before.Traces[queued.TraceID].Hold {
		t.Fatal("reopen did not preserve paused execution and hold the queued task")
	}
	replayed, err := reopened.SubmitInput(ctx, original)
	if err != nil || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("original receipt replay=%+v err=%v", replayed, err)
	}
	conflicting := original
	conflicting.Content = json.RawMessage(`{"text":"changed"}`)
	_, err = reopened.SubmitInput(ctx, conflicting)
	if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeIdempotencyConflict {
		t.Fatalf("idempotency conflict priority lost: %v", err)
	}
	for _, cmd := range []agent.InputCommand{
		{Kind: "prompt"},
		{Kind: "chat"},
		{Kind: "steering", TargetTraceID: first.TraceID},
		{Kind: "follow_up", TargetTraceID: first.TraceID},
		{Kind: "follow_up", TargetTraceID: queued.TraceID},
	} {
		cmd.Content = json.RawMessage(`{"text":"new work"}`)
		_, err := reopened.SubmitInput(ctx, cmd)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
			t.Errorf("%s should report recovery blocked, got %v", cmd.Kind, err)
		}
	}
	if err := reopened.ContinueQueue(ctx, queued.TraceID); err == nil {
		t.Error("continue queue silently accepted blocked work")
	} else if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeReconciliationRequired {
		t.Errorf("continue error=%v", err)
	}
	for _, cmd := range []agent.InputCommand{
		{Kind: "steering", TargetTraceID: "missing", Content: json.RawMessage(`{"text":"bad target"}`)},
		{Kind: "follow_up", TargetTraceID: "missing", Content: json.RawMessage(`{"text":"bad target"}`)},
	} {
		_, err := reopened.SubmitInput(ctx, cmd)
		if pe, ok := product.AsError(err); !ok || pe.Code != product.CodeNotFound {
			t.Errorf("missing target error=%v", err)
		}
	}
	after, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls() != 0 || !reflect.DeepEqual(before, after) {
		t.Errorf("blocked requests changed facts or invoked model: calls=%d cursor=%d->%d inputs=%d->%d hold=%v", fake.Calls(), before.Cursor, after.Cursor, len(before.Inputs), len(after.Inputs), after.Traces[queued.TraceID].Hold)
	}
}
