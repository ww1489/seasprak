package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// workflowState carries the outputs of executed nodes, keyed by node ID.
// Each lambda returns a new top-level map so parallel paths never share a
// mutable value; joins union the outputs of the paths that actually ran.
type workflowState map[string]map[string]any

func init() {
	schema.RegisterName[workflowState]("seasprak_workflow_state")
	compose.RegisterValuesMergeFunc(func(states []workflowState) (workflowState, error) {
		merged := workflowState{}
		for _, st := range states {
			for id, out := range st {
				merged[id] = out // a node's outputs are identical on every path carrying them
			}
		}
		return merged, nil
	})
}

// workflowPromptPlaceholder matches the placeholder syntax that
// agent.CompileWorkflow validates.
var workflowPromptPlaceholder = regexp.MustCompile(`\{\{\s*[^{}]*?\s*\}\}`)

func workflowNodeKey(id string) string { return "wf:" + id }

// BuildWorkflowGraph compiles a validated workflow into an Eino graph whose
// side-effecting nodes run through exec. Condition nodes are graph branches;
// the untaken branch is skipped by the framework and never calls exec.
// store is optional; nil disables checkpointing.
func BuildWorkflowGraph(ctx context.Context, c *agent.CompiledWorkflow, exec agent.WorkflowNodeExecutor, store compose.CheckPointStore) (compose.Runnable[map[string]any, map[string]any], error) {
	return buildWorkflowGraph(ctx, c, exec, store, compose.AllPredecessor)
}

// buildWorkflowGraph takes the trigger mode so tests can prove why
// AnyPredecessor is unsafe for joins of unequal-length parallel paths.
func buildWorkflowGraph(ctx context.Context, c *agent.CompiledWorkflow, exec agent.WorkflowNodeExecutor, store compose.CheckPointStore, mode compose.NodeTriggerMode) (compose.Runnable[map[string]any, map[string]any], error) {
	if !c.Valid() {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow is not compiled")
	}
	if exec == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "workflow node executor is required")
	}
	def := c.Definition
	nodes := make(map[string]agent.WorkflowNode, len(def.Nodes))
	for _, n := range def.Nodes {
		nodes[n.ID] = n
	}
	g := compose.NewGraph[map[string]any, map[string]any]()
	for _, id := range c.Order {
		n := nodes[id]
		var err error
		switch n.Type {
		case agent.WorkflowNodeStart:
			err = g.AddLambdaNode(workflowNodeKey(id), compose.InvokableLambda(func(_ context.Context, in map[string]any) (workflowState, error) {
				raw, err := json.Marshal(in)
				if err != nil {
					return nil, product.NewError(product.CodeInvalidArgument, "workflow input is not valid JSON")
				}
				if err := agent.ValidateWorkflowInput(c, raw); err != nil {
					return nil, err
				}
				var out map[string]any
				if err := decodeWorkflowJSON(raw, &out); err != nil {
					return nil, err
				}
				return workflowState{id: out}, nil
			}))
		case agent.WorkflowNodeEnd:
			err = g.AddLambdaNode(workflowNodeKey(id), compose.InvokableLambda(func(_ context.Context, st workflowState) (map[string]any, error) {
				return resolveWorkflowInputs(st, n)
			}))
		case agent.WorkflowNodeCondition:
			err = g.AddLambdaNode(workflowNodeKey(id), compose.InvokableLambda(func(_ context.Context, st workflowState) (workflowState, error) {
				return st, nil
			}))
		default:
			err = g.AddLambdaNode(workflowNodeKey(id), compose.InvokableLambda(func(ctx context.Context, st workflowState) (workflowState, error) {
				out, err := runWorkflowNode(ctx, c, exec, n, st)
				if err != nil {
					return nil, err
				}
				next := make(workflowState, len(st)+1)
				for k, v := range st {
					next[k] = v
				}
				next[id] = out
				return next, nil
			}))
		}
		if err != nil {
			return nil, product.NewError(product.CodeInternal, "workflow graph construction failed")
		}
	}
	if err := addWorkflowEdges(g, c, nodes); err != nil {
		return nil, err
	}
	opts := []compose.GraphCompileOption{
		compose.WithGraphName("workflow:" + def.Name),
		// AllPredecessor (DAG) mode: a join waits for every predecessor that
		// was not skipped by a branch, so it runs exactly once per invocation.
		compose.WithNodeTriggerMode(mode),
	}
	if store != nil {
		opts = append(opts, compose.WithCheckPointStore(store))
	}
	r, err := g.Compile(ctx, opts...)
	if err != nil {
		return nil, product.NewError(product.CodeInternal, "workflow graph compilation failed")
	}
	return r, nil
}

func addWorkflowEdges(g *compose.Graph[map[string]any, map[string]any], c *agent.CompiledWorkflow, nodes map[string]agent.WorkflowNode) error {
	kept := make(map[string]bool, len(c.Order))
	for _, id := range c.Order {
		kept[id] = true
	}
	fail := func() error { return product.NewError(product.CodeInternal, "workflow graph construction failed") }
	ports := map[string]map[string]string{}
	for _, e := range c.Definition.Edges {
		if !kept[e.From] || !kept[e.To] {
			continue
		}
		if nodes[e.From].Type == agent.WorkflowNodeCondition {
			if ports[e.From] == nil {
				ports[e.From] = map[string]string{}
			}
			ports[e.From][e.Port] = workflowNodeKey(e.To)
			continue
		}
		if g.AddEdge(workflowNodeKey(e.From), workflowNodeKey(e.To)) != nil {
			return fail()
		}
	}
	for _, id := range c.Order {
		n := nodes[id]
		switch n.Type {
		case agent.WorkflowNodeStart:
			if g.AddEdge(compose.START, workflowNodeKey(id)) != nil {
				return fail()
			}
		case agent.WorkflowNodeEnd:
			if g.AddEdge(workflowNodeKey(id), compose.END) != nil {
				return fail()
			}
		case agent.WorkflowNodeCondition:
			targets := ports[id]
			branch := compose.NewGraphBranch(func(_ context.Context, st workflowState) (string, error) {
				ok, err := evalWorkflowCondition(st, n)
				if err != nil {
					return "", err
				}
				return targets[fmt.Sprint(ok)], nil
			}, map[string]bool{targets["true"]: true, targets["false"]: true})
			if g.AddBranch(workflowNodeKey(id), branch) != nil {
				return fail()
			}
		}
	}
	return nil
}

func runWorkflowNode(ctx context.Context, c *agent.CompiledWorkflow, exec agent.WorkflowNodeExecutor, n agent.WorkflowNode, st workflowState) (map[string]any, error) {
	in, err := resolveWorkflowInputs(st, n)
	if err != nil {
		return nil, err
	}
	switch n.Type {
	case agent.WorkflowNodeLiteral:
		return in, nil
	case agent.WorkflowNodeModel:
		text, err := exec.RunModel(ctx, n.ID, n.Model, renderWorkflowPrompt(n.Prompt, in))
		if err != nil {
			return nil, err
		}
		return map[string]any{n.OutputField(): text}, nil
	case agent.WorkflowNodeTool:
		args, err := json.Marshal(in)
		if err != nil {
			return nil, product.Errorf(product.CodeInvalidArgument, "workflow node %q: arguments are not valid JSON", n.ID)
		}
		result, err := exec.RunTool(ctx, n.ID, n.Tool, args)
		if err != nil {
			return nil, err
		}
		return map[string]any{n.OutputField(): result}, nil
	case agent.WorkflowNodeSubflow:
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, product.Errorf(product.CodeInvalidArgument, "workflow node %q: subflow input is not valid JSON", n.ID)
		}
		return exec.RunSubflow(ctx, n.ID, c.Subflows[n.Subflow], raw)
	}
	return nil, product.Errorf(product.CodeInternal, "workflow node %q: type is not executable", n.ID)
}

func decodeWorkflowJSON(raw []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return product.NewError(product.CodeInvalidArgument, "workflow value is not valid JSON")
	}
	return nil
}

func resolveWorkflowValue(st workflowState, nodeID string, v agent.WorkflowValue) (any, error) {
	if v.Ref == nil {
		var out any
		return out, decodeWorkflowJSON(v.Literal, &out)
	}
	out, ok := st[v.Ref.Node][v.Ref.Field]
	if !ok {
		return nil, product.Errorf(product.CodeInternal, "workflow node %q: output %s.%s was not produced", nodeID, v.Ref.Node, v.Ref.Field)
	}
	return out, nil
}

func resolveWorkflowInputs(st workflowState, n agent.WorkflowNode) (map[string]any, error) {
	out := make(map[string]any, len(n.Inputs))
	for k, v := range n.Inputs {
		val, err := resolveWorkflowValue(st, n.ID, v)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

func workflowString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// renderWorkflowPrompt replaces {{field}} with the resolved input value;
// validation guarantees every placeholder names an input.
func renderWorkflowPrompt(prompt string, in map[string]any) string {
	return workflowPromptPlaceholder.ReplaceAllStringFunc(prompt, func(m string) string {
		return workflowString(in[strings.TrimSpace(m[2:len(m)-2])])
	})
}

func workflowNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	}
	return 0, false
}

// workflowNormalize converts numbers to float64 so equal values compare equal
// regardless of their JSON spelling.
func workflowNormalize(v any) any {
	switch t := v.(type) {
	case json.Number:
		if f, ok := workflowNumber(t); ok {
			return f
		}
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = workflowNormalize(t[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = workflowNormalize(x)
		}
		return out
	}
	return v
}

func workflowTruthy(v any) bool {
	switch t := workflowNormalize(v).(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

func evalWorkflowCondition(st workflowState, n agent.WorkflowNode) (bool, error) {
	cond := n.Condition
	left, err := resolveWorkflowValue(st, n.ID, cond.Left)
	if err != nil {
		return false, err
	}
	if cond.Op == "truthy" {
		return workflowTruthy(left), nil
	}
	right, err := resolveWorkflowValue(st, n.ID, cond.Right)
	if err != nil {
		return false, err
	}
	switch cond.Op {
	case "eq", "ne":
		eq := reflect.DeepEqual(workflowNormalize(left), workflowNormalize(right))
		return eq == (cond.Op == "eq"), nil
	case "gt", "lt":
		if l, ok := workflowNumber(left); ok {
			if r, ok := workflowNumber(right); ok {
				return (cond.Op == "gt" && l > r) || (cond.Op == "lt" && l < r), nil
			}
		}
		ls, lok := left.(string)
		rs, rok := right.(string)
		if lok && rok {
			return (cond.Op == "gt" && ls > rs) || (cond.Op == "lt" && ls < rs), nil
		}
	}
	return false, product.Errorf(product.CodeInvalidArgument, "workflow node %q: condition operands are not comparable", n.ID)
}
