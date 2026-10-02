package workflowagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cloudwego/eino/compose"

	product "github.com/ww1489/seasprak/internal/errors"
)

type fakeWorkflowExec struct {
	mu      sync.Mutex
	calls   map[string]int
	prompts map[string]string
	args    map[string]string
	fail    string
}

func newFakeWorkflowExec() *fakeWorkflowExec {
	return &fakeWorkflowExec{calls: map[string]int{}, prompts: map[string]string{}, args: map[string]string{}}
}

func (f *fakeWorkflowExec) record(nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[nodeID]++
	if nodeID == f.fail {
		return product.NewError(product.CodeResourceUnavailable, "synthetic node failure")
	}
	return nil
}

func (f *fakeWorkflowExec) RunModel(_ context.Context, nodeID, model, prompt string) (string, error) {
	if err := f.record(nodeID); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.prompts[nodeID] = prompt
	f.mu.Unlock()
	return model + ":" + prompt, nil
}

func (f *fakeWorkflowExec) RunTool(_ context.Context, nodeID, tool string, args json.RawMessage) (string, error) {
	if err := f.record(nodeID); err != nil {
		return "", err
	}
	f.mu.Lock()
	f.args[nodeID] = string(args)
	f.mu.Unlock()
	return tool + "(" + string(args) + ")", nil
}

func (f *fakeWorkflowExec) RunSubflow(_ context.Context, nodeID string, sub *CompiledWorkflow, input json.RawMessage) (map[string]any, error) {
	if err := f.record(nodeID); err != nil {
		return nil, err
	}
	return map[string]any{"answer": sub.Definition.Name + string(input)}, nil
}

func (f *fakeWorkflowExec) count(id string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[id]
}

func (f *fakeWorkflowExec) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func wfLit(v string) WorkflowValue { return WorkflowValue{Literal: json.RawMessage(v)} }
func wfRef(n, f string) WorkflowValue {
	return WorkflowValue{Ref: &WorkflowRef{Node: n, Field: f}}
}

// s -> t(tool) -> c(score > 50); true -> hi(model), false -> lo(tool); both -> e.
func testBranchingWorkflow() WorkflowDefinition {
	return WorkflowDefinition{
		Name: "triage", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"score":{"type":"number"}},"required":["name","score"]}`),
		Nodes: []WorkflowNode{
			{ID: "s", Type: WorkflowNodeStart},
			{ID: "t", Type: WorkflowNodeTool, Tool: "echo", Inputs: map[string]WorkflowValue{"q": wfRef("s", "name")}},
			{ID: "c", Type: WorkflowNodeCondition, Condition: &WorkflowCondition{Op: "gt", Left: wfRef("s", "score"), Right: wfLit("50")}},
			{ID: "hi", Type: WorkflowNodeModel, Model: "m1", Prompt: "high {{name}}/{{ echo }}", Inputs: map[string]WorkflowValue{"name": wfRef("s", "name"), "echo": wfRef("t", "result")}},
			{ID: "lo", Type: WorkflowNodeTool, Tool: "shout", Inputs: map[string]WorkflowValue{"text": wfRef("s", "name")}},
			{ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"name": wfRef("s", "name"), "echo": wfRef("t", "result"), "kind": wfLit(`"done"`)}},
			{ID: "orphan", Type: WorkflowNodeTool, Tool: "echo"},
		},
		Edges: []WorkflowEdge{
			{From: "s", To: "t"}, {From: "t", To: "c"},
			{From: "c", To: "hi", Port: "true"}, {From: "c", To: "lo", Port: "false"},
			{From: "hi", To: "e"}, {From: "lo", To: "e"},
		},
	}
}

func testWorkflowBindings() WorkflowBindings {
	obj := json.RawMessage(`{"type":"object"}`)
	return WorkflowBindings{Models: map[string]bool{"m1": true}, Tools: map[string]json.RawMessage{"echo": obj, "shout": obj}}
}

func compileTestWorkflow(t *testing.T, def WorkflowDefinition, b WorkflowBindings) *CompiledWorkflow {
	t.Helper()
	c, err := CompileWorkflow(def, b)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return c
}

func TestWorkflowGraphRunsOnlyTakenBranch(t *testing.T) {
	c := compileTestWorkflow(t, testBranchingWorkflow(), testWorkflowBindings())
	for _, tc := range []struct {
		score        float64
		taken, other string
	}{{90, "hi", "lo"}, {10, "lo", "hi"}} {
		exec := newFakeWorkflowExec()
		r, err := BuildWorkflowGraph(t.Context(), c, exec, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		out, err := r.Invoke(t.Context(), map[string]any{"name": "ada", "score": tc.score})
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		if exec.count("t") != 1 || exec.count(tc.taken) != 1 || exec.count(tc.other) != 0 || exec.count("orphan") != 0 || exec.total() != 2 {
			t.Fatalf("score %v: calls = %v", tc.score, exec.calls)
		}
		want := map[string]any{"name": "ada", "echo": `echo({"q":"ada"})`, "kind": "done"}
		if len(out) != len(want) {
			t.Fatalf("output = %v", out)
		}
		for k, v := range want {
			if out[k] != v {
				t.Fatalf("output[%s] = %v, want %v", k, out[k], v)
			}
		}
		if tc.taken == "hi" && exec.prompts["hi"] != `high ada/echo({"q":"ada"})` {
			t.Fatalf("prompt = %q", exec.prompts["hi"])
		}
		if tc.taken == "lo" && exec.args["lo"] != `{"text":"ada"}` {
			t.Fatalf("tool args = %q", exec.args["lo"])
		}
	}
}

// The join e has a direct predecessor t and a branch predecessor hi/lo, so
// its predecessors finish in different supersteps.
func mixedJoinWorkflow() WorkflowDefinition {
	def := testBranchingWorkflow()
	def.Edges = append(def.Edges, WorkflowEdge{From: "t", To: "e"})
	return def
}

func TestWorkflowGraphTriggerModeForJoins(t *testing.T) {
	c := compileTestWorkflow(t, mixedJoinWorkflow(), testWorkflowBindings())
	for _, score := range []float64{90, 10} {
		exec := newFakeWorkflowExec()
		r, err := BuildWorkflowGraph(t.Context(), c, exec, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := r.Invoke(t.Context(), map[string]any{"name": "ada", "score": score})
		taken, other := "hi", "lo"
		if score < 50 {
			taken, other = "lo", "hi"
		}
		if err != nil || out["kind"] != "done" || exec.count("t") != 1 || exec.count(taken) != 1 || exec.count(other) != 0 {
			t.Fatalf("mixed join score %v: out=%v err=%v calls=%v", score, out, err, exec.calls)
		}
	}
	// AnyPredecessor (Pregel) fires the join as soon as t finishes, so the
	// workflow ends before the taken branch runs; it must not be used.
	anyExec := newFakeWorkflowExec()
	anyGraph, err := buildWorkflowGraph(t.Context(), c, anyExec, nil, compose.AnyPredecessor)
	if err != nil {
		t.Fatal(err)
	}
	out, err := anyGraph.Invoke(t.Context(), map[string]any{"name": "ada", "score": 90})
	if err != nil || out["kind"] != "done" || anyExec.count("t") != 1 || anyExec.count("hi") != 0 {
		t.Fatalf("AnyPredecessor behavior changed; revisit the trigger mode choice: out=%v err=%v calls=%v", out, err, anyExec.calls)
	}
}

func TestWorkflowGraphRejectsInvalidInputBeforeNodes(t *testing.T) {
	c := compileTestWorkflow(t, testBranchingWorkflow(), testWorkflowBindings())
	exec := newFakeWorkflowExec()
	r, err := BuildWorkflowGraph(t.Context(), c, exec, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Invoke(t.Context(), map[string]any{"name": "ada"})
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument || !strings.Contains(pe.Message, "score") || exec.total() != 0 {
		t.Fatalf("missing input: err=%v calls=%v", err, exec.calls)
	}
}

func TestWorkflowGraphNodeFailureStopsDownstream(t *testing.T) {
	c := compileTestWorkflow(t, testBranchingWorkflow(), testWorkflowBindings())
	exec := newFakeWorkflowExec()
	exec.fail = "t"
	r, err := BuildWorkflowGraph(t.Context(), c, exec, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Invoke(t.Context(), map[string]any{"name": "ada", "score": 90})
	var pe *product.Error
	if !errors.As(err, &pe) || pe.Code != product.CodeResourceUnavailable || exec.count("t") != 1 || exec.total() != 1 {
		t.Fatalf("failure: err=%v calls=%v", err, exec.calls)
	}
}

func TestWorkflowGraphSubflowAndConditionOps(t *testing.T) {
	sub := compileTestWorkflow(t, WorkflowDefinition{
		Name: "sub", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		Nodes: []WorkflowNode{
			{ID: "s", Type: WorkflowNodeStart},
			{ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"answer": wfRef("s", "topic")}},
		},
		Edges: []WorkflowEdge{{From: "s", To: "e"}},
	}, WorkflowBindings{})
	for _, tc := range []struct {
		op, right string
		topic     string
		wantSub   int
	}{{"eq", `"go"`, "go", 1}, {"ne", `"go"`, "go", 0}, {"lt", `"m"`, "go", 1}, {"truthy", "", "", 0}} {
		cond := &WorkflowCondition{Op: tc.op, Left: wfRef("s", "topic")}
		if tc.right != "" {
			cond.Right = wfLit(tc.right)
		}
		def := WorkflowDefinition{
			Name: "outer", Version: "v1", Source: "test", FormatVersion: WorkflowFormatV1,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
			Nodes: []WorkflowNode{
				{ID: "s", Type: WorkflowNodeStart},
				{ID: "c", Type: WorkflowNodeCondition, Condition: cond},
				{ID: "x", Type: WorkflowNodeSubflow, Subflow: "sub@v1", Inputs: map[string]WorkflowValue{"topic": wfRef("s", "topic")}},
				{ID: "skip", Type: WorkflowNodeLiteral, Inputs: map[string]WorkflowValue{"v": wfLit("0")}},
				{ID: "e", Type: WorkflowNodeEnd, Inputs: map[string]WorkflowValue{"topic": wfRef("s", "topic")}},
			},
			Edges: []WorkflowEdge{{From: "s", To: "c"}, {From: "c", To: "x", Port: "true"}, {From: "c", To: "skip", Port: "false"}, {From: "x", To: "e"}, {From: "skip", To: "e"}},
		}
		c := compileTestWorkflow(t, def, WorkflowBindings{Subflows: map[string]*CompiledWorkflow{"sub@v1": sub}})
		exec := newFakeWorkflowExec()
		r, err := BuildWorkflowGraph(t.Context(), c, exec, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := r.Invoke(t.Context(), map[string]any{"topic": tc.topic})
		if err != nil || out["topic"] != tc.topic || exec.count("x") != tc.wantSub {
			t.Fatalf("op %s: out=%v err=%v calls=%v", tc.op, out, err, exec.calls)
		}
	}
}

func TestBuildWorkflowGraphRejectsUncompiled(t *testing.T) {
	exec := newFakeWorkflowExec()
	def := testBranchingWorkflow()
	def.Nodes = append(def.Nodes, WorkflowNode{ID: "n3", Type: "code"})
	c, err := CompileWorkflow(def, testWorkflowBindings())
	if err == nil || c != nil {
		t.Fatal("invalid definition compiled")
	}
	for _, candidate := range []*CompiledWorkflow{nil, {Definition: def, Order: []string{"s", "e"}}} {
		r, err := BuildWorkflowGraph(t.Context(), candidate, exec, nil)
		var pe *product.Error
		if r != nil || !errors.As(err, &pe) || pe.Code != product.CodeInvalidArgument {
			t.Fatalf("uncompiled workflow built: %v", err)
		}
	}
	if _, err := BuildWorkflowGraph(t.Context(), compileTestWorkflow(t, testBranchingWorkflow(), testWorkflowBindings()), nil, nil); err == nil {
		t.Fatal("nil executor accepted")
	}
	if exec.total() != 0 {
		t.Fatalf("executor called for invalid definitions: %v", exec.calls)
	}
}

func TestWorkflowGraphCheckpointStoreAndName(t *testing.T) {
	c := compileTestWorkflow(t, testBranchingWorkflow(), testWorkflowBindings())
	store := &p3WorkflowStore{}
	exec := newFakeWorkflowExec()
	r, err := BuildWorkflowGraph(t.Context(), c, exec, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Invoke(t.Context(), map[string]any{"name": "ada", "score": 1}, compose.WithCheckPointID("wf")); err != nil {
		t.Fatalf("invoke with checkpoint: %v", err)
	}
	if gets, _ := store.counts(); gets != 1 {
		t.Fatalf("checkpoint store not consulted: gets=%d", gets)
	}
}
