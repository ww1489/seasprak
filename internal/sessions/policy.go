package sessions

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

func initializeExecutionPolicy(opts Options, manager *state.Manager) error {
	current := manager.View().ExecutionPolicy
	if current.Revision != 0 {
		if opts.Policy == nil {
			return nil
		}
		requested, err := state.NormalizeExecutionPolicy(*opts.Policy)
		if err != nil {
			return err
		}
		if requested.SandboxMode != current.SandboxMode || requested.ApprovalPolicy != current.ApprovalPolicy || requested.Auto != current.Auto {
			return product.NewError(product.CodeStateConflict, "configured execution policy differs from persisted policy")
		}
		return nil
	}
	if opts.ReadOnly {
		return nil
	}
	declaration := agent.ResolvedPolicy{}
	if opts.Policy != nil {
		declaration = *opts.Policy
	}
	// A pre-policy journal is not a fresh configuration grant. Writable reopen
	// establishes conservative defaults; an explicit conflicting request fails.
	if manager.View().LastSeq != 0 {
		requested, err := state.NormalizeExecutionPolicy(declaration)
		if err != nil {
			return err
		}
		defaults, _ := state.NormalizeExecutionPolicy(agent.ResolvedPolicy{})
		if requested != defaults {
			return product.NewError(product.CodeStateConflict, "legacy session requires the default execution policy")
		}
		declaration = defaults
	}
	return manager.SetExecutionPolicy(context.Background(), 0, declaration)
}

// setExecutionPolicy is a finite trusted internal operation. It does not expose
// a public policy-edit API or bypass the single mailbox/manager commit path.
func (rt *runtime) setExecutionPolicy(ctx context.Context, expected uint64, p agent.ResolvedPolicy) error {
	return rt.do(ctx, func(rt *runtime) error {
		if err := rt.writable(); err != nil {
			return err
		}
		return rt.manager.SetExecutionPolicy(ctx, expected, p)
	})
}
func (rt *runtime) ExecutionPolicyRef(ctx context.Context, scope agent.ExecutionScope) (string, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := rt.active.ctx.Err(); err != nil {
			return nil, err
		}
		if err := rt.writable(); err != nil {
			return nil, err
		}
		p := rt.manager.View().ExecutionPolicy
		if p.Ref == "" || p.Revision == 0 {
			return nil, product.NewError(product.CodeResourceUnavailable, "execution policy is unavailable")
		}
		return p.Ref, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

type sessionAuthorizer struct {
	rt    *runtime
	scope agent.ExecutionScope
}

func (a sessionAuthorizer) Authorize(ctx context.Context, authorization agent.FrozenCall) (agent.Decision, error) {
	value, err := a.rt.call(ctx, func(rt *runtime) (any, error) {
		if !rt.matchesExecution(a.scope) {
			return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		// The boundary annotates the actual turn; the fixed execution binding
		// above still rejects a replaced worker. Keep explicit fallback scopes
		// intact so a mismatched standalone scope cannot acquire permission.
		scope := einorun.ScopeFromContext(ctx, a.scope)
		if scope.TurnID == "" {
			scope.TurnID = rt.active.turnID
		}
		v := rt.manager.View()
		frozen, ok := v.FrozenExecutions["execution:"+authorization.CallID]
		call, exists := v.Calls[authorization.CallID]
		if !exists || !rt.matchesCallScope(scope, call) || !ok || frozen.CallID != authorization.CallID || frozen.ProviderCallID != authorization.ProviderCallID || frozen.Tool != authorization.Name || frozen.Generation != authorization.Generation || string(frozen.FinalArguments) != authorization.Arguments || frozen.Hash != authorization.Hash || frozen.Scope != call.Scope {
			return nil, product.NewError(product.CodePermissionDenied, "authorization does not match committed execution")
		}
		return rt.checkToolPolicy(ctx, scope, frozen)
	})
	if err != nil {
		return agent.DecisionDeny, err
	}
	return value.(agent.Decision), nil
}

// checkToolPolicy runs only inside the mailbox. In particular the claim path
// calls it directly: re-entering the mailbox or ledger would deadlock.
func (rt *runtime) checkToolPolicy(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenExecution) (agent.Decision, error) {
	return rt.checkToolPolicyState(ctx, scope, frozen, false)
}

func (rt *runtime) checkToolPolicyState(ctx context.Context, scope agent.ExecutionScope, frozen agent.FrozenExecution, claimed bool) (agent.Decision, error) {
	if err := ctx.Err(); err != nil {
		return agent.DecisionCancel, err
	}
	if rt.active == nil {
		return agent.DecisionDeny, product.NewError(product.CodeStateConflict, "execution is no longer active")
	}
	if err := rt.active.ctx.Err(); err != nil {
		return agent.DecisionCancel, err
	}
	if err := rt.writable(); err != nil {
		return agent.DecisionDeny, err
	}
	v := rt.manager.View()
	call, ok := v.Calls[frozen.CallID]
	if !ok || !rt.matchesCallScope(scope, call) || frozen.Scope != call.Scope || call.Observation != nil || call.Claimed != claimed || !acceptedAttemptForCall(v, call) {
		return agent.DecisionDeny, product.NewError(product.CodeStateConflict, "execution is not a pending accepted call")
	}
	committed, ok := v.FrozenExecutions[frozen.ID]
	digest, err := frozen.Digest()
	if !ok || frozen.ID != "execution:"+call.Call.CallID || frozen.Hash == "" || digest != frozen.Hash || err != nil || committed.Hash != frozen.Hash || frozen.Tool != call.Call.Name || frozen.Generation != call.Call.Generation || frozen.ProviderCallID != call.Call.ProviderCallID {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution description is not committed")
	}
	p := v.ExecutionPolicy
	if p.Ref == "" || p.Ref != frozen.PolicyRef {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution policy changed after freezing")
	}
	if _, err := state.NormalizeExecutionPolicy(p); err != nil {
		return agent.DecisionDeny, err
	}
	if frozen.RequestedGrantRef != "" {
		if p.ApprovalPolicy == "never" {
			return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "execution requires a disallowed approval")
		}
		return agent.DecisionAsk, product.NewError(product.CodeResourceUnavailable, "execution approval is not available")
	}
	if p.SandboxMode == "read-only" && frozen.Effect != "read" && frozen.Effect != "none" {
		return agent.DecisionDeny, product.NewError(product.CodePermissionDenied, "read-only policy denies declared side effects")
	}
	switch frozen.BackendID {
	case "trusted-run":
		if len(frozen.Argv) == 0 && frozen.Cwd == "" && len(frozen.Mounts) == 0 && frozen.StdinRef == "" && frozen.TempRootRef == "" {
			for _, def := range rt.opts.Tools {
				if def.Name == frozen.Tool && def.Version == frozen.ToolVersion && toolArgumentHash(def.Schema) == frozen.SchemaHash && (def.Run != nil || def.RunWithOutput != nil) && (def.Execution.BackendID == "" || def.Execution.BackendID == "trusted-run") {
					return agent.DecisionAllow, nil
				}
			}
		}
	case "process-operations":
		if rt.opts.Operations.Process != nil && len(frozen.Argv) != 0 {
			return agent.DecisionAllow, nil
		}
	case "file-operations":
		if rt.opts.Operations.Files != nil && len(frozen.Argv) == 0 {
			return agent.DecisionAllow, nil
		}
	case "artifact-store":
		if rt.opts.Operations.Artifacts != nil {
			return agent.DecisionAllow, nil
		}
	}
	return agent.DecisionDeny, product.NewError(product.CodeResourceUnavailable, "required controlled execution backend is not available")
}
