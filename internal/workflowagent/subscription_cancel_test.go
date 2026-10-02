package workflowagent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkflowReadOnlyHistoricalSubscriptionsUnregister(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	w := newWorkflow(t, opts)
	before, _ := w.Snapshot(t.Context())
	ro := opts
	ro.ReadOnly = true
	ro.Tools, ro.Models = nil, nil
	browser, err := OpenWorkflowAgent(t.Context(), ro)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close(context.Background())
	for i := 0; i < 8; i++ {
		sub, err := browser.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var events uint64
		deadline := time.NewTimer(5 * time.Second)
		finished := false
		for !finished {
			select {
			case ev, ok := <-sub.Events:
				if !ok {
					finished = true
					break
				}
				if ev.DurableSeq == nil {
					t.Fatal("read-only instance resurrected transient facts")
				}
				events++
			case <-deadline.C:
				sub.Close()
				t.Fatal("read-only history did not finish without a future commit")
			}
		}
		deadline.Stop()
		<-sub.stopped
		if sub.Err() != nil || events != before.DurableSeq {
			t.Fatalf("history stopped incorrectly: events=%d err=%v", events, sub.Err())
		}
		browser.mu.Lock()
		retained := len(browser.subs)
		browser.mu.Unlock()
		sub.mu.Lock()
		queued, bytes := len(sub.queue)+len(sub.handoff), sub.bytes
		sub.mu.Unlock()
		if retained != 0 || queued != 0 || bytes != 0 {
			t.Fatalf("historical subscriber retained registrations/buffer: %d/%d/%d", retained, queued, bytes)
		}
		sub.Close()
	}
	after, _ := w.Snapshot(t.Context())
	if after.Revision != before.Revision || count.Load() != 0 {
		t.Fatal("read-only history executed or changed the writer")
	}
}

func TestWorkflowCancelledSubscriptionUnregistersWithoutFutureCommit(t *testing.T) {
	var count atomic.Int32
	w := newWorkflow(t, testOptions(t, toolOnly(), nil, &count))
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := w.SubscribeFrom(ctx, WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	cancel()
	select {
	case <-sub.stopped:
	case <-time.After(time.Second):
		t.Fatal("cancelled subscription did not exit")
	}
	w.mu.Lock()
	retained := len(w.subs)
	w.mu.Unlock()
	if retained != 0 {
		t.Errorf("cancelled subscription retained %d registrations", retained)
	}
	s, _ := w.Snapshot(t.Context())
	if s.State != "created" || s.Usage.ToolExecutions != 0 {
		t.Fatal("subscription cancellation changed execution")
	}
}
