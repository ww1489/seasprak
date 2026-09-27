package tools

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
)

// These fakes only capture requests and update in-memory counters. They never
// launch a host process or write session runtime data. This declaration models
// P2 controlled capability; it does not certify any OS sandbox.
func memoryBackendCapabilities(id string) agent.BackendCapabilities {
	return agent.BackendCapabilities{BackendID: id, Version: "test-v1", EnvironmentID: "test-memory", SupportedModes: []string{"read-only", "workspace-write", "danger-full-access"}, Enforcement: "partial", RuntimeDataWriteProtected: true}
}
func (*deadlineCaptureProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("deadline-process-fake"), nil
}

func (*controlledProcessOperations) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("controlled-process-fake"), nil
}
func (*controlledFileOperations) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("controlled-files-fake"), nil
}
func (*builtinFileProbe) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("builtin-files-fake"), nil
}
func (*builtinProcessProbe) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("builtin-process-fake"), nil
}
func (*artifactProcessProbe) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("artifact-process-fake"), nil
}
func (*referenceOnlyProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("reference-process-fake"), nil
}
func (*runningProcessObservation) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return memoryBackendCapabilities("running-process-fake"), nil
}
