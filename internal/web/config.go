package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/codeagent"
	"github.com/ww1489/seasprak/internal/llm"
	store "github.com/ww1489/seasprak/internal/storage"
	"github.com/ww1489/seasprak/internal/workflowagent"
)

// Config contains only trusted process-start inputs, never HTTP request fields.
// StateRoot's parent must exist. A missing StateRoot is created privately;
// existing StateRoot permissions are validated, never silently changed.
type Config struct{ Workspace, StateRoot, ConfigPath, Listen string }

// runtimeOptions keeps the ordinary session inventory separate from the
// fixed-version workflow inventory consumed by the later run routes.
type runtimeOptions struct {
	Code      codeagent.Options
	Workflows map[string]workflowagent.WorkflowOptions
}

// The startup file wraps the existing Catalog configuration. NoCredentials is
// explicit; otherwise CredentialRef must be env:NAME. No file credential source,
// uploaded executable, tool backend or plugin is accepted by this bootstrap.
type startupConfig struct {
	Model                 llm.ModelConfig `json:"model"`
	GenerationFingerprint string          `json:"generationFingerprint"`
	// Profile must be set explicitly to "memory" to allow execution without host
	// file/process backends. The default profile stays unavailable for writers.
	Profile string `json:"profile,omitempty"`
	// Tools names built-in definitions to install. Only builtins whose backend
	// is owned by the session itself are allowed; file and process tools need
	// host backends this bootstrap never assembles.
	Tools []string `json:"tools,omitempty"`
	// ApprovalTools is a subset of Tools that requires one-operation approval.
	// It cannot enable a tool or backend by itself.
	ApprovalTools []string `json:"approvalTools,omitempty"`
	// Both inventories are immutable trusted startup declarations. Workflows
	// are independent runs, never ordinary or builtin delegation targets.
	Agents    []startupAgent    `json:"agents,omitempty"`
	Workflows []startupWorkflow `json:"workflows,omitempty"`
}

type startupAgent struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Instruction string   `json:"instruction"`
	Delegable   bool     `json:"delegable,omitempty"`
	Tools       []string `json:"tools,omitempty"`
}

// startupWorkflow accepts only the independent declarative definition.
type startupWorkflow struct {
	workflowagent.WorkflowDefinition
}

// sessionOwnedTools are the built-ins that run without any host backend:
// write_todos persists through the session's own TODO backend.
var sessionOwnedTools = map[string]bool{"write_todos": true}

// builtinTools resolves the configured names to built-in definitions,
// rejecting unknown, host-backed and duplicate names.
func builtinTools(names, approvalNames []string) ([]tools.Definition, error) {
	if len(names) == 0 {
		if len(approvalNames) != 0 {
			return nil, invalid("approvalTools must name configured tools")
		}
		return nil, nil
	}
	defs := map[string]tools.Definition{}
	for _, d := range tools.NewBuiltinDefinitions(tools.BuiltinOptions{}) {
		defs[d.Name] = d
	}
	approvals := map[string]bool{}
	for _, name := range approvalNames {
		if name == "" || approvals[name] {
			return nil, invalid("approvalTools must name distinct configured tools")
		}
		approvals[name] = true
	}
	seen := map[string]bool{}
	out := make([]tools.Definition, 0, len(names))
	for _, name := range names {
		d, ok := defs[name]
		if !ok || !sessionOwnedTools[name] || seen[name] {
			return nil, invalid("tools must name distinct session-owned built-in tools")
		}
		seen[name] = true
		if approvals[name] {
			d.Execution.RequestedGrantRef = "one-operation"
			delete(approvals, name)
		}
		out = append(out, d)
	}
	if len(approvals) != 0 {
		return nil, invalid("approvalTools must name configured tools")
	}
	return out, nil
}

// startupTargets validates only ordinary Code targets before creating state.
func startupTargets(conf startupConfig, defs []tools.Definition) ([]agent.AgentDefinition, error) {
	targets := make([]agent.AgentDefinition, 0, len(conf.Agents))
	for _, a := range conf.Agents {
		if strings.TrimSpace(a.Instruction) == "" {
			return nil, invalid("agent instruction is required")
		}
		targets = append(targets, agent.AgentDefinition{Name: a.Name, Version: a.Version, Description: a.Description, Instruction: a.Instruction, Delegable: a.Delegable, Kind: agent.AgentKindAgent, Tools: append([]string(nil), a.Tools...)})
	}
	if _, err := agent.NewAgentRegistry("", targets); err != nil {
		return nil, err
	}
	available := map[string]bool{}
	for _, def := range defs {
		available[def.Name] = true
	}
	for _, target := range targets {
		if target.Delegable {
			available["delegate_task"] = true
		}
	}
	for _, target := range targets {
		for _, name := range target.Tools {
			if name == "search_tools" || !available[name] {
				return nil, invalid("agent tools must name configured session-owned tools or delegate_task")
			}
		}
	}
	return targets, nil
}

// startupWorkflows statically compiles the separate inventory. Exact versions,
// missing references and cycles are checked before state-root creation.
func startupWorkflows(conf startupConfig, defs []tools.Definition) (map[string]*workflowagent.CompiledWorkflow, error) {
	byID := make(map[string]workflowagent.WorkflowDefinition, len(conf.Workflows))
	for _, w := range conf.Workflows {
		id := w.Name + "@" + w.Version
		if _, duplicate := byID[id]; duplicate {
			return nil, invalid("workflow name and version must be unique")
		}
		byID[id] = w.WorkflowDefinition
	}
	bindings := workflowagent.WorkflowBindings{Models: map[string]bool{workflowagent.WorkflowModelBinding: true}, Tools: map[string]json.RawMessage{}, Subflows: map[string]*workflowagent.CompiledWorkflow{}}
	for _, d := range defs {
		bindings.Tools[d.Name] = append(json.RawMessage(nil), d.Schema...)
	}
	visiting := map[string]bool{}
	var compile func(string) error
	compile = func(id string) error {
		if bindings.Subflows[id] != nil {
			return nil
		}
		w, ok := byID[id]
		if !ok {
			return invalid("workflow subflow is not registered")
		}
		if visiting[id] {
			return invalid("workflow subflow dependency is recursive")
		}
		visiting[id] = true
		defer delete(visiting, id)
		for _, node := range w.Nodes {
			if node.Type == workflowagent.WorkflowNodeSubflow {
				if err := compile(node.Subflow); err != nil {
					return err
				}
			}
		}
		c, err := workflowagent.CompileWorkflow(w, bindings)
		if err != nil {
			return err
		}
		bindings.Subflows[id] = c
		return nil
	}
	for _, w := range conf.Workflows {
		if err := compile(w.Name + "@" + w.Version); err != nil {
			return nil, err
		}
	}
	return bindings.Subflows, nil
}

// startupWorkflowFingerprint freezes the complete trusted model configuration,
// tool versions/declarations and separate definition inventory. It never
// contributes to Code generation and is never emitted through HTTP DTOs.
func startupWorkflowFingerprint(conf startupConfig, defs []tools.Definition, compiled map[string]*workflowagent.CompiledWorkflow) string {
	type binding struct {
		Name, Version, Interface string
		Schema                   json.RawMessage
		Execution                tools.ExecutionDescription
	}
	bindings := make([]binding, 0, len(defs))
	for _, d := range defs {
		bindings = append(bindings, binding{d.Name, d.Version, d.ToolInterface, d.Schema, d.Execution})
	}
	hashes := map[string]string{}
	for id, c := range compiled {
		hashes[id] = c.Hash
	}
	raw, _ := json.Marshal(struct {
		Application string
		Model       llm.ModelConfig
		Tools       []binding
		Definitions map[string]string
	}{conf.GenerationFingerprint, conf.Model, bindings, hashes})
	sum := sha256.Sum256(raw)
	return "startup-workflow-v1:" + hex.EncodeToString(sum[:])
}

type environmentCredential struct{ model llm.ModelConfig }

func (e environmentCredential) Resolve(ctx context.Context, ref string) (llm.ResolvedCredential, error) {
	if err := ctx.Err(); err != nil {
		return llm.ResolvedCredential{}, err
	}
	if ref != e.model.CredentialRef || !strings.HasPrefix(ref, "env:") || len(ref) == 4 {
		return llm.ResolvedCredential{}, invalid("credential reference must name an environment variable")
	}
	name := strings.TrimPrefix(ref, "env:")
	for _, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return llm.ResolvedCredential{}, invalid("invalid credential reference")
		}
	}
	secret := os.Getenv(name)
	if secret == "" {
		return llm.ResolvedCredential{}, unavailable("configured model credential is unavailable")
	}
	return llm.ResolvedCredential{Secret: secret, AccountScope: e.model.AccountScope, Provider: e.model.Provider, Endpoint: e.model.Endpoint}, nil
}

func loadOptions(ctx context.Context, c Config) (runtimeOptions, error) {
	if !filepath.IsAbs(c.Workspace) || !filepath.IsAbs(c.StateRoot) || !filepath.IsAbs(c.ConfigPath) {
		return runtimeOptions{}, invalid("absolute workspace, state-root and config paths are required")
	}
	workspace, err := store.ResolveDir(c.Workspace)
	if err != nil {
		return runtimeOptions{}, invalid("workspace is unavailable")
	}
	// Resolve the existing parent before making any directory. This also prevents
	// a symlinked parent from placing the credential under the workspace.
	parent, err := store.ResolveDir(filepath.Dir(c.StateRoot))
	if err != nil {
		return runtimeOptions{}, invalid("state-root parent is unavailable")
	}
	root := filepath.Join(parent, filepath.Base(filepath.Clean(c.StateRoot)))
	if store.PathsOverlap(workspace, root) {
		return runtimeOptions{}, invalid("workspace and state-root must not overlap")
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return runtimeOptions{}, invalid("state-root must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return runtimeOptions{}, invalid("state-root unavailable")
	}
	if strings.EqualFold(filepath.Base(c.ConfigPath), ".test_env") {
		return runtimeOptions{}, invalid("live-test credential files are not startup configuration")
	}
	configPath, err := filepath.EvalSymlinks(c.ConfigPath)
	if err != nil || strings.EqualFold(filepath.Base(configPath), ".test_env") {
		return runtimeOptions{}, invalid("startup configuration is unavailable or forbidden")
	}
	f, err := os.Open(configPath)
	if err != nil {
		return runtimeOptions{}, invalid("startup configuration is unavailable")
	}
	var conf startupConfig
	err = decodeObject(f, &conf)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return runtimeOptions{}, invalid("invalid startup configuration")
	}
	if strings.TrimSpace(conf.GenerationFingerprint) == "" {
		return runtimeOptions{}, invalid("generationFingerprint is required")
	}
	if conf.Profile != codeagent.ProfileDefault && conf.Profile != codeagent.ProfileMemory {
		return runtimeOptions{}, invalid("profile is unavailable")
	}
	defs, err := builtinTools(conf.Tools, conf.ApprovalTools)
	if err != nil {
		return runtimeOptions{}, err
	}
	targets, err := startupTargets(conf, defs)
	if err != nil {
		return runtimeOptions{}, err
	}
	compiled, err := startupWorkflows(conf, defs)
	if err != nil {
		return runtimeOptions{}, err
	}
	fingerprint := conf.GenerationFingerprint
	if registry, registryErr := agent.NewAgentRegistry("", targets); registryErr != nil {
		return runtimeOptions{}, registryErr
	} else if registry.Hash() != "" {
		fingerprint += ":targets:" + registry.Hash()
	}
	resolver := environmentCredential{model: conf.Model}
	catalog := llm.NewCatalog(resolver)
	// Only factories already implemented and pinned by this project are installed.
	switch conf.Model.Protocol {
	case "openai-chat":
		err = catalog.RegisterOpenAIChat(nil, MaxJSONBytes)
	case "openai-responses":
		err = catalog.RegisterOpenAIResponses(nil, MaxJSONBytes)
	case "anthropic-messages":
		err = catalog.RegisterAnthropicMessages(nil, MaxJSONBytes)
	case "gemini-generate-content":
		err = catalog.RegisterGeminiGenerateContent(nil, MaxJSONBytes)
	case "deepseek-chat":
		err = catalog.RegisterDeepSeekChat(nil, MaxJSONBytes)
	default:
		return runtimeOptions{}, invalid("model protocol is unavailable")
	}
	if err != nil {
		return runtimeOptions{}, invalid("model factory configuration rejected")
	}
	if err = catalog.Register(conf.Model); err != nil {
		return runtimeOptions{}, invalid("model configuration rejected")
	}
	bound, err := catalog.Bind(conf.Model.Key(), llm.RequestedOptions{})
	if err != nil {
		return runtimeOptions{}, invalid("model binding rejected")
	}
	if !conf.Model.NoCredentials {
		if _, err = resolver.Resolve(ctx, conf.Model.CredentialRef); err != nil {
			return runtimeOptions{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return runtimeOptions{}, err
	}
	code := codeagent.Options{Workspace: workspace, StateRoot: root, Model: bound, Principal: localPrincipal, GenerationFingerprint: fingerprint, Profile: conf.Profile, Tools: defs, Agents: targets}
	out := runtimeOptions{Code: code, Workflows: map[string]workflowagent.WorkflowOptions{}}
	workflowFingerprint := startupWorkflowFingerprint(conf, defs, compiled)
	for id, c := range compiled {
		workflowTools := make([]tools.Definition, len(defs))
		for i, d := range defs {
			workflowTools[i] = d.Clone()
		}
		subflows := map[string]workflowagent.WorkflowDefinition{}
		for key, sub := range compiled {
			copy, _ := json.Marshal(sub.Definition)
			var detached workflowagent.WorkflowDefinition
			_ = json.Unmarshal(copy, &detached)
			subflows[key] = detached
		}
		out.Workflows[id] = workflowagent.WorkflowOptions{Definition: c.Definition, Subflows: subflows, Models: map[string]model.AgenticModel{workflowagent.WorkflowModelBinding: bound}, Tools: workflowTools, Principal: localPrincipal, Workspace: workspace, StateRoot: root, Limits: code.Limits, Policy: code.Policy, GenerationFingerprint: workflowFingerprint}
	}
	if err = privateDir(root); err != nil {
		return runtimeOptions{}, unavailable("state-root protection could not be established")
	}
	return out, nil
}

func listenAddress(address string) (string, error) {
	if address == "" {
		address = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", invalid("listen requires a loopback IP and port")
	}
	ip := net.ParseIP(host)
	p, e := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || e != nil || p < 0 || p > 65535 {
		return "", invalid("listen requires a loopback IP and port")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(p)), nil
}
