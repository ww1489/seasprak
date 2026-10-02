package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/codeagent/state"
)

// Invocations project identity, parent, target and state only; the child's
// result text, call list and model counts stay server-side.
func TestSnapshotProjectsInvocationsWithoutPrivateFields(t *testing.T) {
	snap := emptySnapshot(3)
	if out := projectSnapshot(snap); out.Invocations == nil || len(out.Invocations) != 0 {
		t.Fatalf("empty invocations must encode as [] got %#v", out.Invocations)
	}
	snap.Invocations = map[string]state.Invocation{
		"b": {ID: "b", ParentInvocationID: "a", ParentCallID: "call-private-b", TraceID: "t1", Target: agent.TargetAgent{Name: "writer", Version: "w1", Hash: "hash-private"}, State: "interrupted", Result: "result-private", ModelCalls: 2, CallIDs: []string{"child-call-private"}},
		"a": {ID: "a", ParentInvocationID: "root", ParentCallID: "call-private-a", TraceID: "t1", Target: agent.TargetAgent{Name: "reviewer", Version: "r1"}, State: "completed", Result: "result-private"},
	}
	out := projectSnapshot(snap)
	want := []invocationDTO{
		{InvocationID: "a", ParentInvocationID: "root", TraceID: "t1", TargetAgent: "reviewer", State: "completed"},
		{InvocationID: "b", ParentInvocationID: "a", TraceID: "t1", TargetAgent: "writer", State: "interrupted"},
	}
	if len(out.Invocations) != len(want) || out.Invocations[0] != want[0] || out.Invocations[1] != want[1] {
		t.Fatalf("invocations %+v", out.Invocations)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private", "modelCalls", "callIds", "parentCallId", "result"} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("snapshot leaked %q: %s", private, raw)
		}
	}
}
