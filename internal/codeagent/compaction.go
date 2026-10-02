package codeagent

import (
	"context"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	"github.com/ww1489/seasprak/internal/codeagent/state"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/llm"
)

// Compaction engineering defaults (08 §2): recent entries kept verbatim and
// the per-tool-result preview size fed to the summary model.
const (
	compactKeepEntries   = 4
	compactToolBytes     = 8 << 10
	compactAppendixItems = 50
)

// Automatic compaction reasons from the model boundary. "boundary" only runs
// an intent that was deferred while an approval was waiting.
const (
	reasonSoftThreshold = "soft_threshold"
	reasonOverflow      = "overflow"
	reasonBoundary      = "boundary"
)

// CompactRequest asks for an idle manual compaction of the selected branch.
type CompactRequest struct {
	IdempotencyKey string
	Reason         string
}

// compactJob is a range fixed in the mailbox; its summary is generated
// outside the mailbox and committed only if the leaf is unchanged.
type compactJob struct {
	receipt  state.OperationReceipt
	branch   string
	leaf     string
	part     agent.CompactionPartition
	previous agent.FileDetails
	facts    []agent.FileFact
	model    einomodel.AgenticModel
	// budget is the active trace ledger; nil for idle maintenance.
	budget *agent.BudgetLedger
}

type compactStart struct {
	receipt state.OperationReceipt
	job     *compactJob
}

func newCompactJob(v state.View, receipt state.OperationReceipt, part agent.CompactionPartition) *compactJob {
	job := &compactJob{receipt: receipt, branch: v.BranchID, leaf: v.LeafID, part: part}
	if part.Previous != nil && part.Previous.Summary.Files != nil {
		job.previous = *part.Previous.Summary.Files
	}
	covered := map[string]bool{}
	for _, group := range [][]agent.AgentMessage{part.H, part.P} {
		for _, m := range group {
			if m.Scope.ToolCallID != "" {
				covered[m.Scope.ToolCallID] = true
			}
		}
	}
	var calls []agent.ToolRecord
	for id, c := range v.Calls {
		if covered[id] {
			calls = append(calls, c)
		}
	}
	job.facts = agent.FileFactsFromCalls(calls)
	return job
}

// Compact durably accepts a maintenance operation, fixes the range in the
// mailbox, generates the summary outside it, and commits the summary entry and
// completion together only if the leaf is unchanged. On any failure the
// operation is failed and the previous projection stays active. While a trace
// waits for approval the intent is only registered as a deferred operation;
// it runs at the next safe boundary without touching the checkpoint.
func (s *AgentSession) Compact(ctx context.Context, req CompactRequest) (state.OperationReceipt, error) {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.writable(); err != nil {
			return nil, err
		}
		reason := req.Reason
		if reason == "" {
			reason = "manual"
		}
		content, _ := jsonStrings([]string{reason})
		cmd := state.OperationCommand{Principal: rt.opts.Principal, Kind: "compact", Target: "history", IdempotencyKey: req.IdempotencyKey, Content: content}
		receipt, found, err := rt.manager.FindOperation(cmd)
		if err != nil {
			return nil, err
		}
		if found {
			// A replay runs a still-deferred intent once the session is idle.
			if rt.manager.View().Operations[receipt.OperationID].State != "deferred" || rt.idleForHistory() != nil {
				return compactStart{receipt: receipt}, nil
			}
			job, err := rt.startDeferredCompaction(ctx, receipt.OperationID, "")
			return compactStart{receipt: receipt, job: job}, err
		}
		if v := rt.manager.View(); v.ActiveTrace != "" && rt.commandBlockedByApproval() {
			receipt, _, err := rt.manager.AcceptTraceControl(ctx, cmd, false, "", "")
			if err != nil {
				return nil, err
			}
			return compactStart{receipt: receipt}, rt.manager.TransitionOperation(ctx, receipt.OperationID, 1, "deferred", "", "")
		}
		if err := rt.idleForHistory(); err != nil {
			return nil, err
		}
		v := rt.manager.View()
		part, err := agent.PartitionForCompaction(v.Messages, compactKeepEntries)
		if err != nil {
			return nil, err
		}
		receipt, _, err = rt.manager.AcceptTraceControl(ctx, cmd, false, "", "")
		if err != nil {
			return nil, err
		}
		job := newCompactJob(v, receipt, part)
		job.model = rt.opts.Model
		return compactStart{receipt: receipt, job: job}, nil
	})
	start, _ := value.(compactStart)
	if err != nil {
		if start.receipt.OperationID != "" {
			return start.receipt, err
		}
		return state.OperationReceipt{}, err
	}
	if start.job == nil {
		return start.receipt, nil
	}
	return start.receipt, s.rt.runCompaction(ctx, *start.job, func(rt *runtime) error { return rt.idleForHistory() })
}

// startDeferredCompaction runs in the mailbox. It fixes the range for a
// deferred operation and moves it to running; a range with nothing to compact
// completes the intent as no_op without a model call. traceID selects the
// current request's prefix P when invoked at a trace boundary.
func (rt *runtime) startDeferredCompaction(ctx context.Context, operationID, traceID string) (*compactJob, error) {
	v := rt.manager.View()
	op := v.Operations[operationID]
	part, err := agent.PartitionForCompactionInTrace(v.Messages, compactKeepEntries, traceID)
	if err != nil {
		return nil, rt.manager.TransitionOperation(ctx, operationID, op.Revision, "completed", "no_op", "")
	}
	if err := rt.manager.TransitionOperation(ctx, operationID, op.Revision, "running", "", ""); err != nil {
		return nil, err
	}
	job := newCompactJob(rt.manager.View(), op.Receipt, part)
	job.model = rt.opts.Model
	return job, nil
}

// runCompaction generates outside the mailbox and commits inside it. check
// re-validates the owner (idle session or the same active execution).
func (rt *runtime) runCompaction(ctx context.Context, job compactJob, check func(*runtime) error) error {
	id := job.receipt.OperationID
	fail := func(cause error) error {
		_ = rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
			op := rt.manager.View().Operations[id]
			return rt.manager.TransitionOperation(context.WithoutCancel(ctx), id, op.Revision, "failed", "", "compaction failed")
		})
		return cause
	}
	previous := ""
	if job.part.Previous != nil {
		previous = job.part.Previous.Summary.Text
	}
	m := job.model
	budget := job.budget
	if budget == nil {
		// Idle maintenance has no trace. It still meters every summary request
		// through its own bounded ledger so observed transports admit it and
		// retries stay within the engineering request limits.
		budget = agent.NewBudget(rt.opts.Limits)
	}
	m = &chargedModel{inner: m, budget: budget, id: id}
	candidate, err := einorun.GenerateCompaction(ctx, m, previous, job.part.H, job.part.P, compactToolBytes)
	if err != nil {
		return fail(err)
	}
	files := agent.MergeFileFacts(job.previous, job.facts)
	summary := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindCompactionSummary, Status: agent.StatusComplete,
		Source: agent.SourceRef{Kind: agent.SourceModel, Description: "compaction"}, Scope: agent.MessageScope{SessionID: rt.opts.SessionID},
		Summary: &agent.SummaryMessage{Text: candidate.Text + agent.FileAppendix(files, compactAppendixItems), FirstKeptID: job.part.FirstKeptID, Files: &files, TemplateVersion: candidate.TemplateVersion}}
	err = rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := check(rt); err != nil {
			return err
		}
		return rt.manager.CommitCompaction(context.WithoutCancel(ctx), id, job.branch, job.leaf, summary)
	})
	if err != nil {
		if pe, ok := product.AsError(err); ok && pe.Code == product.CodeStorageUnavailable {
			return err
		}
		return fail(err)
	}
	return nil
}

// CompactForRequest implements agent.CompactionRequester for the active
// execution. It compacts only at a boundary where the model input is exactly
// the committed projection, charges each summary model call to the trace
// ledger, and counts automatic attempts durably per trace. It returns
// compacted=false (no error) when there is nothing to do.
func (rt *runtime) CompactForRequest(ctx context.Context, scope agent.ExecutionScope, req agent.CompactionRequest) ([]agent.AgentMessage, bool, error) {
	value, err := rt.call(ctx, func(rt *runtime) (any, error) {
		deferred, hasDeferred := rt.manager.DeferredCompaction()
		if req.Reason == reasonBoundary && !hasDeferred {
			return nil, nil
		}
		if !rt.matchesExecution(scope) {
			return nil, product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		frame := rt.active
		if err := frame.ctx.Err(); err != nil {
			return nil, err
		}
		v := rt.manager.View()
		tr := v.Traces[scope.TraceID]
		if tr == nil || tr.State != "running" {
			return nil, nil
		}
		if !hasDeferred && rt.manager.TraceCompactions(tr.ID) >= tr.Limits.TraceCompactions {
			return nil, nil
		}
		projected, err := agent.ConvertToLLM(agent.ProjectHistory(v.Messages))
		if err != nil || len(projected) != req.VisibleMessages {
			return nil, nil // in-flight messages are not all committed yet
		}
		var job *compactJob
		if hasDeferred {
			if job, err = rt.startDeferredCompaction(ctx, deferred.Receipt.OperationID, tr.ID); err != nil || job == nil {
				return nil, err
			}
		} else {
			part, err := agent.PartitionForCompactionInTrace(v.Messages, compactKeepEntries, tr.ID)
			if err != nil {
				return nil, nil // no_op: no model call, no operation
			}
			content, _ := jsonStrings([]string{req.Reason})
			cmd := state.OperationCommand{Principal: rt.opts.Principal, Kind: "compact", Target: tr.ID, IdempotencyKey: "auto:" + agent.MustID(), Content: content}
			receipt, _, err := rt.manager.AcceptTraceControl(ctx, cmd, false, "", "")
			if err != nil {
				return nil, err
			}
			job = newCompactJob(v, receipt, part)
		}
		job.model = frame.currentModel
		job.budget = frame.budget
		return job, nil
	})
	if err != nil || value == nil {
		return nil, false, err
	}
	job := value.(*compactJob)
	if err := rt.runCompaction(ctx, *job, func(rt *runtime) error {
		if !rt.matchesExecution(scope) {
			return product.NewError(product.CodeStateConflict, "execution is no longer active")
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	value, err = rt.call(ctx, func(rt *runtime) (any, error) {
		return agent.ProjectHistory(rt.manager.View().Messages), nil
	})
	if err != nil {
		return nil, false, err
	}
	return value.([]agent.AgentMessage), true, nil
}

// chargedModel charges each summary model call to the shared trace ledger
// before it is made, like a workflow node call. It creates no Turn.
type chargedModel struct {
	inner  einomodel.AgenticModel
	budget *agent.BudgetLedger
	id     string
}

func (m *chargedModel) charge(ctx context.Context) (context.Context, error) {
	if m.inner == nil {
		return ctx, product.NewError(product.CodeResourceUnavailable, "model instance is unavailable")
	}
	observed := llm.UsesObservedTransport(m.inner)
	transport := 1
	if observed {
		transport = 0 // each physical request is charged by the transport observer
	}
	if err := m.budget.ChargeDelegated(1, transport); err != nil {
		return ctx, err
	}
	// An overflow recovery runs inside the failed agent attempt's context;
	// the summary's usage must not be reported as that attempt's usage.
	ctx = llm.WithUsageObservation(ctx, nil)
	if observed {
		ctx = llm.WithRequestObservation(ctx, llm.RequestIdentity{ModelCallID: m.id, AttemptID: m.id + ":" + agent.MustID(), Purpose: "compaction"}, compactionTransport{budget: m.budget})
	}
	return ctx, nil
}

func (m *chargedModel) Generate(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.AgenticMessage, error) {
	ctx, err := m.charge(ctx)
	if err != nil {
		return nil, err
	}
	return m.inner.Generate(ctx, in, opts...)
}

func (m *chargedModel) Stream(ctx context.Context, in []*schema.AgenticMessage, opts ...einomodel.Option) (*schema.StreamReader[*schema.AgenticMessage], error) {
	ctx, err := m.charge(ctx)
	if err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, in, opts...)
}

// compactionTransport charges each physical summary request to the trace.
type compactionTransport struct{ budget *agent.BudgetLedger }

func (t compactionTransport) BeforeRequest(context.Context, llm.TransportRequest) error {
	return t.budget.ChargeDelegated(0, 1)
}
