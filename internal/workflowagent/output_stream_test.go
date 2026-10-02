package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
)

func TestWorkflowTemporaryOutputSeparatesStdoutAndStderr(t *testing.T) {
	var count atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &count)
	ready, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	opts.Tools[0].Run = nil
	opts.Tools[0].RunWithOutput = func(ctx context.Context, _ json.RawMessage, sink agent.ToolOutputSink) (string, error) {
		for _, chunk := range []agent.ToolOutputChunk{{Stream: "stdout", Text: "out"}, {Stream: "stderr", Text: "err"}} {
			if err := sink.WriteOutput(ctx, chunk); err != nil {
				return "", err
			}
		}
		close(ready)
		<-release
		return "done", nil
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	<-ready
	s, _ := w.Snapshot(t.Context())
	if len(s.Transient.Tools) != 2 {
		t.Errorf("separate output streams=%d want 2", len(s.Transient.Tools))
	}
	texts := map[string]string{}
	for _, d := range s.Transient.Tools {
		texts[d.Stream] = d.Text
	}
	if texts["stdout"] != "out" || texts["stderr"] != "err" {
		t.Errorf("output streams mixed: %v", texts)
	}
	release <- struct{}{}
	waitStopped(t, w)
}
