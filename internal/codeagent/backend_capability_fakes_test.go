package codeagent

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
)

// These explicitly controlled P2 fakes capture requests and counters in memory;
// they cannot write the journal or launch a host process. Reports describe only
// that simulated environment, never native sandbox certification.
func sessionFakeCapabilities(id string) agent.BackendCapabilities {
	return agent.BackendCapabilities{BackendID: id, Version: "test-v1", EnvironmentID: "session-test-memory", SupportedModes: []string{"read-only", "workspace-write", "danger-full-access"}, Enforcement: "partial", RuntimeDataWriteProtected: true}
}
func (*commandProcessProbe) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("command-process-fake"), nil
}
func (*sessionControlledProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("session-process-fake"), nil
}
func (*gatedOutputProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("gated-output-fake"), nil
}
func (identityOutputProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return sessionFakeCapabilities("identity-output-fake"), nil
}
