package workflowagent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/storage/memory"
)

func TestWorkflowSubflowInputFailureRecordsFailedNode(t *testing.T) {
	var effects atomic.Int32
	child := toolOnly()
	child.Name = "child"
	child.InputSchema = json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number","minimum":1}},"required":["limit"]}`)
	d := child
	d.Name = "parent"
	d.InputSchema = json.RawMessage(`{"type":"object","properties":{"limit":{"type":"number"}},"required":["limit"]}`)
	d.Nodes = []WorkflowNode{{ID: "s", Type: "start"}, {ID: "sub", Type: "subflow", Subflow: "child@v1", Inputs: map[string]WorkflowValue{"limit": output("s", "limit")}}, {ID: "e", Type: "end", Inputs: map[string]WorkflowValue{"result": output("sub", "result")}}}
	d.Edges = []WorkflowEdge{{From: "s", To: "sub"}, {From: "sub", To: "e"}}
	opts := testOptions(t, d, nil, &effects)
	opts.Subflows = map[string]WorkflowDefinition{"child@v1": child}
	w := newWorkflow(t, opts)
	_, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{"limit":0}`), Principal: "local"})
	if err != nil {
		t.Fatal(err)
	}
	s := waitStopped(t, w)
	if s.State != "failed" || s.ErrorCode != product.CodeInvalidArgument || s.FailedNode == "" || effects.Load() != 0 {
		t.Errorf("subflow validation state=%s code=%s failedNode=%s effects=%d", s.State, s.ErrorCode, s.FailedNode, effects.Load())
	}
	if len(s.WorkflowNodes) != 1 {
		t.Errorf("child effects admitted: %d nodes", len(s.WorkflowNodes))
	}
	for _, n := range s.WorkflowNodes {
		if n.Kind != "subflow" || n.State != "failed" || n.ID != s.FailedNode || n.ErrorCode != product.CodeInvalidArgument {
			t.Errorf("unrecorded admitted failure: %+v", n)
		}
	}
}

func TestWorkflowLookupToolReturnsOwnedProjection(t *testing.T) {
	var effects atomic.Int32
	opts := testOptions(t, toolOnly(), nil, &effects)
	f := injectStore(t, &opts)
	w := newWorkflow(t, opts)
	submit(t, w)
	waitStopped(t, w)
	loaded, err := f.Load(t.Context(), opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the real accepted-call prefix after observation, before node
	// completion. No execution is started by this read-port ownership probe.
	for i, c := range loaded.Commits {
		found := false
		for _, r := range c.ControlRecords {
			found = found || r.Type == "workflow_tool_observation"
		}
		if found {
			loaded.Commits = loaded.Commits[:i+1]
			break
		}
	}
	s, err := replay(loaded, opts.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var call agent.ToolRecord
	for _, call = range s.Calls {
	}
	projection := agent.ToolOutputProjection{CallID: call.Call.CallID, Observation: *call.Observation, Content: "original-projection"}
	backend, err := memory.Open(opts.RunID, loaded.Header)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	for _, c := range loaded.Commits {
		if _, err := backend.Append(t.Context(), opts.RunID, storage.ExpectedCommit{ExpectedPreviousSeq: c.ExpectedPreviousSeq}, c); err != nil {
			t.Fatal(err)
		}
	}
	probe := assemble(opts, backend, nil, s.Initial.BindingVersion)
	probe.state = s
	probe.active = &segment{scope: call.Scope, ctx: context.Background()}
	probe.mu.Lock()
	err = probe.commitLocked(t.Context(), []storage.Record{record("workflow_tool_projection", call.Call.CallID, projection)}, nil)
	probe.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	returned, err := probe.LookupWorkflowTool(t.Context(), call.Scope, call.Call.CallID)
	if err != nil {
		t.Fatal(err)
	}
	returned.Observation.Content = "mutated-observation"
	returned.Projection.Content = "mutated-projection"
	again, err := probe.LookupWorkflowTool(t.Context(), call.Scope, call.Call.CallID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Projection.Content != "original-projection" || again.Observation.Content != call.Observation.Content {
		t.Errorf("Lookup exposed internal pointers: %+v", again)
	}
	if effects.Load() != 1 {
		t.Fatal("read port executed an effect")
	}
}
