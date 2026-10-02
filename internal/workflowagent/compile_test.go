package workflowagent_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	product "github.com/ww1489/seasprak/internal/errors"
	agent "github.com/ww1489/seasprak/internal/workflowagent"
)

func lit(v string) agent.WorkflowValue { return agent.WorkflowValue{Literal: json.RawMessage(v)} }
func ref(node, field string) agent.WorkflowValue {
	return agent.WorkflowValue{Ref: &agent.WorkflowRef{Node: node, Field: field}}
}

const wfSchema = `{"type":"object","properties":{"name":{"type":"string"},"score":{"type":"number"}},"required":["name","score"]}`

// branchingWorkflow: s -> t -> c; c true -> hi, false -> lo; hi,lo -> e.
// Node o is a legal unconnected orphan that must be pruned.
func branchingWorkflow() agent.WorkflowDefinition {
	return agent.WorkflowDefinition{
		Name: "triage", Version: "v1", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(wfSchema),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "t", Type: agent.WorkflowNodeTool, Tool: "echo", Inputs: map[string]agent.WorkflowValue{"q": ref("s", "name")}},
			{ID: "c", Type: agent.WorkflowNodeCondition, Condition: &agent.WorkflowCondition{Op: "gt", Left: ref("s", "score"), Right: lit("50")}},
			{ID: "hi", Type: agent.WorkflowNodeModel, Model: "m1", Prompt: "high {{name}} {{ echo }}", Inputs: map[string]agent.WorkflowValue{"name": ref("s", "name"), "echo": ref("t", "result")}},
			{ID: "lo", Type: agent.WorkflowNodeTool, Tool: "shout", Inputs: map[string]agent.WorkflowValue{"text": ref("s", "name")}},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"name": ref("s", "name"), "echo": ref("t", "result"), "kind": lit(`"done"`)}},
			{ID: "o", Type: agent.WorkflowNodeLiteral, Inputs: map[string]agent.WorkflowValue{"x": lit("1")}},
		},
		Edges: []agent.WorkflowEdge{
			{From: "s", To: "t"}, {From: "t", To: "c"},
			{From: "c", To: "hi", Port: "true"}, {From: "c", To: "lo", Port: "false"},
			{From: "hi", To: "e"}, {From: "lo", To: "e"},
		},
	}
}

func wfBindings() agent.WorkflowBindings {
	return agent.WorkflowBindings{
		Models: map[string]bool{"m1": true},
		Tools:  map[string]json.RawMessage{"echo": json.RawMessage(`{"type":"object"}`), "shout": json.RawMessage(`{"type":"object"}`)},
	}
}

func subWorkflow(t *testing.T) *agent.CompiledWorkflow {
	t.Helper()
	sub, err := agent.CompileWorkflow(agent.WorkflowDefinition{
		Name: "sub", Version: "v1", Source: "test", FormatVersion: agent.WorkflowFormatV1,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"]}`),
		Nodes: []agent.WorkflowNode{
			{ID: "s", Type: agent.WorkflowNodeStart},
			{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"answer": ref("s", "topic")}},
		},
		Edges: []agent.WorkflowEdge{{From: "s", To: "e"}},
	}, agent.WorkflowBindings{})
	if err != nil {
		t.Fatalf("sub workflow: %v", err)
	}
	return sub
}

func node(def *agent.WorkflowDefinition, id string) *agent.WorkflowNode {
	for i := range def.Nodes {
		if def.Nodes[i].ID == id {
			return &def.Nodes[i]
		}
	}
	panic("fixture node missing: " + id)
}

func TestCompileWorkflowValid(t *testing.T) {
	def := branchingWorkflow()
	c, err := agent.CompileWorkflow(def, wfBindings())
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !reflect.DeepEqual(c.Pruned, []string{"o"}) {
		t.Fatalf("pruned = %v", c.Pruned)
	}
	if !reflect.DeepEqual(c.Order, []string{"s", "t", "c", "hi", "lo", "e"}) {
		t.Fatalf("order = %v", c.Order)
	}
	if len(c.Hash) != 64 || string(c.InputSchema) != wfSchema || !c.Valid() {
		t.Fatalf("hash/schema not populated")
	}
	again, err := agent.CompileWorkflow(branchingWorkflow(), wfBindings())
	if err != nil || again.Hash != c.Hash {
		t.Fatal("hash is not deterministic")
	}
	// The compiled definition is independent of the caller's value.
	def.Nodes[0].ID = "mutated"
	if c.Definition.Nodes[0].ID != "s" {
		t.Fatal("compiled definition aliases caller nodes")
	}
	changed := branchingWorkflow()
	node(&changed, "hi").Prompt = "other {{name}}"
	other, err := agent.CompileWorkflow(changed, wfBindings())
	if err != nil || other.Hash == c.Hash {
		t.Fatal("hash does not cover definition content")
	}
}

func TestCompileWorkflowRejections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*agent.WorkflowDefinition, *agent.WorkflowBindings)
		want   string
	}{
		{"missing name", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.Name = "" }, "workflow name"},
		{"missing source", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.Source = "" }, "workflow source"},
		{"bad format", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.FormatVersion = "coze/v0" }, "formatVersion"},
		{"non-object schema", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.InputSchema = json.RawMessage(`{"type":"string"}`)
		}, "inputSchema"},
		{"invalid schema", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.InputSchema = json.RawMessage(`{"type":"object","properties":{"a":{"type":7}}}`)
		}, "inputSchema"},
		{"unknown type unconnected", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "n3", Type: "code"})
		}, `workflow node "n3": unknown type`},
		{"extra field", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "o").Extra = map[string]json.RawMessage{"batch": json.RawMessage(`true`)}
		}, `workflow node "o": unsupported field "batch"`},
		{"field not for type", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { node(d, "lo").Model = "m1" }, `workflow node "lo": unsupported field "model"`},
		{"model missing prompt", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { node(d, "hi").Prompt = "" }, `workflow node "hi": prompt is required`},
		{"prompt unknown placeholder", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "hi").Prompt = "{{missing}}"
		}, `workflow node "hi": prompt placeholder "missing"`},
		{"literal with ref", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "o").Inputs["y"] = ref("s", "name")
		}, `workflow node "o": literal input "y"`},
		{"invalid literal", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { node(d, "o").Inputs["x"] = lit("{") }, `workflow node "o": input "x"`},
		{"bad condition op", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { node(d, "c").Condition.Op = "regex" }, `workflow node "c": condition op`},
		{"duplicate id", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "t", Type: agent.WorkflowNodeLiteral, Inputs: map[string]agent.WorkflowValue{"x": lit("1")}})
		}, `workflow node "t": duplicate id`},
		{"empty id", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{Type: agent.WorkflowNodeLiteral})
		}, "workflow node 7: id is required"},
		{"dangling edge", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Edges = append(d.Edges, agent.WorkflowEdge{From: "o", To: "ghost"})
		}, `workflow edge 6: unknown node "ghost"`},
		{"dangling ref", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "lo").Inputs["text"] = ref("ghost", "x")
		}, `workflow node "lo": input "text" references unknown node "ghost"`},
		{"bad port", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.Edges[3].Port = "maybe" }, "workflow edge 3: port"},
		{"port on plain edge", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.Edges[0].Port = "true" }, "workflow edge 0: port"},
		{"duplicate port", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) { d.Edges[3].Port = "true" }, `workflow node "c": condition requires`},
		{"two starts", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "s2", Type: agent.WorkflowNodeStart})
		}, "exactly one start"},
		{"cycle", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Edges = append(d.Edges, agent.WorkflowEdge{From: "hi", To: "t"})
		}, "cycle"},
		{"missing model binding", func(d *agent.WorkflowDefinition, b *agent.WorkflowBindings) { b.Models = nil }, `workflow node "hi": model "m1" is not bound`},
		{"missing tool binding on orphan", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "x", Type: agent.WorkflowNodeTool, Tool: "plugin"})
		}, `workflow node "x": tool "plugin" is not bound`},
		{"missing subflow binding", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "x", Type: agent.WorkflowNodeSubflow, Subflow: "sub@v1"})
		}, `workflow node "x": subflow "sub@v1" is not bound`},
		{"subflow recursion", func(d *agent.WorkflowDefinition, b *agent.WorkflowBindings) {
			d.Nodes = append(d.Nodes, agent.WorkflowNode{ID: "x", Type: agent.WorkflowNodeSubflow, Subflow: "triage@v1"})
			b.Subflows = map[string]*agent.CompiledWorkflow{"triage@v1": {}}
		}, `workflow node "x": subflow "triage@v1" is recursive`},
		{"ref to other branch", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "e").Inputs["answer"] = ref("hi", "text")
		}, `workflow node "e": input "answer" reads node "hi"`},
		{"ref to pruned orphan", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "e").Inputs["x"] = ref("o", "x")
		}, `workflow node "e": input "x" references pruned node "o"`},
		{"ref to unknown field", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			node(d, "lo").Inputs["text"] = ref("s", "nickname")
		}, `workflow node "lo": input "text" references unknown field "nickname"`},
		{"end unreachable", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Edges = d.Edges[:4]
		}, "end is not reachable"},
		{"dead end", func(d *agent.WorkflowDefinition, _ *agent.WorkflowBindings) {
			d.Edges = append(d.Edges, agent.WorkflowEdge{From: "t", To: "o"})
		}, `workflow node "o": cannot reach end`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, b := branchingWorkflow(), wfBindings()
			tc.mutate(&def, &b)
			c, err := agent.CompileWorkflow(def, b)
			pe, ok := product.AsError(err)
			if c != nil || !ok || pe.Code != product.CodeInvalidArgument {
				t.Fatalf("want invalid_argument, got %v", err)
			}
			if !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("message %q does not contain %q", pe.Message, tc.want)
			}
		})
	}
}

func TestCompileWorkflowSubflow(t *testing.T) {
	sub := subWorkflow(t)
	build := func(inputs map[string]agent.WorkflowValue, endRef agent.WorkflowValue) agent.WorkflowDefinition {
		return agent.WorkflowDefinition{
			Name: "outer", Version: "v1", Source: "test", FormatVersion: agent.WorkflowFormatV1,
			InputSchema: json.RawMessage(wfSchema),
			Nodes: []agent.WorkflowNode{
				{ID: "s", Type: agent.WorkflowNodeStart},
				{ID: "x", Type: agent.WorkflowNodeSubflow, Subflow: "sub@v1", Inputs: inputs},
				{ID: "e", Type: agent.WorkflowNodeEnd, Inputs: map[string]agent.WorkflowValue{"out": endRef}},
			},
			Edges: []agent.WorkflowEdge{{From: "s", To: "x"}, {From: "x", To: "e"}},
		}
	}
	b := agent.WorkflowBindings{Subflows: map[string]*agent.CompiledWorkflow{"sub@v1": sub}}
	c, err := agent.CompileWorkflow(build(map[string]agent.WorkflowValue{"topic": ref("s", "name")}, ref("x", "answer")), b)
	if err != nil || c.Subflows["sub@v1"] != sub {
		t.Fatalf("valid subflow rejected: %v", err)
	}
	for name, def := range map[string]agent.WorkflowDefinition{
		"missing required sub input": build(nil, ref("x", "answer")),
		"unknown sub output":         build(map[string]agent.WorkflowValue{"topic": ref("s", "name")}, ref("x", "nope")),
	} {
		if _, err := agent.CompileWorkflow(def, b); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A bound subflow that itself embeds the outer workflow is recursive.
	inner := subWorkflow(t)
	inner.Subflows = map[string]*agent.CompiledWorkflow{"outer@v1": sub}
	b.Subflows["sub@v1"] = inner
	_, err = agent.CompileWorkflow(build(map[string]agent.WorkflowValue{"topic": ref("s", "name")}, ref("x", "answer")), b)
	if pe, ok := product.AsError(err); !ok || !strings.Contains(pe.Message, "recursive") {
		t.Fatalf("indirect recursion accepted: %v", err)
	}
}

func TestValidateWorkflowInput(t *testing.T) {
	c, err := agent.CompileWorkflow(branchingWorkflow(), wfBindings())
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.ValidateWorkflowInput(c, json.RawMessage(`{"name":"a","score":1}`)); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	for input, want := range map[string]string{
		`{"name":"a"}`:              `missing required field "score"`,
		`{"name":"a","score":"hi"}`: "does not match",
		`{"name":"a","score":1} {}`: "not valid JSON",
		`[]`:                        "does not match",
	} {
		err := agent.ValidateWorkflowInput(c, json.RawMessage(input))
		pe, ok := product.AsError(err)
		if !ok || pe.Code != product.CodeInvalidArgument || !strings.Contains(pe.Message, want) {
			t.Fatalf("input %s: got %v, want %q", input, err, want)
		}
	}
	if err := agent.ValidateWorkflowInput(&agent.CompiledWorkflow{}, json.RawMessage(`{}`)); err == nil {
		t.Fatal("uncompiled workflow accepted input")
	}
}
