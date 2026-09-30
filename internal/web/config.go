package web

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	"github.com/ww1489/seasprak/internal/llm"
	"github.com/ww1489/seasprak/internal/sessions"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// Config contains only trusted process-start inputs, never HTTP request fields.
// StateRoot's parent must exist. A missing StateRoot is created privately;
// existing StateRoot permissions are validated, never silently changed.
type Config struct{ Workspace, StateRoot, ConfigPath, Listen string }

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
	// Agents and Workflows are immutable trusted startup declarations. They
	// share the bound model and configured tool generation; HTTP cannot add or
	// replace them while the process is running.
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

// startupWorkflow embeds the one accepted declarative definition. Delegable
// controls only whether another registered agent may invoke it.
type startupWorkflow struct {
	agent.WorkflowDefinition
	Delegable bool `json:"delegable,omitempty"`
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

// startupTargets validates and compiles the immutable target inventory before
// the state directory or listener is created. Workflow dependencies are
// resolved by exact name@version; cycles and missing bindings fail closed.
func startupTargets(conf startupConfig, defs []tools.Definition) ([]agent.AgentDefinition, error) {
	targets := make([]agent.AgentDefinition, 0, len(conf.Agents)+len(conf.Workflows))
	for _, a := range conf.Agents {
		if strings.TrimSpace(a.Instruction) == "" {
			return nil, invalid("agent instruction is required")
		}
		targets = append(targets, agent.AgentDefinition{Name: a.Name, Version: a.Version, Description: a.Description, Instruction: a.Instruction, Delegable: a.Delegable, Kind: agent.AgentKindAgent, Tools: append([]string(nil), a.Tools...)})
	}

	byID := make(map[string]startupWorkflow, len(conf.Workflows))
	for _, w := range conf.Workflows {
		id := w.Name + "@" + w.Version
		if _, duplicate := byID[id]; duplicate {
			return nil, invalid("workflow name and version must be unique")
		}
		byID[id] = w
	}
	compiled := make(map[string]agent.AgentDefinition, len(conf.Workflows))
	visiting := map[string]bool{}
	var compile func(string) error
	compile = func(id string) error {
		if _, ok := compiled[id]; ok {
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
			if node.Type == agent.WorkflowNodeSubflow {
				if err := compile(node.Subflow); err != nil {
					return err
				}
			}
		}
		opts := sessions.Options{Tools: defs, Agents: append([]agent.AgentDefinition(nil), targets...)}
		for _, target := range compiled {
			opts.Agents = append(opts.Agents, target)
		}
		target, err := sessions.CompileWorkflowTarget(w.WorkflowDefinition, opts)
		if err != nil {
			return err
		}
		target.Delegable = w.Delegable
		compiled[id] = target
		return nil
	}
	for _, w := range conf.Workflows {
		id := w.Name + "@" + w.Version
		if err := compile(id); err != nil {
			return nil, err
		}
		targets = append(targets, compiled[id])
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

func loadOptions(ctx context.Context, c Config) (sessions.Options, error) {
	if !filepath.IsAbs(c.Workspace) || !filepath.IsAbs(c.StateRoot) || !filepath.IsAbs(c.ConfigPath) {
		return sessions.Options{}, invalid("absolute workspace, state-root and config paths are required")
	}
	workspace, err := store.ResolveDir(c.Workspace)
	if err != nil {
		return sessions.Options{}, invalid("workspace is unavailable")
	}
	// Resolve the existing parent before making any directory. This also prevents
	// a symlinked parent from placing the credential under the workspace.
	parent, err := store.ResolveDir(filepath.Dir(c.StateRoot))
	if err != nil {
		return sessions.Options{}, invalid("state-root parent is unavailable")
	}
	root := filepath.Join(parent, filepath.Base(filepath.Clean(c.StateRoot)))
	if store.PathsOverlap(workspace, root) {
		return sessions.Options{}, invalid("workspace and state-root must not overlap")
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return sessions.Options{}, invalid("state-root must be a real directory")
		}
	} else if !os.IsNotExist(err) {
		return sessions.Options{}, invalid("state-root unavailable")
	}
	if strings.EqualFold(filepath.Base(c.ConfigPath), ".test_env") {
		return sessions.Options{}, invalid("live-test credential files are not startup configuration")
	}
	configPath, err := filepath.EvalSymlinks(c.ConfigPath)
	if err != nil || strings.EqualFold(filepath.Base(configPath), ".test_env") {
		return sessions.Options{}, invalid("startup configuration is unavailable or forbidden")
	}
	f, err := os.Open(configPath)
	if err != nil {
		return sessions.Options{}, invalid("startup configuration is unavailable")
	}
	var conf startupConfig
	err = decodeObject(f, &conf)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return sessions.Options{}, invalid("invalid startup configuration")
	}
	if strings.TrimSpace(conf.GenerationFingerprint) == "" {
		return sessions.Options{}, invalid("generationFingerprint is required")
	}
	if conf.Profile != sessions.ProfileDefault && conf.Profile != sessions.ProfileMemory {
		return sessions.Options{}, invalid("profile is unavailable")
	}
	defs, err := builtinTools(conf.Tools, conf.ApprovalTools)
	if err != nil {
		return sessions.Options{}, err
	}
	targets, err := startupTargets(conf, defs)
	if err != nil {
		return sessions.Options{}, err
	}
	fingerprint := conf.GenerationFingerprint
	if registry, registryErr := agent.NewAgentRegistry("", targets); registryErr != nil {
		return sessions.Options{}, registryErr
	} else if registry.Hash() != "" {
		// Target declarations are part of the generation identity. A restart
		// with changed agents/workflows cannot silently accept new work under
		// an old session generation even when the operator forgot to bump the
		// application fingerprint.
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
		return sessions.Options{}, invalid("model protocol is unavailable")
	}
	if err != nil {
		return sessions.Options{}, invalid("model factory configuration rejected")
	}
	if err = catalog.Register(conf.Model); err != nil {
		return sessions.Options{}, invalid("model configuration rejected")
	}
	model, err := catalog.Bind(conf.Model.Key(), llm.RequestedOptions{})
	if err != nil {
		return sessions.Options{}, invalid("model binding rejected")
	}
	if !conf.Model.NoCredentials {
		if _, err = resolver.Resolve(ctx, conf.Model.CredentialRef); err != nil {
			return sessions.Options{}, err
		}
	}
	if err = ctx.Err(); err != nil {
		return sessions.Options{}, err
	}
	if err = privateDir(root); err != nil {
		return sessions.Options{}, unavailable("state-root protection could not be established")
	}
	return sessions.Options{Workspace: workspace, StateRoot: root, Model: model, Principal: localPrincipal, GenerationFingerprint: fingerprint, Profile: conf.Profile, Tools: defs, Agents: targets}, nil
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
