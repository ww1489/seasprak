package sessions

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/agent/tools"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type AgentSession struct{ rt *runtime }

func Start(opts Options, manager *state.Manager, generation string) (*AgentSession, error) {
	if !opts.ReadOnly {
		if opts.Profile == ProfileDefault {
			return nil, product.NewError(product.CodeResourceUnavailable, "default file and process backends are not available")
		}
		if opts.Profile != ProfileMemory {
			return nil, product.NewError(product.CodeUnsupportedCapability, "unknown capability profile")
		}
		if opts.Model == nil {
			return nil, product.NewError(product.CodeInvalidArgument, "model is required")
		}
	}
	opts.Limits = opts.Limits.WithDefaults()
	opts.Tools = append([]tools.Definition(nil), opts.Tools...)
	for i := range opts.Tools {
		opts.Tools[i] = opts.Tools[i].Clone()
	}
	if opts.Policy != nil {
		copyPolicy := *opts.Policy
		opts.Policy = &copyPolicy
	}
	if opts.Store == nil || manager == nil {
		return nil, product.NewError(product.CodeInvalidArgument, "session store and manager are required")
	}
	registry, err := agent.NewAgentRegistry(opts.Instruction, opts.Agents)
	if err != nil {
		return nil, err
	}
	if err := initializeExecutionPolicy(opts, manager); err != nil {
		return nil, err
	}
	rt := &runtime{opts: opts, manager: manager, mailbox: make(chan command, 64), done: make(chan struct{}), subs: map[int]*subscription{}, generation: generation, registry: registry}
	// An injected TODO backend remains the sole owner of its persistence.
	if rt.opts.Operations.Todos == nil {
		rt.opts.Operations.Todos = &sessionTodos{rt: rt}
	}
	rt.restoreDefaultSelection()
	if err := rt.restoreResourceHolds(); err != nil {
		return nil, err
	}
	view := manager.View()
	// Opening never replays an execution or silently releases an old queue.
	if !opts.ReadOnly && !view.RepairRequired {
		for id, tr := range view.Traces {
			if tr.Started && !terminal(tr.State) && tr.State != "paused" {
				if err := manager.SetTraceState(context.Background(), id, "paused", false); err != nil {
					return nil, err
				}
			}
			if tr.State == "queued" && !tr.Hold {
				if err := manager.HoldIndependent(context.Background(), id); err != nil {
					return nil, err
				}
			}
		}
	}
	// The builtin delegation evidence query is read-only; a host query with
	// the same ID keeps precedence.
	if _, ok := rt.opts.ReconcileQueries[DelegateEvidenceQuery]; !ok {
		queries := make(map[string]ReconcileQuery, len(rt.opts.ReconcileQueries)+1)
		for id, query := range rt.opts.ReconcileQueries {
			queries[id] = query
		}
		queries[DelegateEvidenceQuery] = ReconcileQueryFunc(rt.delegateEvidence)
		rt.opts.ReconcileQueries = queries
	}
	if !opts.ReadOnly && !view.RepairRequired {
		// Child resume is not delivered: a running child becomes interrupted,
		// and its claimed parent call keeps the existing unknown-effect handling.
		if err := failInterruptedInvocations(manager); err != nil {
			return nil, err
		}
		rt.settleAllTraceOperations()
	}
	rt.cursor = manager.View().Cursor
	go rt.loop()
	return &AgentSession{rt: rt}, nil
}
func (s *AgentSession) SubmitInput(ctx context.Context, cmd agent.InputCommand) (agent.InputReceipt, error) {
	cmd.Content = append([]byte(nil), cmd.Content...)
	if cmd.Principal == "" {
		cmd.Principal = s.rt.opts.Principal
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if err := rt.flushHostCommands(ctx); err != nil {
			return nil, err
		}
		target, err := rt.targetFor(cmd)
		_, replay := rt.manager.FindInput(cmd)
		if err != nil {
			// Idempotent replays keep their original receipt even if the name
			// is unknown now; only new requests are rejected here.
			if !replay {
				return nil, err
			}
		}
		// Workflow input is validated before acceptance, so a missing field
		// writes nothing and never reaches a model.
		if !replay {
			if err := rt.admitWorkflowInput(cmd, target); err != nil {
				return nil, err
			}
			if err := rt.admitAttachments(cmd, target); err != nil {
				return nil, err
			}
		}
		before := rt.manager.View().LastSeq
		// A target with its own trusted model is not redirected by the
		// session default-model selection.
		selection := rt.defaultModelID
		if def, derr := rt.definitionFor(target); derr == nil && def.Model != nil {
			selection = ""
		}
		receipt, err := rt.manager.AcceptWithLimitsAndSelection(ctx, cmd, target, rt.opts.Limits, selection)
		if err != nil {
			return nil, err
		}
		// A replayed acceptance receipt never schedules work again.
		if receipt.AcceptedCommit > before {
			rt.schedule()
		}
		return receipt, nil
	})
	if err != nil {
		return agent.InputReceipt{}, err
	}
	return value.(agent.InputReceipt), nil
}
func (s *AgentSession) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	select {
	case <-s.rt.done:
		return s.rt.snapshot(s.rt.manager.View(), nil), nil
	default:
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		v := rt.manager.View()
		resume := make(map[string]ResumeEligibility, len(v.Traces))
		for id := range v.Traces {
			err := rt.writable()
			if err == nil {
				if interruptedRootChild(v, id) {
					_, err = rt.validateChildResume(v, id)
				} else if v.Traces[id].Kind == "command" {
					err = incompatibleResume("legacy direct commands cannot be resumed")
				} else if _, workflow := rt.workflowTarget(v.Traces[id].Target); workflow {
					err = rt.validateWorkflowResume(id, v)
				} else {
					_, err = rt.validateResume(ctx, id, v)
				}
			}
			status := ResumeEligibility{CanResume: err == nil}
			if err != nil {
				status.Code = product.CodeIncompatibleResume
				if pe, ok := product.AsError(err); ok {
					status.Code = pe.Code
					status.Reason = pe.Message
				}
			}
			resume[id] = status
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		snap := rt.snapshot(v, resume)
		snap.Transient = rt.transient.copy()
		return snap, nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return value.(Snapshot), nil
}

// snapshot consumes the exclusive object graph returned by Manager.View.
// All private-history validation must finish before entering this projection.
func (rt *runtime) snapshot(v state.View, resume map[string]ResumeEligibility) (out Snapshot) {
	interactions, approvals := rt.snapshotApprovals(v)
	for id, op := range v.Operations {
		if op.Kind == "respond_interaction" {
			delete(v.Operations, id)
		}
	}
	for id, op := range rt.approvalOps {
		v.Operations[id] = op
	}
	messages := snapshotMessages(v)
	out = Snapshot{ModelAttempts: snapshotAttemptViews(v), Observations: snapshotObservationViews(v), Revision: v.LastSeq, SessionID: rt.opts.SessionID, Cursor: v.Cursor, ActiveTrace: v.ActiveTrace, Traces: v.Traces, Inputs: v.Inputs, Messages: messages, Turns: v.Turns, Calls: v.Calls, Operations: v.Operations, Selections: v.Selections, Reconciliations: v.Reconciliations, RepairRequired: v.RepairRequired, Resume: resume, Interactions: interactions, Approvals: approvals, FrozenExecutions: v.FrozenExecutions, Invocations: v.Invocations}
	out.WorkflowNodes = v.WorkflowNodes
	out.PendingReconciliations = pendingReconciliations(v)
	return out
}
