package state

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

type Manager struct {
	mu        sync.Mutex
	store     store.Store
	sessionID string
	view      *View
	fault     error
}

func NewManager(store store.Store, id string) (*Manager, error) {
	stored, err := store.Load(context.Background(), id)
	if err != nil {
		return nil, err
	}
	v := emptyView()
	v.RepairRequired = stored.RepairRequired
	for _, c := range stored.Commits {
		if err := ValidateHostCommandCommit(*v, c, id); err != nil {
			return nil, err
		}
		if err := validateHostCommandConsumptionCommit(*v, c, id); err != nil {
			return nil, err
		}
		if err := applyCommit(v, c); err != nil {
			return nil, err
		}
	}
	// Loading is diagnostic only: an unclosed reservation is conservative
	// crash occupancy, never evidence of elapsed downtime or execution exit.
	for _, tr := range v.Traces {
		tr.Activity.Uncertain += tr.Activity.Reserved
		tr.Activity.Reserved = 0
		tr.Activity.ExecutionID = ""
	}
	return &Manager{store: store, sessionID: id, view: v}, nil
}
func emptyView() *View {
	return &View{BranchID: "main", Nodes: map[string]HistoryNode{}, Branches: map[string]BranchHead{"main": {}}, Traces: map[string]*TraceState{}, Inputs: map[string]*InputState{}, Idem: map[string]idemRecord{}, Turns: map[string]agent.TurnRecord{}, Calls: map[string]agent.ToolRecord{}, Operations: map[string]Operation{}, Observations: map[string]ObservationRevision{}, ToolProjections: map[string]agent.ToolOutputProjection{}, Reconciliations: map[string]Reconciliation{}, ModelAttempts: map[string]ModelAttempt{}, AttemptResults: map[string]ModelAttemptTransition{}, AttemptDetails: map[string]ModelAttemptDetailsRecord{}, FrozenExecutions: map[string]FrozenExecution{}, Interactions: map[string]Interaction{}, Approvals: map[string]Approval{}, ApprovalBindings: map[string]ApprovalBinding{}, ApprovalDecisions: map[string]ApprovalDecision{}, ApprovalClaims: map[string]ApprovalClaim{}, Selections: map[string]Selection{}, Checkpoints: map[string]CheckpointRef{}, ResumedExecutions: map[string]ResumedExecution{}, Invocations: map[string]Invocation{}, WorkflowNodes: map[string]WorkflowNodeRun{}}
}
func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

// UnfinishedModelAttempt reports only missing durable terminal evidence.
// It does not infer a crash, success, or execution exit.
type UnfinishedModelAttempt struct {
	AttemptID string `json:"attemptId"`
	Reason    string `json:"reason"`
}

func (m *Manager) View() View {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := clone(*m.view)
	for id, initial := range v.ModelAttempts {
		if _, ended := v.AttemptResults[id]; initial.State == "started" && !ended {
			v.UnfinishedAttempts = append(v.UnfinishedAttempts, UnfinishedModelAttempt{AttemptID: id, Reason: "terminal_not_recorded"})
		}
	}
	sort.Slice(v.UnfinishedAttempts, func(i, j int) bool { return v.UnfinishedAttempts[i].AttemptID < v.UnfinishedAttempts[j].AttemptID })
	return v
}
func (m *Manager) Fault() error { m.mu.Lock(); defer m.mu.Unlock(); return m.fault }

// WriteStatus preserves fault-before-repair precedence without copying a View.
func (m *Manager) WriteStatus() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault != nil {
		return m.fault
	}
	if m.view.RepairRequired {
		return product.NewError(product.CodeStorageUnavailable, "journal requires repair")
	}
	return nil
}
func record(kind, id string, v any) store.Record {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return store.Record{Type: kind, Version: 1, ID: id, Payload: raw}
}
func (m *Manager) event(kind, trace, turn string, v any) agent.Event {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	// Durable events are display payloads; history entries retain all private
	// protocol data. Decode also covers finalized tool results passed as JSON.
	if kind == "message.finalized" {
		var msg agent.AgentMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			panic(err)
		}
		raw, err = json.Marshal(agent.PublicMessage(msg))
		if err != nil {
			panic(err)
		}
	}
	return agent.Event{SchemaVersion: 1, Type: kind, Scope: agent.EventScope{SessionID: m.sessionID, TraceID: trace, TurnID: turn}, EventID: agent.MustID(), OccurredAt: time.Now().UTC(), Payload: raw}
}

// commit validates a private candidate. Neither state nor events become visible before Append succeeds.
func (m *Manager) commit(ctx context.Context, controls, entries []store.Record, events []agent.Event) (store.CommitReceipt, error) {
	return m.commitWithCandidate(ctx, controls, entries, events, false)
}

// Both candidate strategies use the same validation and publication boundary.
// activityOnly is reserved for ReserveActivity/SettleActivity and is checked
// against the complete commit before any shallow candidate can be applied.
func (m *Manager) commitWithCandidate(ctx context.Context, controls, entries []store.Record, events []agent.Event, activityOnly bool) (store.CommitReceipt, error) {
	if m.fault != nil {
		return store.CommitReceipt{}, m.fault
	}
	if m.view.RepairRequired {
		return store.CommitReceipt{}, product.NewError(product.CodeStorageUnavailable, "journal requires repair")
	}
	if err := ctx.Err(); err != nil {
		return store.CommitReceipt{}, err
	}
	c := store.Commit{RecordType: "commit", Version: 1, CommitID: agent.MustID(), CommitSeq: m.view.LastSeq + 1, ExpectedPreviousSeq: m.view.LastSeq, ControlRecords: controls, Entries: entries, Events: events}
	for i := range c.Events {
		seq := m.view.Cursor + uint64(i) + 1
		c.Events[i].DurableSeq = &seq
	}
	if err := ValidateHostCommandCommit(*m.view, c, m.sessionID); err != nil {
		return store.CommitReceipt{}, err
	}
	if err := validateHostCommandConsumptionCommit(*m.view, c, m.sessionID); err != nil {
		return store.CommitReceipt{}, err
	}
	var candidate View
	if activityOnly {
		var err error
		candidate, err = m.activityCandidate(c)
		if err != nil {
			return store.CommitReceipt{}, err
		}
	} else {
		candidate = clone(*m.view)
	}
	if err := applyCommit(&candidate, c); err != nil {
		return store.CommitReceipt{}, err
	}
	receipt, err := m.store.Append(ctx, m.sessionID, store.ExpectedCommit{ExpectedPreviousSeq: m.view.LastSeq}, c)
	if err != nil {
		m.fault = product.NewError(product.CodeStorageUnavailable, "session commit failed; reopen required")
		return store.CommitReceipt{}, err
	}
	m.view = &candidate
	return receipt, nil
}
