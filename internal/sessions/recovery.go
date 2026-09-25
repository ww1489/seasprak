package sessions

import (
	"context"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// cancelInterrupted runs only in the mailbox, when this trace has no worker.
// It uses the same turn cleanup and terminal commit as an active cancellation.
func (rt *runtime) cancelInterrupted(ctx context.Context, tr *state.TraceState) error {
	if tr.State != "paused" && tr.State != "cancelling" {
		return product.NewError(product.CodeStateConflict, "trace is not interrupted")
	}
	view := rt.manager.View()
	if !tr.ExecutionStopped && view.TraceHasUnresolvedEffects(tr.ID) {
		return product.NewError(product.CodeReconciliationRequired, "interrupted execution has no stopped proof")
	}
	if err := rt.manager.SetTraceState(ctx, tr.ID, "cancelling", false); err != nil {
		return err
	}
	// Once cancellation intent is accepted, caller cancellation must not leave
	// the stopped trace half-finalized. No model or tool can run on this path.
	ctx = context.WithoutCancel(ctx)
	// Safe cancellation is not observed execution exit. Preserve any existing
	// stopped proof, but never manufacture one from an interrupted journal.
	for _, turn := range view.Turns {
		if turn.TraceID == tr.ID && !turn.Ended {
			if err := rt.finishInterruptedTurn(&execution{turnID: turn.ID}); err != nil {
				return err
			}
		}
	}
	return rt.manager.SetTraceState(ctx, tr.ID, "cancelled", true)
}
