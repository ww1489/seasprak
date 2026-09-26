package agent

import "context"

// ResolvedPolicy is the first-stage execution policy, not a sandbox capability.
// Ref and Revision are assigned by the trusted state manager, never by a caller.
type ResolvedPolicy struct {
	Ref            string `json:"ref"`
	Revision       uint64 `json:"revision"`
	SandboxMode    string `json:"sandboxMode"`
	ApprovalPolicy string `json:"approvalPolicy"`
	Auto           bool   `json:"auto"`
}

// ExecutionPolicySource supplies the current trusted reference before hashing.
// Legacy independently assembled executors need not implement this port.
type ExecutionPolicySource interface {
	ExecutionPolicyRef(context.Context, ExecutionScope) (string, error)
}
