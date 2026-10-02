package workflowagent

import (
	"context"
	"encoding/json"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

// WorkflowDefinition is the unified declarative workflow format. It holds no
// Go functions or compiled runnables; bindings name trusted local resources.
type WorkflowDefinition struct {
	Name          string          `json:"name"`
	Version       string          `json:"version"`
	Description   string          `json:"description,omitempty"`
	Source        string          `json:"source"`
	FormatVersion string          `json:"formatVersion"`
	InputSchema   json.RawMessage `json:"inputSchema"`
	Nodes         []WorkflowNode  `json:"nodes"`
	Edges         []WorkflowEdge  `json:"edges"`
	Resumable     bool            `json:"resumable,omitempty"`
}

// WorkflowFormatV1 is the only accepted unified definition format.
const WorkflowFormatV1 = "seasprak-workflow/v1"

// WorkflowModelBinding is the trusted startup model binding name.
const WorkflowModelBinding = "default"

// Supported node types for the first static subset.
const (
	WorkflowNodeStart     = "start"
	WorkflowNodeEnd       = "end"
	WorkflowNodeLiteral   = "literal"
	WorkflowNodeModel     = "model"
	WorkflowNodeCondition = "condition"
	WorkflowNodeTool      = "tool"
	WorkflowNodeSubflow   = "subflow"
)

// WorkflowNode is one node. Inputs map a field name to a literal or to an
// earlier node output. Extra carries unknown source fields so validation can
// reject semantics this subset does not support.
type WorkflowNode struct {
	ID     string                   `json:"id"`
	Type   string                   `json:"type"`
	Inputs map[string]WorkflowValue `json:"inputs,omitempty"`
	// Outputs declares produced fields and their JSON types (string, number, boolean, object, array).
	Outputs map[string]string `json:"outputs,omitempty"`
	// Model names a trusted model binding; Prompt is a template over inputs.
	Model  string `json:"model,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// Tool names a trusted tool binding; its arguments are the node inputs.
	Tool string `json:"tool,omitempty"`
	// Condition selects the outgoing port "true" or "false".
	Condition *WorkflowCondition `json:"condition,omitempty"`
	// Subflow names a registered workflow as name@version.
	Subflow string                     `json:"subflow,omitempty"`
	Extra   map[string]json.RawMessage `json:"extra,omitempty"`
}

// WorkflowValue is exactly one of a literal or a reference.
type WorkflowValue struct {
	Literal json.RawMessage `json:"literal,omitempty"`
	Ref     *WorkflowRef    `json:"ref,omitempty"`
}

// WorkflowRef names an output field of another node, or of start's input.
type WorkflowRef struct {
	Node  string `json:"node"`
	Field string `json:"field"`
}

// WorkflowCondition compares Left to Right with Op (eq, ne, gt, lt, truthy).
type WorkflowCondition struct {
	Op    string        `json:"op"`
	Left  WorkflowValue `json:"left"`
	Right WorkflowValue `json:"right,omitempty"`
}

// WorkflowEdge connects nodes. Port is required only from condition nodes.
type WorkflowEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Port string `json:"port,omitempty"`
}

// CompiledWorkflow is a validated static workflow definition ready to run.
// BuildWorkflowGraph builds the Eino graph from this value.
type CompiledWorkflow struct {
	Definition  WorkflowDefinition
	Hash        string
	InputSchema json.RawMessage
	// Pruned lists legal orphan nodes removed after raw validation.
	Pruned []string
	// Order is a topological order of the kept nodes.
	Order []string
	// Subflows holds the resolved fixed-version bindings of kept subflow
	// nodes, keyed by name@version.
	Subflows map[string]*CompiledWorkflow

	inputSchema *jsonschema.Schema
}

// OutputField is the single string field produced by a model or tool node:
// its declared output, or "text" for model and "result" for tool nodes.
func (n WorkflowNode) OutputField() string {
	for field := range n.Outputs {
		return field // validation allows exactly one declared output
	}
	if n.Type == WorkflowNodeModel {
		return "text"
	}
	return "result"
}

// Valid reports whether c was produced by CompileWorkflow.
func (c *CompiledWorkflow) Valid() bool { return c != nil && c.inputSchema != nil }

// WorkflowNodeExecutor runs the side-effecting nodes of a compiled workflow
// through trusted local bindings. Implementations own authorization, budget
// and identity; the graph only resolves inputs and routes outputs.
type WorkflowNodeExecutor interface {
	RunModel(ctx context.Context, nodeID, model, prompt string) (string, error)
	RunTool(ctx context.Context, nodeID, tool string, args json.RawMessage) (string, error)
	RunSubflow(ctx context.Context, nodeID string, sub *CompiledWorkflow, input json.RawMessage) (map[string]any, error)
}
