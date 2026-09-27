package tools

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// CapabilityHash probes only the actual installed instance. An independent
// report supplied alongside Operations could accidentally certify another backend.
func (o Operations) CapabilityHash(ctx context.Context, backend, mode string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var installed any
	switch backend {
	case "file-operations":
		installed = o.Files
	case "process-operations":
		installed = o.Process
	default:
		return "", product.NewError(product.CodeResourceUnavailable, "backend has no controlled capability report")
	}
	reporter, ok := installed.(agent.BackendCapabilityReporter)
	if !ok {
		return "", product.NewError(product.CodeResourceUnavailable, "controlled backend capability report is missing")
	}
	report, err := reporter.ExecutionCapabilities(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	if err != nil {
		return "", product.NewError(product.CodeResourceUnavailable, "controlled backend capability probe failed")
	}
	if report.BackendID == "" || report.Version == "" || report.EnvironmentID == "" ||
		!report.RuntimeDataWriteProtected || (report.Enforcement != "full" && report.Enforcement != "partial") ||
		!slices.Contains(report.SupportedModes, mode) || (mode != "read-only" && mode != "workspace-write" && mode != "danger-full-access") {
		return "", product.NewError(product.CodeResourceUnavailable, "controlled backend lacks required protection or mode support")
	}
	report.SupportedModes = slices.Clone(report.SupportedModes)
	slices.Sort(report.SupportedModes)
	report.SupportedModes = slices.Compact(report.SupportedModes)
	raw, err := json.Marshal(report)
	if err != nil {
		return "", err
	}
	return argumentHash(raw), nil
}

// ValidateCapabilities checks every installed controlled port at configuration
// time, including capabilities that are not selected for the first model turn.
func (o Operations) ValidateCapabilities(ctx context.Context, mode string) error {
	for _, backend := range []string{"file-operations", "process-operations"} {
		if (backend == "file-operations" && o.Files == nil) || (backend == "process-operations" && o.Process == nil) {
			continue
		}
		if _, err := o.CapabilityHash(ctx, backend, mode); err != nil {
			return err
		}
	}
	return nil
}

func (o Operations) ValidateFrozenCapabilities(ctx context.Context, frozen agent.FrozenExecution) error {
	if frozen.BackendID != "file-operations" && frozen.BackendID != "process-operations" {
		return nil
	}
	hash, err := o.CapabilityHash(ctx, frozen.BackendID, frozen.SandboxMode)
	if err != nil {
		return err
	}
	if frozen.BackendCapabilitiesHash == "" || hash != frozen.BackendCapabilitiesHash {
		return product.NewError(product.CodeResourceUnavailable, "controlled backend capabilities changed after freezing")
	}
	return nil
}

// Recheck after the session validator too: it can wait for the mailbox while a
// backend's configuration changes. The backend consumes this one-use ticket
// immediately before effects, in addition to the executor's pre-invocation gate.
type backendTicketValidator struct {
	operations Operations
	next       agent.ExecutionTicketValidator
}

func (v backendTicketValidator) ValidateExecutionTicket(ctx context.Context, frozen agent.FrozenExecution) error {
	if v.next != nil {
		if err := v.next.ValidateExecutionTicket(ctx, frozen); err != nil {
			return err
		}
	}
	return v.operations.ValidateFrozenCapabilities(ctx, frozen)
}
