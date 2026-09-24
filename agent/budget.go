package agent

import (
	"sync"

	"github.com/ww1489/seasprak/model"
)

// BudgetLedger counts shared trace usage. Occupancy happens before the call.
// SetPersist must not call back into the same ledger.
type BudgetLedger struct {
	mu       sync.Mutex
	limits   model.Limits
	used     Usage
	turnUsed int
	persist  func(Usage) error
}

type Usage struct {
	LogicalModelCalls int
	TransportRequests int
	ToolExecutions    int
}

func NewBudget(limits model.Limits) *BudgetLedger {
	return &BudgetLedger{limits: limits.WithDefaults()}
}

func (b *BudgetLedger) SetPersist(fn func(Usage) error) {
	b.mu.Lock()
	b.persist = fn
	b.mu.Unlock()
}

func (b *BudgetLedger) Limits() model.Limits {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limits
}

func (b *BudgetLedger) Snapshot() Usage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

func (b *BudgetLedger) Restore(u Usage) {
	b.mu.Lock()
	b.used = u
	b.turnUsed = 0
	b.mu.Unlock()
}

func (b *BudgetLedger) BeginTurn() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used.LogicalModelCalls >= b.limits.TraceLogicalModelCalls {
		return model.NewError(model.CodeBudgetExhausted, "model budget exhausted")
	}
	next := b.used
	next.LogicalModelCalls++
	return b.commit(next, 0)
}

func (b *BudgetLedger) OccupyModel() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turnUsed >= b.limits.LogicalModelRequests || b.used.TransportRequests >= b.limits.TraceTransportRequests {
		return model.NewError(model.CodeBudgetExhausted, "model budget exhausted")
	}
	next := b.used
	next.TransportRequests++
	return b.commit(next, b.turnUsed+1)
}

// ModelRetryAllowed reports room for another transport attempt in this turn.
// Logical turn count is not a retry limit.
func (b *BudgetLedger) ModelRetryAllowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turnUsed < b.limits.LogicalModelRequests && b.used.TransportRequests < b.limits.TraceTransportRequests
}

func (b *BudgetLedger) OccupyTool() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used.ToolExecutions >= b.limits.TraceToolCalls {
		return model.NewError(model.CodeBudgetExhausted, "tool budget exhausted")
	}
	next := b.used
	next.ToolExecutions++
	return b.commit(next, b.turnUsed)
}

func (b *BudgetLedger) commit(next Usage, turnUsed int) error {
	if b.persist != nil {
		if err := b.persist(next); err != nil {
			return err
		}
	}
	b.used = next
	b.turnUsed = turnUsed
	return nil
}
