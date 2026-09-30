package state

import (
	"context"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// CommitCompaction appends the summary entry on top of expectedLeaf and
// completes the accepted (or running deferred) operation in the same commit.
// It rejects the candidate if the selected branch or leaf moved after the
// range was fixed.
func (m *Manager) CommitCompaction(ctx context.Context, operationID, branchID, expectedLeaf string, summary agent.AgentMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.view.BranchID != branchID || m.view.LeafID != expectedLeaf {
		return product.NewError(product.CodeStateConflict, "history changed while the compaction candidate was generated")
	}
	if summary.Kind != agent.KindCompactionSummary || summary.Summary == nil || summary.Summary.FirstKeptID == "" {
		return product.NewError(product.CodeInvalidArgument, "compaction entry is invalid")
	}
	if err := summary.Validate(); err != nil {
		return err
	}
	op, ok := m.view.Operations[operationID]
	if !ok || op.Kind != "compact" || (op.State != "accepted" && op.State != "running") {
		return product.NewError(product.CodeStateConflict, "compaction operation is not accepted")
	}
	op.Revision++
	op.State, op.ResultRef = "completed", summary.ID
	entry := record("message", summary.ID, summary)
	entry.ParentID = expectedLeaf
	_, err := m.commit(ctx, []store.Record{record("operation", operationID, op)}, []store.Record{entry},
		[]agent.Event{m.event("compaction.completed", "", "", map[string]string{"operationId": operationID, "entryId": summary.ID, "firstKeptId": summary.Summary.FirstKeptID})})
	return err
}

// TraceCompactions counts the automatic compaction attempts durably accepted
// for traceID. Automatic operations target the trace; manual and deferred
// maintenance operations target "history" and are not counted.
func (m *Manager) TraceCompactions(traceID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, op := range m.view.Operations {
		if op.Kind == "compact" && op.Receipt.Target == traceID {
			n++
		}
	}
	return n
}

// DeferredCompaction returns the oldest compaction intent registered while an
// approval was waiting.
func (m *Manager) DeferredCompaction() (Operation, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found Operation
	ok := false
	for _, op := range m.view.Operations {
		if op.Kind == "compact" && op.State == "deferred" && (!ok || op.Receipt.AcceptedCommit < found.Receipt.AcceptedCommit) {
			found, ok = op, true
		}
	}
	return found, ok
}
