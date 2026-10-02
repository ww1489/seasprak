package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/config"
	"github.com/ww1489/seasprak/internal/storage"
)

func migrationLiteral(raw string) WorkflowValue {
	return WorkflowValue{Literal: json.RawMessage(raw)}
}

func migrationTriage() WorkflowDefinition {
	return WorkflowDefinition{
		Name: "migration-triage", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"score":{"type":"number"}},"required":["name","score"]}`),
		Nodes: []WorkflowNode{
			{ID: "s", Type: WorkflowNodeStart},
			{ID: "lit", Type: WorkflowNodeLiteral, Inputs: map[string]WorkflowValue{"greeting": migrationLiteral(`"hi"`)}},
			{ID: "echo", Type: WorkflowNodeTool, Tool: "echo", Inputs: map[string]WorkflowValue{"q": output("s", "name"), "g": output("lit", "greeting")}},
			{ID: "c", Type: WorkflowNodeCondition, Condition: &WorkflowCondition{Op: "gt", Left: output("s", "score"), Right: migrationLiteral("50")}},
			{ID: "shout", Type: WorkflowNodeTool, Tool: "shout", Inputs: map[string]WorkflowValue{"text": output("echo", "result")}},
			{ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"name": output("s", "name"), "echo": output("echo", "result")}},
		},
		Edges: []WorkflowEdge{{From: "s", To: "lit"}, {From: "lit", To: "echo"}, {From: "echo", To: "c"}, {From: "c", To: "shout", Port: "true"}, {From: "c", To: "e", Port: "false"}, {From: "shout", To: "e"}},
	}
}

func migrationReadTool(name string, run func(context.Context, json.RawMessage) (string, error)) tools.Definition {
	return tools.Definition{Name: name, Version: "v1", Schema: json.RawMessage(`{"type":"object"}`), Execution: tools.ExecutionDescription{Effect: "read", Concurrency: "shared", Resources: []agent.ExecutionResource{{Identity: "migration-input"}}}, Run: run}
}

func migrationAssertIdentities(t *testing.T, w *WorkflowAgent, s WorkflowSnapshot, toolCount int) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.state.Calls) != toolCount || len(w.state.Attempts) != 0 || len(w.state.Requests) != 0 || len(s.ModelAttempts) != 0 {
		t.Errorf("effect records: tools=%d modelAttempts=%d physicalRequests=%d snapshotAttempts=%d", len(w.state.Calls), len(w.state.Attempts), len(w.state.Requests), len(s.ModelAttempts))
	}
	for id, n := range s.WorkflowNodes {
		wantID := s.RunID + ":" + n.InvocationID + ":" + n.Path + ":1"
		if id != n.ID || n.ID != wantID || n.Ordinal != 1 || n.State != "completed" || n.DefinitionHash == "" || n.InvocationID == "" {
			t.Errorf("unstable or unfinished effect node: %+v", n)
		}
		if n.Kind == "subflow" {
			if n.ChildInvocationID == "" || n.ChildInvocationID == n.InvocationID || n.ToolCallID != "" {
				t.Errorf("subflow identity: %+v", n)
			}
			continue
		}
		if n.Kind != "tool" || n.ToolCallID != n.ID {
			t.Errorf("effect kind/call identity: %+v", n)
			continue
		}
		call, ok := w.state.Calls[n.ToolCallID]
		if !ok || !call.Claimed || call.Observation == nil || call.Observation.Status != "succeeded" || !call.Observation.Executed || call.Call.CallID != n.ID || call.Call.ProviderCallID != "" || call.Call.OperationID != "" {
			t.Errorf("controlled node call: %+v", call)
		}
		scope := call.Scope
		if scope.WorkflowRunID != s.RunID || scope.NodeExecutionID != n.ID || scope.InvocationID != n.InvocationID || scope.WorkflowDefinitionHash != n.DefinitionHash || scope.Generation != s.BindingVersion || scope.ExecutionID == "" || scope.SessionID != "" || scope.BranchID != "" || scope.TraceID != "" || scope.TurnID != "" || scope.SelectionRevision != 0 {
			t.Errorf("workflow-only call scope: %+v", scope)
		}
		frozen, ok := w.state.Frozen["execution:"+n.ID]
		if !ok || frozen.Origin != "workflow_node" || frozen.Scope != scope || frozen.NodeExecutionID != n.ID || frozen.DefinitionRef != n.DefinitionHash || frozen.BindingRef != s.BindingVersion || frozen.ProviderCallID != "" {
			t.Errorf("frozen execution binding: %+v", frozen)
		}
	}
}

func TestWorkflowMigrationConditionalBranchesAndStructuredValues(t *testing.T) {
	for _, tc := range []struct {
		branch, input, name string
		shout, tools        int32
	}{
		{"true", `{"name":"ada","score":80}`, "ada", 1, 2},
		{"false", `{"name":"bo","score":10}`, "bo", 0, 1},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			var echo, shout atomic.Int32
			opts := testOptions(t, migrationTriage(), nil, &echo)
			opts.Models = nil
			opts.Tools = []tools.Definition{
				migrationReadTool("echo", func(_ context.Context, raw json.RawMessage) (string, error) { echo.Add(1); return string(raw), nil }),
				migrationReadTool("shout", func(_ context.Context, raw json.RawMessage) (string, error) { shout.Add(1); return string(raw), nil }),
			}
			w := newWorkflow(t, opts)
			if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(tc.input), Principal: "local", IdempotencyKey: "input"}); err != nil {
				t.Fatal(err)
			}
			s := waitStopped(t, w)
			t.Logf("state=%s stopped=%t echo=%d shout=%d logical=%d physical=%d tools=%d nodes=%d", s.State, s.ExecutionStopped, echo.Load(), shout.Load(), s.Usage.LogicalModelCalls, s.Usage.TransportRequests, s.Usage.ToolExecutions, len(s.WorkflowNodes))
			if s.State != "completed" || !s.ExecutionStopped || echo.Load() != 1 || shout.Load() != tc.shout || s.Usage.ToolExecutions != int(tc.tools) || s.Usage.LogicalModelCalls != 0 || s.Usage.TransportRequests != 0 || len(s.WorkflowNodes) != int(tc.tools) {
				t.Fatalf("conditional run counts/state did not match branch %s", tc.branch)
			}
			var result struct{ Name, Echo string }
			if err := json.Unmarshal(s.Result, &result); err != nil {
				t.Fatal(err)
			}
			var echoed struct{ Q, G string }
			if err := json.Unmarshal([]byte(result.Echo), &echoed); err != nil {
				t.Fatal(err)
			}
			if result.Name != tc.name || echoed.Q != tc.name || echoed.G != "hi" {
				t.Errorf("structured result=%+v echo=%+v", result, echoed)
			}
			for _, n := range s.WorkflowNodes {
				if tc.shout == 0 && n.NodeID == "shout" {
					t.Error("untaken branch has a NodeRun")
				}
				if n.NodeID == "echo" && n.Result != result.Echo {
					t.Error("end did not select the original echo output")
				}
				if n.NodeID == "shout" {
					var args struct{ Text string }
					if err := json.Unmarshal([]byte(n.Result), &args); err != nil || args.Text != result.Echo {
						t.Errorf("shout did not receive the original echo output: error=%v", err)
					}
				}
			}
			migrationAssertIdentities(t, w, s, int(tc.tools))
		})
	}
}

// Observations happen after the real memory append. This wrapper never changes
// a production transition and delegates Close to the actual memory backend.
type migrationObservedStore struct {
	storage.Store
	closes     atomic.Int32
	aCompleted chan struct{}
	aOnce      sync.Once
}

func (s *migrationObservedStore) Append(ctx context.Context, id string, expected storage.ExpectedCommit, commit storage.Commit) (storage.CommitReceipt, error) {
	receipt, err := s.Store.Append(ctx, id, expected, commit)
	if err == nil && s.aCompleted != nil {
		for _, rec := range commit.ControlRecords {
			var p nodeRecord
			if rec.Type == "workflow_node" && json.Unmarshal(rec.Payload, &p) == nil && p.Node.NodeID == "a" && p.Node.State == "completed" {
				s.aOnce.Do(func() { close(s.aCompleted) })
			}
		}
	}
	return receipt, err
}

func (s *migrationObservedStore) Close() error {
	s.closes.Add(1)
	return s.Store.Close()
}

func migrationWaitSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatalf("did not observe %s: %v", name, ctx.Err())
	}
}

func TestWorkflowMigrationUnequalParallelJoin(t *testing.T) {
	var a, b1, b2, join atomic.Int32
	aEntered, aGate := make(chan struct{}), make(chan struct{})
	b2Entered, b2Gate := make(chan struct{}), make(chan struct{})
	releaseA := sync.OnceFunc(func() { close(aGate) })
	releaseB2 := sync.OnceFunc(func() { close(b2Gate) })
	defer releaseA()
	defer releaseB2()
	d := WorkflowDefinition{
		Name: "migration-unequal-join", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		Nodes: []WorkflowNode{
			{ID: "s", Type: WorkflowNodeStart},
			{ID: "a", Type: WorkflowNodeTool, Tool: "a", Inputs: map[string]WorkflowValue{"q": output("s", "q")}},
			{ID: "b1", Type: WorkflowNodeTool, Tool: "b1", Inputs: map[string]WorkflowValue{"q": output("s", "q")}},
			{ID: "b2", Type: WorkflowNodeTool, Tool: "b2", Inputs: map[string]WorkflowValue{"previous": output("b1", "result")}},
			// The existing static compiler requires a reference to dominate all
			// paths. Neither a nor b2 dominates join, so keep its legal common
			// start binding and check both committed branch outputs separately.
			{ID: "join", Type: WorkflowNodeTool, Tool: "join", Inputs: map[string]WorkflowValue{"q": output("s", "q"), "joined": migrationLiteral("true")}},
			{ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"result": output("join", "result")}},
		},
		Edges: []WorkflowEdge{{From: "s", To: "a"}, {From: "a", To: "join"}, {From: "s", To: "b1"}, {From: "b1", To: "b2"}, {From: "b2", To: "join"}, {From: "join", To: "e"}},
	}
	opts := testOptions(t, d, nil, &a)
	opts.Models = nil
	opts.Tools = []tools.Definition{
		migrationReadTool("a", func(ctx context.Context, raw json.RawMessage) (string, error) {
			if a.Add(1) == 1 {
				close(aEntered)
			}
			select {
			case <-aGate:
				return string(raw), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}),
		migrationReadTool("b1", func(_ context.Context, raw json.RawMessage) (string, error) { b1.Add(1); return string(raw), nil }),
		migrationReadTool("b2", func(ctx context.Context, raw json.RawMessage) (string, error) {
			if b2.Add(1) == 1 {
				close(b2Entered)
			}
			select {
			case <-b2Gate:
				return string(raw), nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}),
		migrationReadTool("join", func(_ context.Context, raw json.RawMessage) (string, error) {
			join.Add(1)
			select {
			case <-b2Gate:
			default:
				t.Error("join actually invoked before b2 was released")
			}
			return string(raw), nil
		}),
	}
	f := injectStore(t, &opts)
	observed := &migrationObservedStore{Store: f.Store, aCompleted: make(chan struct{})}
	opts.Store = observed
	w := newWorkflow(t, opts)
	if _, err := w.SubmitInput(t.Context(), WorkflowInputCommand{Input: json.RawMessage(`{"q":"original"}`), Principal: "local", IdempotencyKey: "input"}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		t.Logf("actual parallel calls: a=%d b1=%d b2=%d join=%d", a.Load(), b1.Load(), b2.Load(), join.Load())
	}()
	migrationWaitSignal(t, aEntered, "a entry")
	releaseA()
	migrationWaitSignal(t, observed.aCompleted, "durable a completion")
	migrationWaitSignal(t, b2Entered, "b2 entry after a completion")
	before, err := w.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("before b2 release: state=%s stopped=%t a=%d b1=%d b2=%d join=%d logical=%d physical=%d tools=%d", before.State, before.ExecutionStopped, a.Load(), b1.Load(), b2.Load(), join.Load(), before.Usage.LogicalModelCalls, before.Usage.TransportRequests, before.Usage.ToolExecutions)
	if join.Load() != 0 || before.ExecutionStopped {
		t.Errorf("join/exit before b2 release: join=%d state=%s stopped=%t", join.Load(), before.State, before.ExecutionStopped)
	}
	for _, n := range before.WorkflowNodes {
		if n.NodeID == "join" {
			t.Error("join admitted before b2 completed")
		}
	}
	releaseB2()
	s := waitStopped(t, w)
	t.Logf("state=%s stopped=%t a=%d b1=%d b2=%d join=%d logical=%d physical=%d tools=%d", s.State, s.ExecutionStopped, a.Load(), b1.Load(), b2.Load(), join.Load(), s.Usage.LogicalModelCalls, s.Usage.TransportRequests, s.Usage.ToolExecutions)
	if s.State != "completed" || !s.ExecutionStopped || a.Load() != 1 || b1.Load() != 1 || b2.Load() != 1 || join.Load() != 1 || s.Usage.ToolExecutions != 4 || s.Usage.LogicalModelCalls != 0 || s.Usage.TransportRequests != 0 || len(s.WorkflowNodes) != 4 {
		t.Fatal("unequal parallel branches did not join exactly once")
	}
	for _, n := range s.WorkflowNodes {
		switch n.NodeID {
		case "a", "b1":
			if n.Result != `{"q":"original"}` {
				t.Errorf("branch %s lost original input: %q", n.NodeID, n.Result)
			}
		case "b2":
			var args struct{ Previous string }
			if err := json.Unmarshal([]byte(n.Result), &args); err != nil || args.Previous != `{"q":"original"}` {
				t.Errorf("b2 lost original b1 output: error=%v result=%q", err, n.Result)
			}
		}
	}
	var result struct{ Result string }
	if err := json.Unmarshal(s.Result, &result); err != nil || result.Result != `{"joined":true,"q":"original"}` {
		t.Errorf("legal join input/result: error=%v result=%q", err, result.Result)
	}
	migrationAssertIdentities(t, w, s, 4)
}

func migrationParent(name, child string) WorkflowDefinition {
	return WorkflowDefinition{
		Name: name, Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1, Resumable: true,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Nodes:       []WorkflowNode{{ID: "s", Type: WorkflowNodeStart}, {ID: child, Type: WorkflowNodeSubflow, Subflow: child + "@v1"}, {ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"result": output(child, "result")}}},
		Edges:       []WorkflowEdge{{From: "s", To: child}, {From: child, To: "e"}},
	}
}

func TestWorkflowMigrationDepthFourStaticSubflows(t *testing.T) {
	if config.SubagentDepth != 4 {
		t.Fatalf("approved root-depth-zero contract changed: depth=%d", config.SubagentDepth)
	}
	var calls atomic.Int32
	leaf := toolOnly()
	leaf.Name = "child4"
	opts := testOptions(t, migrationParent("root", "child1"), nil, &calls)
	opts.Models = nil
	opts.Subflows = map[string]WorkflowDefinition{
		"child1@v1": migrationParent("child1", "child2"),
		"child2@v1": migrationParent("child2", "child3"),
		"child3@v1": migrationParent("child3", "child4"),
		"child4@v1": leaf,
	}
	w := newWorkflow(t, opts)
	submit(t, w)
	s := waitStopped(t, w)
	t.Logf("state=%s stopped=%t leafTools=%d logical=%d physical=%d tools=%d nodes=%d", s.State, s.ExecutionStopped, calls.Load(), s.Usage.LogicalModelCalls, s.Usage.TransportRequests, s.Usage.ToolExecutions, len(s.WorkflowNodes))
	if s.State != "completed" || !s.ExecutionStopped || calls.Load() != 1 || s.Usage.ToolExecutions != 1 || s.Usage.LogicalModelCalls != 0 || s.Usage.TransportRequests != 0 || len(s.WorkflowNodes) != 5 {
		t.Fatal("legal depth-four static subflow did not complete once")
	}
	var result struct{ Result string }
	if err := json.Unmarshal(s.Result, &result); err != nil || result.Result != `{"q":"fixed"}` {
		t.Errorf("leaf result did not propagate through every end: error=%v result=%q", err, result.Result)
	}
	w.mu.Lock()
	invocation := w.state.Run.InvocationID
	frozen := w.state.Initial.Manifest.Subflows
	hashes := w.state.Initial.Manifest.SubflowHashes
	w.mu.Unlock()
	if len(frozen) != 4 || len(hashes) != 4 {
		t.Errorf("frozen subflow bindings: definitions=%d hashes=%d", len(frozen), len(hashes))
	}
	byPath := map[string]NodeRun{}
	for _, n := range s.WorkflowNodes {
		byPath[n.Path] = n
	}
	seen := map[string]bool{invocation: true}
	path := ""
	for _, name := range []string{"child1", "child2", "child3", "child4"} {
		path += name
		n, ok := byPath[path]
		key := name + "@v1"
		if !ok || n.Kind != "subflow" || n.InvocationID != invocation || n.ChildInvocationID == "" || seen[n.ChildInvocationID] || n.Result != string(s.Result) || frozen[key].Name != name || frozen[key].Version != "v1" || hashes[key] == "" {
			t.Fatalf("nested identity/result/binding at %s: %+v", path, n)
		}
		seen[n.ChildInvocationID] = true
		invocation = n.ChildInvocationID
		path += "/"
	}
	inner := byPath[path+"t"]
	if inner.Kind != "tool" || inner.InvocationID != invocation || inner.Path != "child1/child2/child3/child4/t" {
		t.Errorf("leaf invocation/path: %+v", inner)
	}
	w.mu.Lock()
	call := w.state.Calls[inner.ToolCallID]
	w.mu.Unlock()
	parent := byPath[strings.TrimSuffix(path, "/")]
	if call.Scope.ParentInvocationID != parent.InvocationID {
		t.Errorf("leaf parent scope=%s, want %s", call.Scope.ParentInvocationID, parent.InvocationID)
	}
	migrationAssertIdentities(t, w, s, 1)
}

func TestWorkflowMigrationCloseTimeoutDoesNotProveExit(t *testing.T) {
	var calls atomic.Int32
	entered, cancelled, gate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release()
	opts := testOptions(t, toolOnly(), nil, &calls)
	opts.Models = nil
	opts.Tools[0].Run = func(ctx context.Context, _ json.RawMessage) (string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-ctx.Done()
		close(cancelled)
		<-gate // Cancellation acknowledgement is deliberately not execution exit.
		return "", ctx.Err()
	}
	f := injectStore(t, &opts)
	observed := &migrationObservedStore{Store: f.Store}
	opts.Store = observed
	w := newWorkflow(t, opts)
	submit(t, w)
	migrationWaitSignal(t, entered, "non-cooperative tool entry")
	w.mu.Lock()
	frame := w.active
	var call agent.ToolRecord
	for _, c := range w.state.Calls {
		call = c
	}
	usage := w.state.Usage
	w.mu.Unlock()
	if frame == nil || calls.Load() != 1 || !call.Claimed || call.Observation != nil || usage.ToolExecutions != 1 || usage.LogicalModelCalls != 0 || usage.TransportRequests != 0 || call.Scope.WorkflowRunID != opts.RunID || call.Scope.InvocationID == "" || call.Scope.NodeExecutionID != call.Call.CallID || call.Scope.ExecutionID != frame.scope.ExecutionID || call.Scope.SessionID != "" || call.Scope.TraceID != "" || call.Scope.TurnID != "" {
		t.Fatal("non-cooperative tool was not admitted under its independent workflow scope")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	err := w.Close(ctx)
	cancel()
	migrationWaitSignal(t, cancelled, "tool cancellation acknowledgement")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("first Close error=%v, want deadline exceeded", err)
	}
	executionDone := false
	select {
	case <-frame.done:
		executionDone = true
		t.Error("Close timeout reported actual exit while runner remains blocked")
	default:
	}
	if observed.closes.Load() != 0 || calls.Load() != 1 {
		t.Errorf("first Close released store/repeated effect: storeClose=%d effects=%d", observed.closes.Load(), calls.Load())
	}
	t.Logf("firstClose=%v storeClose=%d effects=%d logical=%d physical=%d tools=%d executionDone=%t", err, observed.closes.Load(), calls.Load(), usage.LogicalModelCalls, usage.TransportRequests, usage.ToolExecutions, executionDone)
	secondStarted, secondDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(secondStarted)
		secondDone <- w.Close(context.Background())
	}()
	migrationWaitSignal(t, secondStarted, "second Close caller")
	waiting, stopWaiting := context.WithTimeout(t.Context(), 20*time.Millisecond)
	select {
	case err := <-secondDone:
		stopWaiting()
		t.Fatalf("second Close returned before runner release: %v", err)
	case <-waiting.Done():
		if !errors.Is(waiting.Err(), context.DeadlineExceeded) {
			stopWaiting()
			t.Fatalf("second Close wait interrupted: %v", waiting.Err())
		}
	}
	stopWaiting()
	if observed.closes.Load() != 0 {
		t.Error("second Close closed the store before actual exit")
	}
	t.Logf("secondCloseStillWaiting=true storeClose=%d effects=%d", observed.closes.Load(), calls.Load())
	release()
	wait, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	var secondErr error
	select {
	case secondErr = <-secondDone:
		if secondErr != nil {
			t.Errorf("second Close failed after release: %v", secondErr)
		}
	case <-wait.Done():
		t.Fatalf("second Close did not complete after real runner exit: %v", wait.Err())
	}
	migrationWaitSignal(t, frame.done, "original execution frame exit")
	repeatedErr := w.Close(wait)
	if repeatedErr != nil {
		t.Errorf("repeated Close: %v", repeatedErr)
	}
	if observed.closes.Load() != 1 || calls.Load() != 1 {
		t.Errorf("Close/effect not exactly once: storeClose=%d effects=%d", observed.closes.Load(), calls.Load())
	}
	t.Logf("secondClose=%v repeatedClose=%v storeClose=%d effects=%d executionDone=true", secondErr, repeatedErr, observed.closes.Load(), calls.Load())
}
