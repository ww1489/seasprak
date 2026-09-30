package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

// ReconcileQuery is a trusted, read-only evidence provider registered by the
// host. It must inspect evidence only; it must not invoke the original tool,
// start a process, or mutate the session.
type ReconcileQuery interface {
	Query(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error)
}

type ReconcileQueryFunc func(context.Context, ReconcileQueryRequest) (ReconcileEvidence, error)

func (f ReconcileQueryFunc) Query(ctx context.Context, request ReconcileQueryRequest) (ReconcileEvidence, error) {
	return f(ctx, request)
}

// OriginalGrantRef asks Reconcile to use the grant of the original frozen call.
const OriginalGrantRef = "\x00original"

type ReconcileCommand struct {
	TraceID            string `json:"traceId"`
	InvocationID       string `json:"invocationId"`
	CallID             string `json:"callId"`
	ObservationID      string `json:"observationId"`
	ObservationVersion uint64 `json:"observationVersion,omitempty"`
	ExpectedRevision   uint64 `json:"expectedRevision"`
	QueryID            string `json:"queryId,omitempty"`
	EvidenceRef        string `json:"evidenceRef,omitempty"`
	GrantRef           string `json:"grantRef,omitempty"`
	IdempotencyKey     string `json:"idempotencyKey,omitempty"`
}

type ReconcileQueryRequest struct {
	SessionID          string `json:"sessionId"`
	TraceID            string `json:"traceId"`
	InvocationID       string `json:"invocationId"`
	CallID             string `json:"callId"`
	ObservationID      string `json:"observationId"`
	ObservationVersion uint64 `json:"observationVersion"`
	EvidenceRef        string `json:"evidenceRef,omitempty"`
	OriginalGrantRef   string `json:"originalGrantRef,omitempty"`
}

type ReconcileEvidence struct {
	EvidenceRefs         []string `json:"evidenceRefs"`
	EvidenceSource       string   `json:"evidenceSource"`
	ConfirmedEffects     []string `json:"confirmedEffects,omitempty"`
	RemainingUnknown     []string `json:"remainingUnknown,omitempty"`
	ConflictRestrictions []string `json:"conflictRestrictions,omitempty"`
	TrustedNoStart       bool     `json:"trustedNoStart,omitempty"`
	ConfirmedExecution   bool     `json:"confirmedExecution,omitempty"`
}

func (e ReconcileEvidence) clone() ReconcileEvidence {
	e.EvidenceRefs = append([]string(nil), e.EvidenceRefs...)
	e.ConfirmedEffects = append([]string(nil), e.ConfirmedEffects...)
	e.RemainingUnknown = append([]string(nil), e.RemainingUnknown...)
	e.ConflictRestrictions = append([]string(nil), e.ConflictRestrictions...)
	return e
}

// PendingReconciliation is the identity of a claimed call whose latest
// observation Reconcile would currently accept.
type PendingReconciliation struct {
	TraceID, InvocationID, CallID, ObservationID string
	ObservationVersion                           uint64
}

// pendingReconciliations mirrors the eligibility checks of validateReconcile.
func pendingReconciliations(view state.View) []PendingReconciliation {
	latest := map[string]state.ObservationRevision{}
	for _, r := range view.Observations {
		if r.Version > latest[r.CallID].Version {
			latest[r.CallID] = r
		}
	}
	out := []PendingReconciliation{}
	for id, call := range view.Calls {
		r, ok := latest[id]
		tr := view.Traces[call.Scope.TraceID]
		if !ok || !call.Claimed || tr == nil || tr.InvocationID != call.Scope.InvocationID || (tr.State != "paused" && tr.State != "cancelling" && !terminal(tr.State)) {
			continue
		}
		if r.Observation.SideEffect != "unknown" && r.Observation.Status != "outcome_unknown" && !view.ReconciliationUnresolved(id) {
			continue
		}
		out = append(out, PendingReconciliation{TraceID: call.Scope.TraceID, InvocationID: call.Scope.InvocationID, CallID: id, ObservationID: r.ID, ObservationVersion: r.Version})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TraceID != out[j].TraceID {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].CallID < out[j].CallID
	})
	return out
}

func (rt *runtime) validateReconcile(view state.View, cmd ReconcileCommand) (state.ObservationRevision, state.TraceState, error) {
	tr := view.Traces[cmd.TraceID]
	if tr == nil {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeNotFound, "trace not found")
	}
	if cmd.InvocationID == "" || cmd.CallID == "" || cmd.ObservationID == "" || cmd.TraceID == "" {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeInvalidArgument, "reconciliation identity is required")
	}
	if tr.InvocationID != cmd.InvocationID || (tr.State != "paused" && tr.State != "cancelling" && !terminal(tr.State)) {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeStateConflict, "trace is not eligible for reconciliation")
	}
	call, ok := view.Calls[cmd.CallID]
	if !ok || call.Scope.TraceID != cmd.TraceID || call.Scope.InvocationID != cmd.InvocationID || !call.Claimed {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeStateConflict, "reconciliation call is not the original claimed call")
	}
	if cmd.GrantRef != frozenGrant(view, cmd.CallID) {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeStateConflict, "reconciliation grant reference does not match the original call")
	}
	latest, err := rt.manager.LatestObservation(cmd.CallID)
	if err != nil {
		return state.ObservationRevision{}, state.TraceState{}, err
	}
	if latest.ID != cmd.ObservationID || (cmd.ObservationVersion != 0 && latest.Version != cmd.ObservationVersion) || latest.Version == 0 || (latest.Observation.SideEffect != "unknown" && latest.Observation.Status != "outcome_unknown" && !view.ReconciliationUnresolved(cmd.CallID)) {
		return state.ObservationRevision{}, state.TraceState{}, product.NewError(product.CodeStateConflict, "reconciliation observation is not unresolved")
	}
	return latest, *tr, nil
}

func reconcileCommandContent(cmd ReconcileCommand) ([]byte, error) {
	return json.Marshal(struct {
		TraceID, InvocationID, CallID, ObservationID, QueryID, EvidenceRef, GrantRef string
		ObservationVersion                                                           uint64
	}{cmd.TraceID, cmd.InvocationID, cmd.CallID, cmd.ObservationID, cmd.QueryID, cmd.EvidenceRef, cmd.GrantRef, cmd.ObservationVersion})
}

func (s *AgentSession) Reconcile(ctx context.Context, cmd ReconcileCommand) (state.OperationReceipt, error) {
	if err := ctx.Err(); err != nil {
		return state.OperationReceipt{}, err
	}
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		if rt.opts.Principal == "" {
			return nil, product.NewError(product.CodePermissionDenied, "reconciliation requires a trusted principal")
		}
		if (cmd.QueryID == "") == (cmd.EvidenceRef == "") {
			return nil, product.NewError(product.CodeInvalidArgument, "exactly one reconciliation evidence source is required")
		}
		// Network callers cannot know the internal grant; resolve it from the
		// original frozen call before digesting so replays stay equivalent.
		if cmd.GrantRef == OriginalGrantRef {
			cmd.GrantRef = frozenGrant(rt.manager.View(), cmd.CallID)
		}
		content, err := reconcileCommandContent(cmd)
		if err != nil {
			return nil, err
		}
		view := rt.manager.View()
		if receipt, found, err := rt.manager.FindOperation(state.OperationCommand{Principal: rt.opts.Principal, Kind: "reconcile", Target: cmd.CallID, IdempotencyKey: cmd.IdempotencyKey, ExpectedRevision: cmd.ExpectedRevision, Content: content}); found || err != nil {
			if err != nil {
				return nil, err
			}
			status, statusErr := rt.manager.GetOperation(receipt.OperationID)
			if statusErr != nil {
				return nil, statusErr
			}
			return reconcileAccepted{receipt: receipt, status: status, done: true}, nil
		}
		latest, trace, err := rt.validateReconcile(view, cmd)
		if err != nil {
			return nil, err
		}
		operation := state.OperationCommand{Principal: rt.opts.Principal, Kind: "reconcile", Target: cmd.CallID, ExpectedRevision: cmd.ExpectedRevision, IdempotencyKey: cmd.IdempotencyKey, Content: content}
		receipt, err := rt.manager.AcceptOperation(ctx, operation)
		if err != nil {
			return nil, err
		}
		status, err := rt.manager.GetOperation(receipt.OperationID)
		if err != nil {
			return nil, err
		}
		if status.State == "accepted" {
			if err := rt.manager.TransitionOperation(context.WithoutCancel(ctx), receipt.OperationID, status.Revision, "running", "", ""); err != nil {
				return nil, err
			}
			status, err = rt.manager.GetOperation(receipt.OperationID)
			if err != nil {
				return nil, err
			}
		}
		return reconcileAccepted{receipt: receipt, status: status, initial: latest, request: ReconcileQueryRequest{SessionID: rt.opts.SessionID, TraceID: cmd.TraceID, InvocationID: cmd.InvocationID, CallID: cmd.CallID, ObservationID: latest.ID, ObservationVersion: latest.Version, EvidenceRef: cmd.EvidenceRef, OriginalGrantRef: frozenGrant(view, cmd.CallID)}, executionStopped: trace.ExecutionStopped, executionID: trace.ExecutionID, traceState: trace.State, done: false}, nil
	})
	if err != nil {
		return state.OperationReceipt{}, err
	}
	accepted := value.(reconcileAccepted)
	if accepted.done {
		return accepted.receipt, nil
	}

	evidence, queryErr := s.queryReconcile(ctx, cmd, accepted.request)
	if queryErr != nil {
		_ = s.finishReconcileFailure(accepted.receipt.OperationID, accepted.status.Revision, queryErr)
		return accepted.receipt, queryErr
	}
	if !accepted.executionStopped && (evidence.TrustedNoStart || evidence.ConfirmedExecution) {
		evidence.TrustedNoStart = false
		evidence.ConfirmedExecution = false
		evidence.RemainingUnknown = append(evidence.RemainingUnknown, "execution was not durably stopped")
		evidence.ConflictRestrictions = append(evidence.ConflictRestrictions, "terminal reconciliation facts require stopped execution")
	}
	result, next, release, err := reconcileResult(accepted.initial, evidence, cmd.QueryID)
	if err != nil {
		_ = s.finishReconcileFailure(accepted.receipt.OperationID, accepted.status.Revision, err)
		return accepted.receipt, err
	}
	result.OperationID = accepted.receipt.OperationID
	result.QueryID = cmd.QueryID
	result.GrantRef = accepted.request.OriginalGrantRef
	value, err = s.rt.call(context.WithoutCancel(ctx), func(rt *runtime) (any, error) {
		view := rt.manager.View()
		trace := view.Traces[cmd.TraceID]
		if trace == nil || trace.InvocationID != cmd.InvocationID || trace.State != accepted.traceState || trace.ExecutionStopped != accepted.executionStopped || trace.ExecutionID != accepted.executionID {
			return nil, product.NewError(product.CodeStateConflict, "reconciliation trace changed while evidence was queried")
		}
		if view.ReconciliationUnresolved(cmd.CallID) {
			if err := rt.restoreResourceHolds(); err != nil {
				return nil, err
			}
		}
		status, err := rt.manager.GetOperation(accepted.receipt.OperationID)
		if err != nil {
			return nil, err
		}
		if err := rt.manager.CommitReconciliation(context.WithoutCancel(ctx), accepted.receipt.OperationID, status.Revision, next, result); err != nil {
			return nil, err
		}
		if release {
			if err := rt.resourceScheduler().ReleaseHold(toolsResourceHoldID(rt.opts.SessionID, cmd.CallID)); err != nil {
				return nil, err
			}
		}
		return result, nil
	})
	if err != nil {
		_ = s.finishReconcileFailure(accepted.receipt.OperationID, accepted.status.Revision, err)
		return accepted.receipt, err
	}
	_ = value
	return accepted.receipt, nil
}

type reconcileAccepted struct {
	receipt          state.OperationReceipt
	status           state.OperationStatus
	initial          state.ObservationRevision
	request          ReconcileQueryRequest
	executionStopped bool
	executionID      string
	traceState       string
	done             bool
}

func (s *AgentSession) queryReconcile(ctx context.Context, cmd ReconcileCommand, request ReconcileQueryRequest) (ReconcileEvidence, error) {
	if cmd.QueryID == "" {
		return ReconcileEvidence{EvidenceRefs: []string{cmd.EvidenceRef}, EvidenceSource: "evidence:" + cmd.EvidenceRef}, nil
	}
	query := s.rt.opts.ReconcileQueries[cmd.QueryID]
	if query == nil {
		return ReconcileEvidence{}, product.NewError(product.CodeUnsupportedCapability, "reconciliation query is unavailable")
	}
	return query.Query(ctx, request)
}

func (s *AgentSession) finishReconcileFailure(id string, revision uint64, err error) error {
	return s.rt.do(context.WithoutCancel(context.Background()), func(rt *runtime) error {
		return rt.manager.TransitionOperation(context.Background(), id, revision, "failed", "", safeReconcileError(err))
	})
}

func safeReconcileError(err error) string {
	if err == nil {
		return "reconciliation failed"
	}
	if pe, ok := product.AsError(err); ok {
		return pe.Code
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "reconciliation query failed"
}

func reconcileResult(previous state.ObservationRevision, evidence ReconcileEvidence, queryID string) (state.Reconciliation, state.ObservationRevision, bool, error) {
	evidence = evidence.clone()
	if evidence.EvidenceSource == "" {
		if queryID != "" {
			evidence.EvidenceSource = "query:" + queryID
		} else {
			return state.Reconciliation{}, state.ObservationRevision{}, false, product.NewError(product.CodeInvalidArgument, "reconciliation evidence source is required")
		}
	}
	if len(evidence.EvidenceRefs) == 0 {
		return state.Reconciliation{}, state.ObservationRevision{}, false, product.NewError(product.CodeInvalidArgument, "reconciliation evidence reference is required")
	}
	if evidence.TrustedNoStart && queryID == "" {
		return state.Reconciliation{}, state.ObservationRevision{}, false, product.NewError(product.CodePermissionDenied, "no-start requires a trusted reconciliation query")
	}
	unknown := append([]string(nil), evidence.RemainingUnknown...)
	if evidence.TrustedNoStart && (evidence.ConfirmedExecution || len(evidence.ConfirmedEffects) > 0) {
		unknown = append(unknown, "no-start and confirmed execution conflict")
		evidence.ConflictRestrictions = append(evidence.ConflictRestrictions, "conflicting execution evidence requires further reconciliation")
	}
	if !evidence.TrustedNoStart && !evidence.ConfirmedExecution && len(unknown) == 0 {
		unknown = []string{"effect remains unknown"}
	}
	observation := previous.Observation
	release := false
	switch {
	case len(unknown) > 0 || len(evidence.ConflictRestrictions) > 0:
		observation = agent.ToolObservation{Status: "outcome_unknown", Content: "reconciliation did not establish a terminal effect", SideEffect: "unknown", Executed: true}
	case evidence.TrustedNoStart:
		observation = agent.ToolObservation{Status: "cancelled", Content: "trusted no-start evidence", SideEffect: "none", Executed: false}
		release = true
	case evidence.ConfirmedExecution:
		observation = agent.ToolObservation{Status: "succeeded", Content: "reconciled execution confirmed", SideEffect: "confirmed", Executed: true}
	default:
		observation = agent.ToolObservation{Status: "outcome_unknown", Content: "reconciliation did not establish a terminal effect", SideEffect: "unknown", Executed: true}
	}
	nextID := "observation:" + agent.MustID()
	reconciliationID := "reconciliation:" + agent.MustID()
	next := state.ObservationRevision{ID: nextID, CallID: previous.CallID, Version: previous.Version + 1, PreviousID: previous.ID, Observation: observation, DetailsRef: evidence.EvidenceSource, ArtifactRefs: append([]string(nil), evidence.EvidenceRefs...)}
	result := state.Reconciliation{ID: reconciliationID, CallID: previous.CallID, ObservationID: previous.ID, ObservationVersion: previous.Version, NewObservationID: next.ID, EvidenceRefs: append([]string(nil), evidence.EvidenceRefs...), EvidenceSource: evidence.EvidenceSource, ConfirmedEffects: append([]string(nil), evidence.ConfirmedEffects...), RemainingUnknown: unknown, ConflictRestrictions: append([]string(nil), evidence.ConflictRestrictions...), CanResume: false, ResumeReason: "resume compatibility must be revalidated"}
	if release {
		result.TrustedNoStart = true
		result.ResumeReason = "trusted no-start recorded; explicit resume is not automatic"
	}
	return result, next, release, nil
}

func frozenGrant(view state.View, callID string) string {
	if frozen, ok := view.FrozenExecutions["execution:"+callID]; ok {
		return frozen.RequestedGrantRef
	}
	return ""
}

func toolsResourceHoldID(sessionID, callID string) string {
	return sessionID + "\x00" + callID
}
