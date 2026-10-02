package codeagent

import (
	"context"
	"slices"

	"github.com/ww1489/seasprak/internal/agent"
	einorun "github.com/ww1489/seasprak/internal/agent/eino"
	product "github.com/ww1489/seasprak/internal/errors"
	sessstore "github.com/ww1489/seasprak/internal/storage"
)

// BranchView is the public projection of one branch head.
type BranchView struct {
	BranchID string
	LeafID   string
	ForkedAt string
	Active   bool
}

// MessagePage is a stable page of the selected or named branch path. Cursors
// are entry IDs, never array indexes.
type MessagePage struct {
	Messages []agent.AgentMessage
	Next     string
}

// ListBranches reads branch heads without writing.
func (s *AgentSession) ListBranches(ctx context.Context) ([]BranchView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	heads, active := s.rt.manager.BranchHeads()
	out := make([]BranchView, 0, len(heads))
	for id, h := range heads {
		out = append(out, BranchView{BranchID: id, LeafID: h.LeafID, ForkedAt: h.ForkedAt, Active: id == active})
	}
	slices.SortFunc(out, func(a, b BranchView) int {
		if a.BranchID < b.BranchID {
			return -1
		}
		if a.BranchID > b.BranchID {
			return 1
		}
		return 0
	})
	return out, nil
}

// ListMessages pages the selected path after entry ID after.
func (s *AgentSession) ListMessages(ctx context.Context, after string, limit int) (MessagePage, error) {
	if err := ctx.Err(); err != nil {
		return MessagePage{}, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	msgs := s.rt.manager.View().Messages
	start := 0
	if after != "" {
		start = -1
		for i, m := range msgs {
			if m.ID == after {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return MessagePage{}, product.NewError(product.CodeInvalidArgument, "message cursor is not on the selected branch")
		}
	}
	end := min(start+limit, len(msgs))
	page := MessagePage{Messages: make([]agent.AgentMessage, 0, end-start)}
	for _, m := range msgs[start:end] {
		page.Messages = append(page.Messages, agent.PublicMessage(m))
	}
	if end < len(msgs) {
		page.Next = msgs[end-1].ID
	}
	return page, nil
}

// idleForHistory rejects history changes while work could observe or extend
// the current path. Navigation never rolls back workspace files.
func (rt *runtime) idleForHistory() error {
	if err := rt.writable(); err != nil {
		return err
	}
	v := rt.manager.View()
	if rt.active != nil || v.ActiveTrace != "" {
		return product.NewError(product.CodeStateConflict, "history cannot change while a trace is active or paused")
	}
	for _, tr := range v.Traces {
		if tr.State == "queued" {
			return product.NewError(product.CodeStateConflict, "history cannot change while traces are queued")
		}
	}
	if v.HasUnresolvedEffects() {
		return product.NewError(product.CodeReconciliationRequired, "unresolved tool effects block history changes")
	}
	return nil
}

// ForkBranch creates branchID rooted at entry fromEntryID on any existing
// path (empty means the history root) and selects it.
func (s *AgentSession) ForkBranch(ctx context.Context, branchID, fromEntryID string) error {
	return s.ForkBranchWithSummary(ctx, branchID, fromEntryID, false)
}

// NavigateBranch selects an existing branch at its head.
func (s *AgentSession) NavigateBranch(ctx context.Context, branchID string) error {
	return s.NavigateBranchWithSummary(ctx, branchID, false)
}

// ForkBranchWithSummary forks like ForkBranch. With summarize, the entries of
// the old path after its common ancestor with the new path are summarized and
// the summary is appended on the new path in the same commit as the switch.
func (s *AgentSession) ForkBranchWithSummary(ctx context.Context, branchID, fromEntryID string, summarize bool) error {
	if sessstore.ValidateResourceID(branchID) != nil {
		return product.NewError(product.CodeInvalidArgument, "branch id is invalid")
	}
	return s.changeBranch(ctx, branchID, fromEntryID, true, summarize)
}

// NavigateBranchWithSummary navigates like NavigateBranch, optionally
// carrying a summary of the abandoned unique suffix (see ForkBranchWithSummary).
func (s *AgentSession) NavigateBranchWithSummary(ctx context.Context, branchID string, summarize bool) error {
	return s.changeBranch(ctx, branchID, "", false, summarize)
}

type branchChange struct {
	branch, leaf string
	suffix       []agent.AgentMessage
}

// changeBranch fixes the unique suffix in the mailbox, generates the summary
// outside it, and commits summary and switch together only if the leaf is
// unchanged. A model or validation failure leaves the old branch selected.
func (s *AgentSession) changeBranch(ctx context.Context, branchID, from string, create, summarize bool) error {
	value, err := s.rt.call(ctx, func(rt *runtime) (any, error) {
		if err := rt.idleForHistory(); err != nil {
			return nil, err
		}
		if !summarize {
			if create {
				return nil, rt.manager.ForkBranch(ctx, branchID, from)
			}
			return nil, rt.manager.NavigateBranch(ctx, branchID)
		}
		target, err := rt.manager.BranchTarget(branchID, from, create)
		if err != nil {
			return nil, err
		}
		v := rt.manager.View()
		if !create && branchID == v.BranchID {
			return nil, nil
		}
		suffix := rt.manager.UniqueSuffix(target)
		if len(suffix) == 0 {
			// Nothing is abandoned: switch without a summary or model call.
			if create {
				return nil, rt.manager.ForkBranch(ctx, branchID, from)
			}
			return nil, rt.manager.NavigateBranch(ctx, branchID)
		}
		return branchChange{branch: v.BranchID, leaf: v.LeafID, suffix: suffix}, nil
	})
	if err != nil || value == nil {
		return err
	}
	change := value.(branchChange)
	// Like idle compaction, branch maintenance has no active trace. Meter it
	// with its own bounded ledger without modifying an earlier trace's usage.
	m := &chargedModel{inner: s.rt.opts.Model, budget: agent.NewBudget(s.rt.opts.Limits), id: agent.MustID()}
	candidate, err := einorun.GenerateBranchSummary(ctx, m, change.suffix, compactToolBytes)
	if err != nil {
		return err
	}
	summary := agent.AgentMessage{ID: agent.MustID(), Kind: agent.KindBranchSummary, Status: agent.StatusComplete,
		Source: agent.SourceRef{Kind: agent.SourceModel, Description: "branch_summary"}, Scope: agent.MessageScope{SessionID: s.rt.opts.SessionID},
		Summary: &agent.SummaryMessage{Text: candidate.Text, TemplateVersion: candidate.TemplateVersion}}
	return s.rt.do(context.WithoutCancel(ctx), func(rt *runtime) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rt.idleForHistory(); err != nil {
			return err
		}
		return rt.manager.ChangeBranchWithSummary(context.WithoutCancel(ctx), branchID, from, create, change.branch, change.leaf, summary)
	})
}
