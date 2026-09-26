package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// BudgetLedger counts shared trace usage. Occupancy happens before the call.
// SetPersist must not call back into the same ledger.
type BudgetLedger struct {
	mu       sync.Mutex
	limits   config.Limits
	used     Usage
	turnUsed int
	persist  func(Usage) error
}

type Usage struct {
	LogicalModelCalls int
	TransportRequests int
	ToolExecutions    int
	ModelCallID       string
	ModelRequests     int
	LastTransport     llm.TransportRequest
}

// ClaimReceipt is an opaque, process-local proof that the durable tool intent
// and its budget were committed together. Only BudgetLedger.ClaimTool can mint
// one, and each receipt may authorize at most one execution ticket.
type ClaimReceipt struct {
	claim *committedToolClaim
}

type committedToolClaim struct {
	ledger     *BudgetLedger
	callID     string
	scope      ExecutionScope
	frozenHash string
	used       atomic.Bool
}

func (r ClaimReceipt) Issued() bool { return r.claim != nil }

func (r ClaimReceipt) consume(callID string, scope ExecutionScope, frozenHash string) bool {
	claim := r.claim
	return claim != nil && claim.ledger != nil && claim.callID == callID && claim.scope == scope && claim.frozenHash == frozenHash && claim.used.CompareAndSwap(false, true)
}

func NewBudget(limits config.Limits) *BudgetLedger {
	return &BudgetLedger{limits: limits.WithDefaults()}
}

func (b *BudgetLedger) SetPersist(fn func(Usage) error) {
	b.mu.Lock()
	b.persist = fn
	b.mu.Unlock()
}

func (b *BudgetLedger) Limits() config.Limits {
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
	b.turnUsed = u.ModelRequests
	b.mu.Unlock()
}

func (b *BudgetLedger) BeginTurn() error {
	return b.BeginTurnID(MustID())
}

// BeginTurnID reuses a logical call on explicit recovery without refunding requests.
func (b *BudgetLedger) BeginTurnID(id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id == "" {
		return product.NewError(product.CodeInvalidArgument, "model call identity is required")
	}
	if b.used.ModelCallID == id {
		return nil
	}
	if b.used.LogicalModelCalls >= b.limits.TraceLogicalModelCalls {
		return product.NewError(product.CodeBudgetExhausted, "model budget exhausted")
	}
	next := b.used
	next.LogicalModelCalls++
	next.ModelCallID = id
	next.LastTransport = llm.TransportRequest{}
	return b.commit(next, 0)
}

// BeforeRequest will atomically reserve a physical request using this ledger's
// existing persistence callback. It is the L2 implementation of the L1 port.
func (b *BudgetLedger) BeforeRequest(ctx context.Context, request llm.TransportRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.ModelCallID == "" || request.AttemptID == "" || request.Purpose == "" || request.TransportAttempt == 0 {
		return product.NewError(product.CodeInvalidArgument, "transport identity is required")
	}
	if request.ModelCallID != b.used.ModelCallID {
		return product.NewError(product.CodeStateConflict, "transport does not belong to active logical call")
	}
	if b.turnUsed >= b.limits.LogicalModelRequests || b.used.TransportRequests >= b.limits.TraceTransportRequests {
		return product.NewError(product.CodeBudgetExhausted, "model budget exhausted")
	}
	next := b.used
	next.TransportRequests++
	next.LastTransport = request
	return b.commit(next, b.turnUsed+1)
}

func (b *BudgetLedger) OccupyModel() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.turnUsed >= b.limits.LogicalModelRequests || b.used.TransportRequests >= b.limits.TraceTransportRequests {
		return product.NewError(product.CodeBudgetExhausted, "model budget exhausted")
	}
	next := b.used
	next.TransportRequests++
	next.LastTransport = llm.TransportRequest{}
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
		return product.NewError(product.CodeBudgetExhausted, "tool budget exhausted")
	}
	next := b.used
	next.ToolExecutions++
	return b.commit(next, b.turnUsed)
}

// ClaimTool commits the only tool-start fact and its budget candidate together.
// The sink must not call back into this ledger; it owns durable claim arbitration.
func (b *BudgetLedger) ClaimTool(ctx context.Context, sink ExecutionSink, scope ExecutionScope, call FrozenCall, frozen FrozenExecution) (ClaimReceipt, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return ClaimReceipt{}, err
	}
	if frozen.CallID == "" || call.CallID != frozen.CallID || frozen.Hash == "" {
		return ClaimReceipt{}, product.NewError(product.CodePermissionDenied, "tool claim binding is incomplete")
	}
	digest, err := frozen.Digest()
	if err != nil || digest != frozen.Hash {
		return ClaimReceipt{}, product.NewError(product.CodePermissionDenied, "tool claim description is invalid")
	}
	if b.used.ToolExecutions >= b.limits.TraceToolCalls {
		return ClaimReceipt{}, product.NewError(product.CodeBudgetExhausted, "tool budget exhausted")
	}
	next := b.used
	next.ToolExecutions++
	body, err := json.Marshal(call)
	if err != nil {
		return ClaimReceipt{}, err
	}
	if err := sink.CommitFact(ctx, scope, Fact{Kind: "tool_intent", Payload: body, Budget: &next}); err != nil {
		return ClaimReceipt{}, err
	}
	b.used = next
	return ClaimReceipt{claim: &committedToolClaim{ledger: b, callID: frozen.CallID, scope: frozen.Scope, frozenHash: frozen.Hash}}, nil
}

func (b *BudgetLedger) commit(next Usage, turnUsed int) error {
	next.ModelRequests = turnUsed
	if b.persist != nil {
		if err := b.persist(next); err != nil {
			return err
		}
	}
	b.used = next
	b.turnUsed = turnUsed
	return nil
}
