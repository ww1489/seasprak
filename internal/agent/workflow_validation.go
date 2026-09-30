package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	product "github.com/ww1489/seasprak/internal/errors"
)

// WorkflowBindings are the trusted local resources a definition may name.
// Nothing outside these maps is looked up or enabled by a definition.
type WorkflowBindings struct {
	Models map[string]bool
	// Tools maps a tool name to the JSON schema of its arguments.
	Tools map[string]json.RawMessage
	// Subflows maps "name@version" to an already compiled workflow.
	Subflows map[string]*CompiledWorkflow
}

var (
	workflowPlaceholder = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)
	workflowNodeTypes   = map[string]bool{WorkflowNodeStart: true, WorkflowNodeEnd: true, WorkflowNodeLiteral: true, WorkflowNodeModel: true, WorkflowNodeCondition: true, WorkflowNodeTool: true, WorkflowNodeSubflow: true}
	workflowOps         = map[string]bool{"eq": true, "ne": true, "gt": true, "lt": true, "truthy": true}
	workflowOutputTypes = map[string]bool{"string": true, "number": true, "boolean": true, "object": true, "array": true}
	// workflowNodeFields lists the execution fields each node type may carry.
	workflowNodeFields = map[string]map[string]bool{
		WorkflowNodeStart:     {},
		WorkflowNodeEnd:       {"inputs": true},
		WorkflowNodeLiteral:   {"inputs": true},
		WorkflowNodeModel:     {"inputs": true, "outputs": true, "model": true, "prompt": true},
		WorkflowNodeTool:      {"inputs": true, "outputs": true, "tool": true},
		WorkflowNodeCondition: {"condition": true},
		WorkflowNodeSubflow:   {"inputs": true, "subflow": true},
	}
)

func workflowInvalid(format string, args ...any) error {
	return product.Errorf(product.CodeInvalidArgument, format, args...)
}

type offlineWorkflowLoader struct{}

func (offlineWorkflowLoader) Load(string) (any, error) {
	return nil, product.NewError(product.CodeInvalidArgument, "unregistered schema reference")
}

// compileWorkflowSchema compiles one self-contained Draft 2020-12 schema.
func compileWorkflowSchema(raw json.RawMessage) (*jsonschema.Schema, any, error) {
	if !json.Valid(raw) {
		return nil, nil, fmt.Errorf("invalid json")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(offlineWorkflowLoader{})
	if err := compiler.AddResource("file:///workflow-input.schema.json", doc); err != nil {
		return nil, nil, err
	}
	schema, err := compiler.Compile("file:///workflow-input.schema.json")
	if err != nil {
		return nil, nil, err
	}
	return schema, doc, nil
}

// schemaProperties returns the declared top-level properties and required
// names of an object schema document.
func schemaProperties(doc any) (props map[string]bool, required []string) {
	obj, _ := doc.(map[string]any)
	props = map[string]bool{}
	if p, ok := obj["properties"].(map[string]any); ok {
		for name := range p {
			props[name] = true
		}
	}
	if r, ok := obj["required"].([]any); ok {
		for _, name := range r {
			if s, ok := name.(string); ok {
				required = append(required, s)
			}
		}
	}
	return props, required
}

// CompileWorkflow validates def against trusted local bindings and returns
// an immutable compiled value. Every raw node and edge, including
// unconnected ones, is validated before legal orphans are pruned; pruning
// never turns a reference into an empty value. All failures are
// invalid_argument errors that name the node, edge or field.
func CompileWorkflow(def WorkflowDefinition, bindings WorkflowBindings) (*CompiledWorkflow, error) {
	schema, schemaDoc, err := checkWorkflowHeader(def)
	if err != nil {
		return nil, err
	}
	nodes, err := checkWorkflowNodes(def)
	if err != nil {
		return nil, err
	}
	if err := checkWorkflowEdges(def, nodes); err != nil {
		return nil, err
	}
	outputs, err := checkWorkflowBindings(def, nodes, bindings, schemaDoc)
	if err != nil {
		return nil, err
	}
	kept, pruned, err := pruneWorkflow(def, nodes)
	if err != nil {
		return nil, err
	}
	order, err := checkWorkflowTopology(def, nodes, kept, outputs)
	if err != nil {
		return nil, err
	}
	// All raw JSON is valid now, so the detached copy cannot fail on content.
	def, canonical, err := copyWorkflowDefinition(def)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	c := &CompiledWorkflow{
		Definition:  def,
		Hash:        hex.EncodeToString(sum[:]),
		InputSchema: append(json.RawMessage(nil), def.InputSchema...),
		Pruned:      pruned,
		Order:       order,
		inputSchema: schema,
	}
	for _, n := range def.Nodes {
		if kept[n.ID] && n.Type == WorkflowNodeSubflow {
			if c.Subflows == nil {
				c.Subflows = map[string]*CompiledWorkflow{}
			}
			c.Subflows[n.Subflow] = bindings.Subflows[n.Subflow]
		}
	}
	return c, nil
}

// copyWorkflowDefinition detaches def from caller-owned slices and maps and
// returns canonical JSON (sorted keys, compact) used for the hash.
func copyWorkflowDefinition(def WorkflowDefinition) (WorkflowDefinition, []byte, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return WorkflowDefinition{}, nil, workflowInvalid("workflow definition contains invalid JSON")
	}
	var copied WorkflowDefinition
	if err := json.Unmarshal(raw, &copied); err != nil {
		return WorkflowDefinition{}, nil, workflowInvalid("workflow definition contains invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return WorkflowDefinition{}, nil, workflowInvalid("workflow definition contains invalid JSON")
	}
	canonical, err := json.Marshal(generic)
	if err != nil {
		return WorkflowDefinition{}, nil, workflowInvalid("workflow definition contains invalid JSON")
	}
	return copied, canonical, nil
}

// workflowNodeRefs lists every value a node reads, labelled for diagnostics,
// in a deterministic order.
func workflowNodeRefs(n WorkflowNode) (labels []string, values []WorkflowValue) {
	keys := make([]string, 0, len(n.Inputs))
	for k := range n.Inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		labels = append(labels, fmt.Sprintf("input %q", k))
		values = append(values, n.Inputs[k])
	}
	if n.Condition != nil {
		labels = append(labels, "condition left")
		values = append(values, n.Condition.Left)
		if n.Condition.Op != "truthy" {
			labels = append(labels, "condition right")
			values = append(values, n.Condition.Right)
		}
	}
	return labels, values
}

func checkWorkflowValue(nodeID, label string, v WorkflowValue, ids map[string]int) error {
	hasLiteral, hasRef := len(v.Literal) > 0, v.Ref != nil
	if hasLiteral == hasRef {
		return workflowInvalid("workflow node %q: %s must be exactly one literal or ref", nodeID, label)
	}
	if hasLiteral {
		if !json.Valid(v.Literal) {
			return workflowInvalid("workflow node %q: %s literal is invalid JSON", nodeID, label)
		}
		return nil
	}
	if v.Ref.Node == "" || v.Ref.Field == "" {
		return workflowInvalid("workflow node %q: %s ref requires node and field", nodeID, label)
	}
	if _, ok := ids[v.Ref.Node]; !ok {
		return workflowInvalid("workflow node %q: %s references unknown node %q", nodeID, label, v.Ref.Node)
	}
	if v.Ref.Node == nodeID {
		return workflowInvalid("workflow node %q: %s references its own node", nodeID, label)
	}
	return nil
}

// checkWorkflowNodes validates every raw node, connected or not, and returns
// the definition index of each node ID.
func checkWorkflowNodes(def WorkflowDefinition) (map[string]int, error) {
	ids := make(map[string]int, len(def.Nodes))
	for i, n := range def.Nodes {
		if n.ID == "" {
			return nil, workflowInvalid("workflow node %d: id is required", i)
		}
		if _, dup := ids[n.ID]; dup {
			return nil, workflowInvalid("workflow node %q: duplicate id", n.ID)
		}
		ids[n.ID] = i
	}
	for _, n := range def.Nodes {
		if !workflowNodeTypes[n.Type] {
			return nil, workflowInvalid("workflow node %q: unknown type %q", n.ID, n.Type)
		}
		if len(n.Extra) > 0 {
			extra := make([]string, 0, len(n.Extra))
			for k := range n.Extra {
				extra = append(extra, k)
			}
			sort.Strings(extra)
			return nil, workflowInvalid("workflow node %q: unsupported field %q", n.ID, extra[0])
		}
		present := []struct {
			name string
			set  bool
		}{
			{"inputs", len(n.Inputs) > 0}, {"outputs", len(n.Outputs) > 0}, {"model", n.Model != ""}, {"prompt", n.Prompt != ""},
			{"tool", n.Tool != ""}, {"condition", n.Condition != nil}, {"subflow", n.Subflow != ""},
		}
		for _, f := range present {
			if f.set && !workflowNodeFields[n.Type][f.name] {
				return nil, workflowInvalid("workflow node %q: unsupported field %q for type %s", n.ID, f.name, n.Type)
			}
		}
		if err := checkWorkflowNodeFields(n); err != nil {
			return nil, err
		}
		labels, values := workflowNodeRefs(n)
		for i := range values {
			if err := checkWorkflowValue(n.ID, labels[i], values[i], ids); err != nil {
				return nil, err
			}
		}
	}
	return ids, nil
}

func checkWorkflowNodeFields(n WorkflowNode) error {
	switch n.Type {
	case WorkflowNodeModel:
		if n.Model == "" {
			return workflowInvalid("workflow node %q: model is required", n.ID)
		}
		if n.Prompt == "" {
			return workflowInvalid("workflow node %q: prompt is required", n.ID)
		}
		for _, m := range workflowPlaceholder.FindAllStringSubmatch(n.Prompt, -1) {
			if _, ok := n.Inputs[m[1]]; !ok {
				return workflowInvalid("workflow node %q: prompt placeholder %q is not an input", n.ID, m[1])
			}
		}
	case WorkflowNodeTool:
		if n.Tool == "" {
			return workflowInvalid("workflow node %q: tool is required", n.ID)
		}
	case WorkflowNodeSubflow:
		if n.Subflow == "" {
			return workflowInvalid("workflow node %q: subflow is required", n.ID)
		}
	case WorkflowNodeCondition:
		if n.Condition == nil {
			return workflowInvalid("workflow node %q: condition is required", n.ID)
		}
		if !workflowOps[n.Condition.Op] {
			return workflowInvalid("workflow node %q: condition op %q is unsupported", n.ID, n.Condition.Op)
		}
		right := n.Condition.Right
		if n.Condition.Op == "truthy" && (len(right.Literal) > 0 || right.Ref != nil) {
			return workflowInvalid("workflow node %q: condition right is not allowed for truthy", n.ID)
		}
	case WorkflowNodeLiteral:
		for k, v := range n.Inputs {
			if v.Ref != nil {
				return workflowInvalid("workflow node %q: literal input %q must not be a ref", n.ID, k)
			}
		}
	}
	// Model and tool executors return one string, so at most one declared
	// output of type string is meaningful.
	if len(n.Outputs) > 1 {
		return workflowInvalid("workflow node %q: at most one output is supported", n.ID)
	}
	for field, typ := range n.Outputs {
		if field == "" || typ != "string" {
			return workflowInvalid("workflow node %q: output %q must be of type string", n.ID, field)
		}
	}
	return nil
}

func checkWorkflowEdges(def WorkflowDefinition, ids map[string]int) error {
	seen := map[[2]string]bool{}
	ports := map[string]map[string]int{}
	for i, e := range def.Edges {
		for _, end := range []string{e.From, e.To} {
			if _, ok := ids[end]; !ok {
				return workflowInvalid("workflow edge %d: unknown node %q", i, end)
			}
		}
		from, to := def.Nodes[ids[e.From]], def.Nodes[ids[e.To]]
		if to.Type == WorkflowNodeStart {
			return workflowInvalid("workflow edge %d: start cannot have incoming edges", i)
		}
		if from.Type == WorkflowNodeEnd {
			return workflowInvalid("workflow edge %d: end cannot have outgoing edges", i)
		}
		if seen[[2]string{e.From, e.To}] {
			return workflowInvalid("workflow edge %d: duplicate edge", i)
		}
		seen[[2]string{e.From, e.To}] = true
		if from.Type == WorkflowNodeCondition {
			if e.Port != "true" && e.Port != "false" {
				return workflowInvalid("workflow edge %d: port %q must be \"true\" or \"false\"", i, e.Port)
			}
			if ports[e.From] == nil {
				ports[e.From] = map[string]int{}
			}
			ports[e.From][e.Port]++
		} else if e.Port != "" {
			return workflowInvalid("workflow edge %d: port %q is only allowed from condition nodes", i, e.Port)
		}
	}
	for _, n := range def.Nodes {
		if n.Type == WorkflowNodeCondition && (ports[n.ID]["true"] != 1 || ports[n.ID]["false"] != 1) {
			return workflowInvalid("workflow node %q: condition requires exactly one \"true\" and one \"false\" edge", n.ID)
		}
	}
	return nil
}

// workflowRecursive reports whether sub, or any subflow it binds, is the
// workflow identified by self.
func workflowRecursive(self string, sub *CompiledWorkflow, depth int) bool {
	if sub == nil || depth > 64 {
		return depth > 64
	}
	if sub.Definition.Name+"@"+sub.Definition.Version == self {
		return true
	}
	for key, inner := range sub.Subflows {
		if key == self || workflowRecursive(self, inner, depth+1) {
			return true
		}
	}
	return false
}

// checkWorkflowBindings resolves every raw node's model, tool and subflow to
// a trusted local binding and returns the output fields each node declares.
func checkWorkflowBindings(def WorkflowDefinition, ids map[string]int, b WorkflowBindings, schemaDoc any) (map[string]map[string]bool, error) {
	self := def.Name + "@" + def.Version
	outputs := make(map[string]map[string]bool, len(def.Nodes))
	for _, n := range def.Nodes {
		fields := map[string]bool{}
		switch n.Type {
		case WorkflowNodeStart:
			fields, _ = schemaProperties(schemaDoc)
		case WorkflowNodeLiteral, WorkflowNodeEnd:
			for k := range n.Inputs {
				fields[k] = true
			}
		case WorkflowNodeModel:
			if !b.Models[n.Model] {
				return nil, workflowInvalid("workflow node %q: model %q is not bound", n.ID, n.Model)
			}
			fields[n.OutputField()] = true
		case WorkflowNodeTool:
			schema, ok := b.Tools[n.Tool]
			if !ok {
				return nil, workflowInvalid("workflow node %q: tool %q is not bound", n.ID, n.Tool)
			}
			_, doc, err := compileWorkflowSchema(schema)
			if err != nil {
				return nil, workflowInvalid("workflow node %q: tool %q schema is invalid", n.ID, n.Tool)
			}
			if err := checkRequiredInputs(n, doc); err != nil {
				return nil, err
			}
			fields[n.OutputField()] = true
		case WorkflowNodeSubflow:
			if n.Subflow == self {
				return nil, workflowInvalid("workflow node %q: subflow %q is recursive", n.ID, n.Subflow)
			}
			sub, ok := b.Subflows[n.Subflow]
			if !ok || sub == nil {
				return nil, workflowInvalid("workflow node %q: subflow %q is not bound", n.ID, n.Subflow)
			}
			if workflowRecursive(self, sub, 0) {
				return nil, workflowInvalid("workflow node %q: subflow %q is recursive", n.ID, n.Subflow)
			}
			if !sub.Valid() || sub.Definition.Name+"@"+sub.Definition.Version != n.Subflow {
				return nil, workflowInvalid("workflow node %q: subflow %q binding is not a compiled workflow of that identity", n.ID, n.Subflow)
			}
			_, doc, err := compileWorkflowSchema(sub.InputSchema)
			if err != nil {
				return nil, workflowInvalid("workflow node %q: subflow %q schema is invalid", n.ID, n.Subflow)
			}
			if err := checkRequiredInputs(n, doc); err != nil {
				return nil, err
			}
			for _, sn := range sub.Definition.Nodes {
				if sn.Type == WorkflowNodeEnd {
					for k := range sn.Inputs {
						fields[k] = true
					}
				}
			}
		}
		outputs[n.ID] = fields
	}
	return outputs, nil
}

func checkRequiredInputs(n WorkflowNode, schemaDoc any) error {
	_, required := schemaProperties(schemaDoc)
	for _, name := range required {
		if _, ok := n.Inputs[name]; !ok {
			return workflowInvalid("workflow node %q: required input %q is not provided", n.ID, name)
		}
	}
	return nil
}

// pruneWorkflow removes nodes that start cannot reach. It runs only after
// every raw node passed validation, and rejects kept references to pruned nodes.
func pruneWorkflow(def WorkflowDefinition, ids map[string]int) (kept map[string]bool, pruned []string, err error) {
	var start, end []string
	for _, n := range def.Nodes {
		switch n.Type {
		case WorkflowNodeStart:
			start = append(start, n.ID)
		case WorkflowNodeEnd:
			end = append(end, n.ID)
		}
	}
	if len(start) != 1 || len(end) != 1 {
		return nil, nil, workflowInvalid("workflow requires exactly one start and one end node (found %d and %d)", len(start), len(end))
	}
	next := map[string][]string{}
	for _, e := range def.Edges {
		next[e.From] = append(next[e.From], e.To)
	}
	kept = map[string]bool{start[0]: true}
	queue := []string{start[0]}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, to := range next[id] {
			if !kept[to] {
				kept[to] = true
				queue = append(queue, to)
			}
		}
	}
	if !kept[end[0]] {
		return nil, nil, workflowInvalid("workflow end is not reachable from start")
	}
	for _, n := range def.Nodes {
		if !kept[n.ID] {
			pruned = append(pruned, n.ID)
		}
	}
	for _, n := range def.Nodes {
		if !kept[n.ID] {
			continue
		}
		labels, values := workflowNodeRefs(n)
		for i, v := range values {
			if v.Ref != nil && !kept[v.Ref.Node] {
				return nil, nil, workflowInvalid("workflow node %q: %s references pruned node %q", n.ID, labels[i], v.Ref.Node)
			}
		}
	}
	return kept, pruned, nil
}

// checkWorkflowTopology orders the kept graph and proves every reference is
// produced on every path that reaches the reading node.
func checkWorkflowTopology(def WorkflowDefinition, ids map[string]int, kept map[string]bool, outputs map[string]map[string]bool) ([]string, error) {
	indegree := map[string]int{}
	next, prev := map[string][]string{}, map[string][]string{}
	for _, e := range def.Edges {
		if kept[e.From] && kept[e.To] {
			next[e.From] = append(next[e.From], e.To)
			prev[e.To] = append(prev[e.To], e.From)
			indegree[e.To]++
		}
	}
	var order []string
	done := map[string]bool{}
	for len(order) < len(kept) {
		progressed := false
		// Ready nodes are taken in definition order for a deterministic Order.
		for _, n := range def.Nodes {
			if kept[n.ID] && !done[n.ID] && indegree[n.ID] == 0 {
				done[n.ID] = true
				order = append(order, n.ID)
				for _, to := range next[n.ID] {
					indegree[to]--
				}
				progressed = true
				break
			}
		}
		if !progressed {
			return nil, workflowInvalid("workflow graph contains a cycle")
		}
	}
	// Every kept node must lead to end; dead ends would run without effect on the result.
	reachesEnd := map[string]bool{}
	for i := len(order) - 1; i >= 0; i-- {
		id := order[i]
		n := def.Nodes[ids[id]]
		reachesEnd[id] = n.Type == WorkflowNodeEnd
		for _, to := range next[id] {
			reachesEnd[id] = reachesEnd[id] || reachesEnd[to]
		}
	}
	for _, id := range order {
		if !reachesEnd[id] {
			return nil, workflowInvalid("workflow node %q: cannot reach end", id)
		}
	}
	// dom[n] holds the nodes executed on every path from start to n.
	dom := map[string]map[string]bool{}
	for _, id := range order {
		var d map[string]bool
		for _, p := range prev[id] {
			if d == nil {
				d = map[string]bool{}
				for k := range dom[p] {
					d[k] = true
				}
				continue
			}
			for k := range d {
				if !dom[p][k] {
					delete(d, k)
				}
			}
		}
		if d == nil {
			d = map[string]bool{}
		}
		d[id] = true
		dom[id] = d
	}
	for _, id := range order {
		labels, values := workflowNodeRefs(def.Nodes[ids[id]])
		for i, v := range values {
			if v.Ref == nil {
				continue
			}
			if !dom[id][v.Ref.Node] {
				return nil, workflowInvalid("workflow node %q: %s reads node %q, which does not run on every path to it", id, labels[i], v.Ref.Node)
			}
			if !outputs[v.Ref.Node][v.Ref.Field] {
				return nil, workflowInvalid("workflow node %q: %s references unknown field %q of node %q", id, labels[i], v.Ref.Field, v.Ref.Node)
			}
		}
	}
	return order, nil
}

// ValidateWorkflowInput validates input against the compiled input schema.
// Missing required parameters are rejected; nothing is guessed or defaulted.
func ValidateWorkflowInput(c *CompiledWorkflow, input json.RawMessage) error {
	if !c.Valid() {
		return workflowInvalid("workflow is not compiled")
	}
	if !json.Valid(input) {
		return workflowInvalid("workflow input is not valid JSON")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(input))
	if err != nil {
		return workflowInvalid("workflow input is not valid JSON")
	}
	if err := c.inputSchema.Validate(doc); err != nil {
		if missing := workflowMissingRequired(err); missing != "" {
			return workflowInvalid("workflow input: missing required field %q", missing)
		}
		return workflowInvalid("workflow input does not match the input schema")
	}
	return nil
}

func workflowMissingRequired(err error) string {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return ""
	}
	if r, ok := ve.ErrorKind.(*kind.Required); ok && len(ve.InstanceLocation) == 0 && len(r.Missing) > 0 {
		return r.Missing[0]
	}
	for _, cause := range ve.Causes {
		if m := workflowMissingRequired(cause); m != "" {
			return m
		}
	}
	return ""
}

func checkWorkflowHeader(def WorkflowDefinition) (*jsonschema.Schema, any, error) {
	for _, f := range []struct{ name, value string }{{"name", def.Name}, {"version", def.Version}, {"source", def.Source}, {"formatVersion", def.FormatVersion}} {
		if strings.TrimSpace(f.value) == "" {
			return nil, nil, workflowInvalid("workflow %s is required", f.name)
		}
	}
	if def.FormatVersion != WorkflowFormatV1 {
		return nil, nil, workflowInvalid("workflow formatVersion %q is unsupported", def.FormatVersion)
	}
	schema, doc, err := compileWorkflowSchema(def.InputSchema)
	if err != nil {
		return nil, nil, workflowInvalid("workflow inputSchema is invalid")
	}
	if obj, ok := doc.(map[string]any); !ok || obj["type"] != "object" {
		return nil, nil, workflowInvalid("workflow inputSchema must be an object schema")
	}
	return schema, doc, nil
}
