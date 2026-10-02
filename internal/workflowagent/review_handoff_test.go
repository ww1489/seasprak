package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestWorkflowSubscribeReplaysPreRegistrationToolPreview(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	emitted, advance, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	opts.Tools[0].Run = nil
	opts.Tools[0].RunWithOutput = func(ctx context.Context, _ json.RawMessage, output agent.ToolOutputSink) (string, error) {
		calls.Add(1)
		if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "before-registration"}); err != nil {
			return "", err
		}
		close(emitted)
		select {
		case <-advance:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if err := output.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: "after-registration"}); err != nil {
			return "", err
		}
		select {
		case <-release:
			return "finished", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	w := newWorkflow(t, opts)
	initial, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	submit(t, w)
	select {
	case <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not reach its first output boundary")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	sub, err := w.SubscribeFrom(ctx, WorkflowSubscribeOptions{After: initial.DurableSeq})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	close(advance)
	oldCount, newCount := 0, 0
	handoffSeen := false
	for {
		select {
		case ev, ok := <-sub.Events:
			if !ok {
				t.Fatalf("subscription ended before handoff: old=%d new=%d err=%v", oldCount, newCount, sub.Err())
			}
			if ev.Scope.WorkflowRunID != initial.RunID || ev.Scope.SessionID != "" {
				t.Fatal("subscription returned another resource's event")
			}
			if ev.DurableSeq != nil {
				if *ev.DurableSeq <= sub.Handoff && handoffSeen {
					t.Error("durable history was delivered after the temporary handoff view")
				}
				continue
			}
			var preview struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Payload, &preview) != nil || preview.Text == "" {
				continue
			}
			switch preview.Text {
			case "before-registration":
				oldCount++
				handoffSeen = true
			case "after-registration":
				newCount++
				if oldCount != 1 {
					t.Errorf("registration lost or reordered its fixed preview: old=%d new=%d", oldCount, newCount)
				}
				release <- struct{}{}
				final := waitStopped(t, w)
				if final.State != "completed" || calls.Load() != 1 || len(final.Transient.Tools) != 0 {
					t.Errorf("handoff observer changed execution: state=%s calls=%d live=%d", final.State, calls.Load(), len(final.Transient.Tools))
				}
				if oldCount != 1 || newCount != 1 {
					t.Errorf("temporary handoff counts old=%d new=%d, want one of each", oldCount, newCount)
				}
				return
			}
		case <-ctx.Done():
			t.Fatalf("temporary handoff missing: old=%d new=%d", oldCount, newCount)
		}
	}
}
