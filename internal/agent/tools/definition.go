package tools

import (
	"context"
	"encoding/json"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ww1489/seasprak/internal/agent"
)

type ExecutionDescription struct {
	Resources         []agent.ExecutionResource
	Effect            string
	Concurrency       string
	BackendID         string
	Argv              []string
	Cwd               string
	EnvironmentRef    string
	StdinRef          string
	Mounts            []agent.ExecutionMount
	TempRootRef       string
	PolicyRef         string // Declarative only; the trusted policy source overrides it.
	RequestedGrantRef string
	Timeout           time.Duration
	OutputLimitBytes  int
}

func (d ExecutionDescription) Clone() ExecutionDescription { return d.clone() }

func (d ExecutionDescription) clone() ExecutionDescription {
	d.Resources = append([]agent.ExecutionResource(nil), d.Resources...)
	d.Argv = append([]string(nil), d.Argv...)
	d.Mounts = append([]agent.ExecutionMount(nil), d.Mounts...)
	return d
}

func ValidToolInterface(kind string) bool {
	switch kind {
	case "", "invokable", "streamable", "enhanced-invokable", "enhanced-streamable":
		return true
	default:
		return false
	}
}

// Explicitly selected Eino interfaces are part of the immutable tool generation.
type Definition struct {
	Version          string
	Name             string
	Description      string
	Schema           json.RawMessage
	ToolInterface    string // Empty means the original invokable interface.
	PrepareArguments []func(context.Context, json.RawMessage) (json.RawMessage, error)
	Validate         func(context.Context, json.RawMessage) error
	ResolveExecution func(context.Context, json.RawMessage, ExecutionDescription) (ExecutionDescription, error)
	BeforeCall       []func(context.Context, agent.FrozenExecution) error
	Execution        ExecutionDescription
	Run              func(context.Context, json.RawMessage) (string, error) // Trusted in-process compatibility only.
	RunWithOutput    func(context.Context, json.RawMessage, agent.ToolOutputSink) (string, error)
}

func (d Definition) Clone() Definition {
	d.Schema = append(json.RawMessage(nil), d.Schema...)
	d.PrepareArguments = append([]func(context.Context, json.RawMessage) (json.RawMessage, error)(nil), d.PrepareArguments...)
	d.BeforeCall = append([]func(context.Context, agent.FrozenExecution) error(nil), d.BeforeCall...)
	d.Execution = d.Execution.clone()
	return d
}

type Operations struct {
	Files     agent.FileOperations
	Process   agent.ProcessOperations
	Artifacts agent.ArtifactStore
}

type ExecutorOption func(*Executor)

func WithCompiledSchemas(compiled map[string]*jsonschema.Schema) ExecutorOption {
	return func(e *Executor) {
		e.comp = make(map[string]*jsonschema.Schema, len(compiled))
		for name, schema := range compiled {
			e.comp[name] = schema
		}
	}
}

func WithOperations(operations Operations) ExecutorOption {
	return func(e *Executor) { e.operations = operations }
}
func WithResourceScheduler(scheduler *ResourceScheduler) ExecutorOption {
	return func(e *Executor) { e.scheduler = scheduler }
}
func WithResourceDomain(environment, workspace string) ExecutorOption {
	return func(e *Executor) { e.environment, e.workspace = environment, workspace }
}
