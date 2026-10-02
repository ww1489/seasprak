package codeagent

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
)

// newChildBudget restores this invocation's model occupancy. A request's local
// delta is rebased on the latest invocation aggregate inside the mailbox, so
// independent model-node/summary ledgers cannot overwrite concurrent charges.
// The child lock may acquire the trace ledger and then the mailbox; the mailbox
// never acquires either ledger while committing the supplied exact candidates.
func (rt *runtime) newChildBudget(ctx context.Context, start delegateStart) *agent.BudgetLedger {
	ledger := agent.NewBudget(start.parent.Limits())
	used := rt.manager.View().InvocationBudgets[start.inv.ID].Usage
	ledger.Restore(used)
	last := used
	ledger.SetPersist(func(next agent.Usage) error {
		logical, physical := next.LogicalModelCalls-last.LogicalModelCalls, next.TransportRequests-last.TransportRequests
		err := start.parent.ChargeDelegatedWith(logical, physical, func(parent agent.Usage) error {
			return rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
				if !rt.matchesDelegatedChild(start.scope) {
					return product.NewError(product.CodeStateConflict, "child budget execution is no longer active")
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := rt.active.ctx.Err(); err != nil {
					return err
				}
				if rt.active.activity == nil {
					return activityExhausted()
				}
				if err := rt.active.activity.allowed(); err != nil {
					return err
				}
				child := rt.manager.View().InvocationBudgets[start.inv.ID].Usage
				child.LogicalModelCalls += logical
				child.TransportRequests += physical
				child.ModelCallID, child.ModelRequests, child.LastTransport = next.ModelCallID, next.ModelRequests, next.LastTransport
				return rt.manager.SaveInvocationBudget(ctx, start.scope, child, parent)
			})
		})
		if err == nil {
			last = next
		}
		return err
	})
	return ledger
}
