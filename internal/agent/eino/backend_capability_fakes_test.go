package eino

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
)

// interfaceProcess is an in-memory ticket/counter fake with no host side effects.
func (*interfaceProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return agent.BackendCapabilities{BackendID: "interface-process-fake", Version: "test-v1", EnvironmentID: "test-memory", SupportedModes: []string{"workspace-write"}, Enforcement: "partial", RuntimeDataWriteProtected: true}, nil
}
