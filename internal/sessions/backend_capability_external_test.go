package sessions_test

import (
	"context"
	"github.com/ww1489/seasprak/internal/agent"
)

// unendedPauseProcess is a controllable in-memory lifecycle fake, not a native
// process. Its unfinished observation does not grant writes to runtime data.
func (*unendedPauseProcess) ExecutionCapabilities(context.Context) (agent.BackendCapabilities, error) {
	return agent.BackendCapabilities{BackendID: "unended-pause-fake", Version: "test-v1", EnvironmentID: "pause-test-memory", SupportedModes: []string{"workspace-write"}, Enforcement: "partial", RuntimeDataWriteProtected: true}, nil
}
