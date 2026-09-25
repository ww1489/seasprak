package consumer_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/sdk"
)

type stoppedModel struct {
	entered chan struct{}
	calls   atomic.Int32
}

func (m *stoppedModel) Generate(ctx context.Context, _ []*schema.AgenticMessage, _ ...einomodel.Option) (*schema.AgenticMessage, error) {
	m.calls.Add(1)
	close(m.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *stoppedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	_, err := m.Generate(ctx, in, opts...)
	return nil, err
}

func TestConsumerCanCancelStoppedPausedTrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	model := &stoppedModel{entered: make(chan struct{})}
	opts := sdk.SessionOptions{SessionID: "consumer-paused", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: model}
	s, err := sdk.CreateAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	first, err := s.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"stop and close"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-model.entered:
	case <-ctx.Done():
		t.Fatal("model did not start")
	}
	queued, err := s.SubmitInput(ctx, sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"remain held"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := &fakeModel{}
	opts.Model = fresh
	reopened, err := sdk.OpenAgentSession(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := reopened.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	before, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var paused *sdk.TraceState = before.Traces[first.TraceID]
	if paused.State != "paused" || !paused.ExecutionStopped {
		t.Fatalf("SDK snapshot lost stopped proof: %+v", paused)
	}
	if err := reopened.Cancel(ctx, first.TraceID); err != nil {
		t.Fatal(err)
	}
	after, err := reopened.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tr := after.Traces[first.TraceID]
	if tr.State != "cancelled" || !tr.Settled || !tr.ExecutionStopped || tr.InvocationID != paused.InvocationID || tr.Usage != paused.Usage || !after.Traces[queued.TraceID].Hold {
		t.Fatal("SDK cancel lost identity, stopped proof, usage, or queue hold")
	}
	if err := reopened.Cancel(ctx, first.TraceID); err != nil {
		t.Fatal(err)
	}
	again, err := reopened.Snapshot(ctx)
	if err != nil || again.Cursor != after.Cursor {
		t.Fatalf("duplicate SDK cancel appended events: %v", err)
	}
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.calls != 0 || model.calls.Load() != 1 {
		t.Fatalf("SDK cancel repeated execution: original=%d fresh=%d", model.calls.Load(), fresh.calls)
	}
}
