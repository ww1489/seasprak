package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/cloudwego/eino/components/model"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// MainAgentName and MainAgentVersion identify the built-in default target.
const (
	MainAgentName    = "main"
	MainAgentVersion = "main-v1"
)

// Agent target kinds.
const (
	AgentKindAgent    = "agent"
	AgentKindWorkflow = "workflow"
)

// AgentDefinition is one ordinary execution target registered at startup.
// It runs the shared controlled Agentic loop with its own instruction and
// optional model. Definitions are immutable; there is no runtime reload.
type AgentDefinition struct {
	Name        string
	Version     string
	Description string
	Instruction string
	// Model overrides the session default model for this target only.
	Model model.AgenticModel
	// Delegable exposes the target to other agents through the controlled task tool.
	Delegable bool
	Kind      string
	// Tools names the session generation tools this agent may call when it
	// runs as a delegated child. The session rejects unknown names at start;
	// an empty list keeps the child tool-less.
	Tools []string
}

// AgentInfo is the public capability view of a registered target.
type AgentInfo struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Kind        string `json:"kind"`
	Delegable   bool   `json:"delegable,omitempty"`
	Hash        string `json:"hash,omitempty"`
}

// AgentRegistry is the immutable startup inventory of execution targets.
type AgentRegistry struct {
	byName map[string]AgentDefinition
	hashes map[string]string
	names  []string
	hash   string
}

// NewAgentRegistry validates definitions. The built-in main agent is always
// present with the session instruction and default model; it cannot be
// redefined, so existing main traces and checkpoints keep their identity.
func NewAgentRegistry(mainInstruction string, defs []AgentDefinition) (*AgentRegistry, error) {
	r := &AgentRegistry{byName: map[string]AgentDefinition{}, hashes: map[string]string{}}
	r.byName[MainAgentName] = AgentDefinition{Name: MainAgentName, Version: MainAgentVersion, Kind: AgentKindAgent, Instruction: mainInstruction}
	for _, def := range defs {
		if def.Kind == "" {
			def.Kind = AgentKindAgent
		}
		if err := validAgentName(def.Name); err != nil {
			return nil, err
		}
		if def.Name == MainAgentName {
			return nil, product.NewError(product.CodeInvalidArgument, "main agent is built in and cannot be redefined")
		}
		if def.Version == "" || len(def.Version) > 128 {
			return nil, product.NewError(product.CodeInvalidArgument, "agent version is required")
		}
		if _, dup := r.byName[def.Name]; dup {
			return nil, product.Errorf(product.CodeInvalidArgument, "duplicate agent %s", def.Name)
		}
		switch def.Kind {
		case AgentKindAgent:
		case AgentKindWorkflow:
			return nil, product.NewError(product.CodeInvalidArgument, "workflows require the independent workflow owner")
		default:
			return nil, product.NewError(product.CodeInvalidArgument, "agent kind is unsupported")
		}
		seen := map[string]bool{}
		for _, name := range def.Tools {
			if name == "" || seen[name] {
				return nil, product.NewError(product.CodeInvalidArgument, "agent tool names must be unique and non-empty")
			}
			seen[name] = true
		}
		def.Tools = append([]string(nil), def.Tools...)
		r.byName[def.Name] = def
	}
	for name, def := range r.byName {
		r.names = append(r.names, name)
		if name != MainAgentName {
			r.hashes[name] = definitionHash(def)
		}
	}
	sort.Strings(r.names)
	if len(r.names) > 1 {
		all := make([]string, 0, len(r.names))
		for _, name := range r.names {
			all = append(all, name+"@"+r.byName[name].Version+"="+r.hashes[name])
		}
		raw, _ := json.Marshal(all)
		sum := sha256.Sum256(raw)
		r.hash = hex.EncodeToString(sum[:])
	}
	return r, nil
}

func validAgentName(name string) error {
	if name == "" || len(name) > 64 {
		return product.NewError(product.CodeInvalidArgument, "agent name is invalid")
	}
	for i, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || (i > 0 && (c == '-' || c == '_'))) {
			return product.NewError(product.CodeInvalidArgument, "agent name is invalid")
		}
	}
	return nil
}

// definitionHash covers every declarative field. A model override is
// identified by its trusted configuration when it exposes one.
func definitionHash(def AgentDefinition) string {
	modelIdentity := ""
	if def.Model != nil {
		modelIdentity = "injected"
		if configured, ok := def.Model.(interface{ Configuration() llm.ModelConfig }); ok {
			cfg := configured.Configuration()
			modelIdentity = cfg.Model + "@" + cfg.Version
		}
	}
	// Preserve the historical empty workflow slot for ordinary target hashes.
	fields := []any{def.Name, def.Version, def.Kind, def.Description, def.Instruction, modelIdentity, def.Delegable, ""}
	// Tools are appended only when declared, so tool-less definitions keep
	// their earlier hash and saved targets still resolve.
	if len(def.Tools) != 0 {
		fields = append(fields, def.Tools)
	}
	raw, _ := json.Marshal(fields)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Target returns the durable identity for a newly accepted request. Empty
// selects main. Unknown names are rejected; they never fall back to main.
func (r *AgentRegistry) Target(name, generation string) (TargetAgent, error) {
	if name == "" {
		name = MainAgentName
	}
	def, ok := r.byName[name]
	if !ok {
		return TargetAgent{}, product.NewError(product.CodeUnsupportedCapability, "agent is not registered")
	}
	return TargetAgent{Name: def.Name, Version: def.Version, Generation: generation, Hash: r.hashes[name]}, nil
}

// Resolve returns the definition for an exact saved target. A missing name,
// different version or changed definition is never replaced by another one.
func (r *AgentRegistry) Resolve(target TargetAgent) (AgentDefinition, error) {
	def, ok := r.byName[target.Name]
	if !ok || def.Version != target.Version || r.hashes[target.Name] != target.Hash {
		return AgentDefinition{}, product.NewError(product.CodeIncompatibleResume, "saved agent target is not registered in this build")
	}
	return def, nil
}

// Delegates lists delegable targets other than main and the caller.
func (r *AgentRegistry) Delegates(caller string) []AgentDefinition {
	var out []AgentDefinition
	for _, name := range r.names {
		def := r.byName[name]
		if def.Delegable && name != MainAgentName && name != caller {
			out = append(out, def)
		}
	}
	return out
}

// Infos returns the capability view in name order.
func (r *AgentRegistry) Infos() []AgentInfo {
	out := make([]AgentInfo, 0, len(r.names))
	for _, name := range r.names {
		def := r.byName[name]
		info := AgentInfo{Name: def.Name, Version: def.Version, Description: def.Description, Kind: def.Kind, Delegable: def.Delegable, Hash: r.hashes[name]}
		out = append(out, info)
	}
	return out
}

// Hash identifies the custom inventory. It is empty when only main exists, so
// sessions without registered targets keep their earlier build fingerprints.
func (r *AgentRegistry) Hash() string { return r.hash }
