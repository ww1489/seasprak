package eino

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/ww1489/seasprak/internal/agent"
)

// AbortTurnLoop requests terminal teardown, not a resumable pause. Callers must
// still wait for execution exit and reconcile any unconfirmed external effects.
func AbortTurnLoop(loop *adk.TurnLoop[agent.InputRef, *schema.AgenticMessage], cancel context.CancelFunc) {
	// Cancel the operation first. Immediate graph interruption can finish the
	// runner while an uncooperative model/tool is still running. Closing input
	// dispatch without that interruption preserves the caller's Wait barrier.
	cancel()
	loop.Stop(adk.WithSkipCheckpoint())
}
