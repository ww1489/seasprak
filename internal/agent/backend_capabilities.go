package agent

import "context"

// BackendCapabilities is a trusted report from the actual installed Operations
// instance, not a tool declaration or user-provided permission. EnvironmentID
// identifies its assembled workspace/state-root/resource mappings; the backend
// must change it when those mappings change. Version identifies the implementation.
// Reports describe current capability, not certification of a native sandbox.
type BackendCapabilities struct {
	BackendID                 string   `json:"backendId"`
	Version                   string   `json:"version"`
	EnvironmentID             string   `json:"environmentId"`
	SupportedModes            []string `json:"supportedModes"`
	Enforcement               string   `json:"enforcement"`
	RuntimeDataWriteProtected bool     `json:"runtimeDataWriteProtected"`
}

// BackendCapabilityReporter must be implemented by each installed Files/Process
// instance. Implementations return a concurrency-safe, detached current snapshot
// without re-entering a session or performing a blocking facility probe, and must
// enforce that snapshot for the authorized start. Missing reports fail
// closed; the host must not infer protection from a non-nil Operations port.
type BackendCapabilityReporter interface {
	ExecutionCapabilities(context.Context) (BackendCapabilities, error)
}

// ExecutionSandboxModeSource supplies the mode associated with the trusted
// policy reference. Standalone executors without this port use workspace-write.
type ExecutionSandboxModeSource interface {
	ExecutionSandboxMode(context.Context, ExecutionScope) (string, error)
}
