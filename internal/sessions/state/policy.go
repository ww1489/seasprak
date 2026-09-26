package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// NormalizeExecutionPolicy accepts declarations only, not caller-owned identity.
func NormalizeExecutionPolicy(p agent.ResolvedPolicy) (agent.ResolvedPolicy, error) {
	p.Ref, p.Revision = "", 0
	if p.SandboxMode == "" {
		p.SandboxMode = "workspace-write"
	}
	if p.ApprovalPolicy == "" {
		p.ApprovalPolicy = "ask"
	}
	switch p.SandboxMode {
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return p, product.NewError(product.CodeInvalidArgument, "invalid execution sandbox mode")
	}
	if p.ApprovalPolicy != "ask" && p.ApprovalPolicy != "never" {
		return p, product.NewError(product.CodeInvalidArgument, "invalid execution approval policy")
	}
	if p.Auto {
		return p, product.NewError(product.CodeUnsupportedCapability, "automatic execution review is not available")
	}
	return p, nil
}
func executionPolicyRef(p agent.ResolvedPolicy) string {
	raw, _ := json.Marshal(struct {
		Revision       uint64 `json:"revision"`
		SandboxMode    string `json:"sandboxMode"`
		ApprovalPolicy string `json:"approvalPolicy"`
		Auto           bool   `json:"auto"`
	}{p.Revision, p.SandboxMode, p.ApprovalPolicy, p.Auto})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SetExecutionPolicy commits the policy and its audit event together. The caller
// serializes runtime changes in its mailbox; no budget ledger is called here.
func (m *Manager) SetExecutionPolicy(ctx context.Context, expected uint64, declaration agent.ResolvedPolicy) error {
	p, err := NormalizeExecutionPolicy(declaration)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.view.ExecutionPolicy.Revision != expected || expected == math.MaxUint64 {
		return product.NewError(product.CodeStateConflict, "execution policy revision changed")
	}
	p.Revision = expected + 1
	p.Ref = executionPolicyRef(p)
	_, err = m.commit(ctx, []store.Record{record("execution_policy", "current", p)}, nil, []agent.Event{m.event("security.policy_changed", "", "", p)})
	return err
}
func applyExecutionPolicy(v *View, r store.Record) error {
	var p agent.ResolvedPolicy
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return product.NewError(product.CodeIncompatibleVersion, "invalid execution policy record")
	}
	declaration, err := NormalizeExecutionPolicy(p)
	if err != nil || r.ID != "current" || p.Revision == 0 || v.ExecutionPolicy.Revision == math.MaxUint64 || p.Revision != v.ExecutionPolicy.Revision+1 || p.Ref != executionPolicyRef(p) || p.SandboxMode != declaration.SandboxMode || p.ApprovalPolicy != declaration.ApprovalPolicy {
		return product.NewError(product.CodeIncompatibleVersion, "invalid execution policy transition")
	}
	v.ExecutionPolicy = p
	return nil
}
