package workflowagent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

func TestWorkflowSlowSubscriptionDoesNotBlockToolOrCommit(t *testing.T) {
	var calls atomic.Int32
	w := newWorkflow(t, testOptions(t, toolOnly(), nil, &calls))
	before, _ := w.Snapshot(t.Context())
	sub, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{After: before.DurableSeq, Limits: config.Limits{SubscriptionEvents: 1, SubscriptionBytes: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	submit(t, w)
	s := waitStopped(t, w)
	if s.State != "completed" || calls.Load() != 1 {
		t.Fatalf("slow consumer blocked %+v", s)
	}
	select {
	case <-sub.Events:
	case <-time.After(time.Second):
		t.Fatal("overflow did not close")
	}
	sub.Close()
	requireCode(t, sub.Err(), product.CodeResyncRequired)
}
func TestWorkflowToolOutputIsTemporaryBoundedAndFinalized(t *testing.T) {
	var calls atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &calls)
	emitted, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	opts.Tools[0].Run = nil
	opts.Tools[0].RunWithOutput = func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		calls.Add(1)
		for i := 0; i < 4; i++ {
			if err := sink.WriteOutput(ctx, agent.ToolOutputChunk{Stream: "stdout", Text: strings.Repeat("x", config.ToolOutputChunkBytes)}); err != nil {
				return "", err
			}
		}
		close(emitted)
		<-release
		return "done", nil
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	<-emitted
	s, _ := w.Snapshot(t.Context())
	size := 0
	for _, delta := range s.Transient.Tools {
		size += len(delta.Text)
	}
	if size != config.TransientToolPreviewBytes {
		t.Fatalf("temporary preview size %d", size)
	}
	w.mu.Lock()
	for _, ev := range w.state.Events {
		if ev.Type == "tool.output.delta" {
			t.Error("output assigned durable sequence")
		}
	}
	w.mu.Unlock()
	release <- struct{}{}
	s = waitStopped(t, w)
	if len(s.Transient.Tools) != 0 || len(s.Transient.Models) != 0 {
		t.Fatal("terminal output remained live")
	}
}
func TestWorkflowCursorRejectsDifferentResourceAndFuture(t *testing.T) {
	var count atomic.Int32
	w := newWorkflow(t, testOptions(t, toolOnly(), nil, &count))
	s, _ := w.Snapshot(t.Context())
	for _, cursor := range []string{"invalid", encodeCursor("other", 0), encodeCursor(s.RunID, s.DurableSeq+1)} {
		_, err := w.SubscribeFrom(t.Context(), WorkflowSubscribeOptions{Cursor: cursor})
		requireCode(t, err, product.CodeInvalidArgument)
	}
}
