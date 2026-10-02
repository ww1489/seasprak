package workflowagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkflowHistoricalSubscriberOwnsDeliveredSequence(t *testing.T) {
	var effects atomic.Int32
	def := toolOnly()
	def.Name = "history-ownership"
	def.Nodes = []WorkflowNode{{ID: "s", Type: "start"}}
	def.Edges = nil
	previous := "s"
	const toolCount = 120
	for i := 0; i < toolCount; i++ {
		id := fmt.Sprintf("t%03d", i)
		def.Nodes = append(def.Nodes, WorkflowNode{ID: id, Type: "tool", Tool: "echo", Inputs: map[string]WorkflowValue{"q": {Literal: json.RawMessage(`"fixed"`)}}})
		def.Edges = append(def.Edges, WorkflowEdge{From: previous, To: id})
		previous = id
	}
	def.Nodes = append(def.Nodes, WorkflowNode{ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output(previous, "result")}})
	def.Edges = append(def.Edges, WorkflowEdge{From: previous, To: "e"})
	opts := testOptions(t, def, nil, &effects)
	backend := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	completed := waitStopped(t, w)
	if completed.State != "completed" || effects.Load() != toolCount {
		t.Fatalf("history fixture state=%s effects=%d", completed.State, effects.Load())
	}
	loaded, err := backend.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	type historicalValue struct {
		sequence uint64
		kind     string
		payload  string
	}
	var original []historicalValue
	for _, commit := range loaded.Commits {
		for _, ev := range commit.Events {
			original = append(original, historicalValue{sequence: *ev.DurableSeq, kind: ev.Type, payload: string(ev.Payload)})
		}
	}
	if len(original) <= 256 {
		t.Fatalf("fixture did not cross a real replay page: events=%d", len(original))
	}
	probe := &reviewLoadedStore{Store: backend, loaded: loaded}
	opts.ReadOnly, opts.Store = true, probe
	opened, err := OpenWorkflowAgent(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close(context.Background())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sub, err := opened.SubscribeFrom(ctx, WorkflowSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	i := 0
	for ev := range sub.Events {
		if i >= len(original) {
			t.Error("replay repeated a history page")
			break
		}
		want := original[i]
		if ev.DurableSeq == nil || *ev.DurableSeq != want.sequence || ev.Type != want.kind || string(ev.Payload) != want.payload {
			t.Errorf("history order/value changed at event %d", i)
		}
		// Ownership transfers to the receiver when the channel delivers this
		// value. The sender must no longer use these pointers for its progress.
		*ev.DurableSeq = completed.DurableSeq + 100
		if len(ev.Payload) > 2 {
			ev.Payload[2] = 'X'
		}
		i++
	}
	if i != len(original) || ctx.Err() != nil || sub.Err() != nil {
		t.Errorf("history did not finish intact: delivered=%d want=%d ctx=%v sub=%v", i, len(original), ctx.Err(), sub.Err())
	}
	<-sub.stopped
	opened.mu.Lock()
	registered := len(opened.subs)
	opened.mu.Unlock()
	after, err := opened.Snapshot(t.Context())
	if err != nil || after.Revision != completed.Revision || after.DurableSeq != completed.DurableSeq || effects.Load() != toolCount || probe.appends.Load() != 0 || registered != 0 {
		t.Fatalf("receiver changed owner or EOF cleanup: err=%v registrations=%d effects=%d", err, registered, effects.Load())
	}
}
