package consumer_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ww1489/seasprak/sdk"
)

// Compile the historical method signatures as part of the independent module.
var (
	_ func(*sdk.AgentSession, context.Context, string) error         = (*sdk.AgentSession).Cancel
	_ func(*sdk.AgentSession, context.Context, string) error         = (*sdk.AgentSession).ContinueQueue
	_ func(*sdk.AgentSession, sdk.Limits) (<-chan sdk.Event, func()) = (*sdk.AgentSession).Subscribe
)

func TestSDKConsumerSubscriptionOverflowAndCursor(t *testing.T) {
	s, err := sdk.CreateAgentSession(t.Context(), sdk.SessionOptions{SessionID: "consumer-events", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: &fakeModel{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	limits := sdk.DefaultLimits()
	limits.SubscriptionBytes = 1 // Even one event cannot fit: no timing assumption.
	slow := s.SubscribeEvents(limits)
	t.Cleanup(slow.Close)
	fast := s.SubscribeEvents(sdk.DefaultLimits())
	t.Cleanup(fast.Close)
	input, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"hello"}`)})
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var last uint64
	settled := false
	for !settled {
		select {
		case ev, ok := <-fast.Events:
			if !ok {
				t.Fatalf("healthy subscription closed: %v", fast.Err())
			}
			if ev.DurableSeq != nil {
				if *ev.DurableSeq <= last {
					t.Fatal("durable event cursor did not increase")
				}
				last = *ev.DurableSeq
				snapshot, err := s.Snapshot(t.Context())
				if err != nil || snapshot.Cursor < last {
					t.Fatalf("event published before committed snapshot: %v", err)
				}
			} else if ev.Type == "message.started" || ev.Type == "message.snapshot" {
				if ev.StreamID == "" || ev.ChunkSeq == nil {
					t.Fatal("temporary event lacks stream position")
				}
			}
			settled = ev.Type == "trace.settled" && ev.Scope.TraceID == input.TraceID
		case <-timer.C:
			t.Fatal("healthy observer did not see settled event")
		}
	}
	select {
	case _, ok := <-slow.Events:
		if ok {
			t.Fatal("overflow subscription delivered an event beyond its byte cap")
		}
	case <-timer.C:
		t.Fatal("overflow subscription did not close")
	}
	if pe, ok := sdk.AsError(slow.Err()); !ok || pe.Code != sdk.CodeResyncRequired {
		t.Fatalf("overflow error = %v", slow.Err())
	}
	before, err := s.Snapshot(t.Context())
	if err != nil || before.Traces[input.TraceID].State != "completed" {
		t.Fatalf("slow observer blocked execution: %v", err)
	}
	if err := s.Cancel(t.Context(), input.TraceID); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(t.Context(), input.TraceID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Snapshot(t.Context())
	if err != nil || after.Cursor != before.Cursor || after.Revision != before.Revision {
		t.Fatalf("duplicate terminal cancel wrote facts: %v", err)
	}
	var closers sync.WaitGroup
	for range 8 {
		closers.Go(func() { slow.Close(); fast.Close() })
	}
	closers.Wait()
}

func TestSDKConsumerContinueQueueKeepsOldSignatureAndTarget(t *testing.T) {
	blocked := &stoppedModel{entered: make(chan struct{})}
	opts := sdk.SessionOptions{SessionID: "consumer-continue", Workspace: t.TempDir(), StateRoot: t.TempDir(), Profile: sdk.ProfileMemory, Model: blocked}
	s, err := sdk.CreateAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	first, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"block"}`)})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not enter")
	}
	queued, err := s.SubmitInput(t.Context(), sdk.InputCommand{Kind: "prompt", Content: json.RawMessage(`{"text":"queue"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	fresh := &fakeModel{}
	opts.Model = fresh
	opened, err := sdk.OpenAgentSession(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	before, err := opened.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Cancel(t.Context(), first.TraceID); err != nil {
		t.Fatal(err)
	}
	if err := opened.ContinueQueue(t.Context(), queued.TraceID); err != nil {
		t.Fatal(err)
	}
	after := waitConsumerApprovalState(t, opened, queued.TraceID, "completed")
	if after.Traces[first.TraceID].State != "cancelled" || after.Traces[queued.TraceID].Hold || after.Traces[queued.TraceID].Generation != before.Traces[queued.TraceID].Generation {
		t.Fatal("ContinueQueue changed target binding or restarted cancelled work")
	}
	fresh.mu.Lock()
	defer fresh.mu.Unlock()
	if fresh.calls != 1 || blocked.calls.Load() != 1 {
		t.Fatalf("unexpected model execution counts: %d/%d", fresh.calls, blocked.calls.Load())
	}
}
