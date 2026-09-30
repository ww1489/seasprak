package state

import (
	"context"
	"encoding/json"

	"github.com/ww1489/seasprak/internal/agent"
	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/store"
)

// HistoryNode is the tree index of one committed history entry.
type HistoryNode struct {
	ParentID  string `json:"parentId,omitempty"`
	CommitSeq uint64 `json:"commitSeq"`
}

// BranchHead is the newest entry of one named branch.
type BranchHead struct {
	LeafID    string `json:"leafId"`
	ForkedAt  string `json:"forkedAt,omitempty"`
	CommitSeq uint64 `json:"commitSeq"`
}

type activeCursor struct {
	BranchID string `json:"branchId"`
	LeafID   string `json:"leafId"`
	// Create marks a new branch rooted at LeafID; otherwise BranchID must exist.
	Create bool `json:"create,omitempty"`
}

// indexEntry records the tree position of an appended entry and advances the
// active branch head. Legacy linear journals rebuild the same index on load.
func indexEntry(v *View, id, parent string, seq uint64) {
	if v.Nodes == nil {
		v.Nodes = map[string]HistoryNode{}
	}
	if v.Branches == nil {
		v.Branches = map[string]BranchHead{}
	}
	v.Nodes[id] = HistoryNode{ParentID: parent, CommitSeq: seq}
	head := v.Branches[v.BranchID]
	head.LeafID, head.CommitSeq = id, seq
	v.Branches[v.BranchID] = head
}

// applyActiveCursor switches the selected branch and rebuilds Messages as the
// root-to-leaf path. Messages leaving the path move to Offpath, so nothing is
// lost and a later navigation can select them again.
func applyActiveCursor(v *View, r store.Record, seq uint64) error {
	var c activeCursor
	if err := json.Unmarshal(r.Payload, &c); err != nil || c.BranchID == "" || store.ValidateResourceID(c.BranchID) != nil {
		return product.NewError(product.CodeIncompatibleVersion, "invalid active cursor")
	}
	if c.LeafID != "" {
		if _, ok := v.Nodes[c.LeafID]; !ok {
			return product.NewError(product.CodeStateConflict, "branch target entry does not exist")
		}
	}
	if v.Branches == nil {
		v.Branches = map[string]BranchHead{}
	}
	_, exists := v.Branches[c.BranchID]
	if c.Create == exists {
		return product.NewError(product.CodeStateConflict, "branch existence does not match the cursor record")
	}
	if c.Create {
		v.Branches[c.BranchID] = BranchHead{LeafID: c.LeafID, ForkedAt: c.LeafID, CommitSeq: seq}
	} else if v.Branches[c.BranchID].LeafID != c.LeafID {
		return product.NewError(product.CodeStateConflict, "cursor leaf is not the branch head")
	}
	pool := make(map[string]agent.AgentMessage, len(v.Messages)+len(v.Offpath))
	for _, m := range v.Messages {
		pool[m.ID] = m
	}
	for id, m := range v.Offpath {
		pool[id] = m
	}
	var path []string
	for id := c.LeafID; id != ""; id = v.Nodes[id].ParentID {
		path = append(path, id)
		if len(path) > len(v.Nodes) {
			return product.NewError(product.CodeIncompatibleVersion, "history contains a cycle")
		}
	}
	messages := make([]agent.AgentMessage, 0, len(path))
	for i := len(path) - 1; i >= 0; i-- {
		m, ok := pool[path[i]]
		if !ok {
			return product.NewError(product.CodeIncompatibleVersion, "history entry is missing")
		}
		messages = append(messages, m)
		delete(pool, path[i])
	}
	v.Messages = messages
	v.Offpath = nil
	if len(pool) > 0 {
		v.Offpath = pool
	}
	v.BranchID, v.LeafID = c.BranchID, c.LeafID
	return nil
}

// Branches returns every named branch head.
func (m *Manager) BranchHeads() (map[string]BranchHead, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.view.Branches), m.view.BranchID
}

// ForkBranch creates branch id rooted at entry from (empty means the history
// root) and selects it. The caller verifies the session is idle.
func (m *Manager) ForkBranch(ctx context.Context, id, from string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setCursor(ctx, activeCursor{BranchID: id, LeafID: from, Create: true})
}

// NavigateBranch selects an existing branch at its current head.
func (m *Manager) NavigateBranch(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	head, ok := m.view.Branches[id]
	if !ok {
		return product.NewError(product.CodeNotFound, "branch not found")
	}
	return m.setCursor(ctx, activeCursor{BranchID: id, LeafID: head.LeafID})
}

// BranchTarget resolves the leaf a fork (create) or navigation would select.
func (m *Manager) BranchTarget(id, from string, create bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if create {
		if _, exists := m.view.Branches[id]; exists {
			return "", product.NewError(product.CodeStateConflict, "branch already exists")
		}
		if from != "" {
			if _, ok := m.view.Nodes[from]; !ok {
				return "", product.NewError(product.CodeStateConflict, "branch target entry does not exist")
			}
		}
		return from, nil
	}
	head, ok := m.view.Branches[id]
	if !ok {
		return "", product.NewError(product.CodeNotFound, "branch not found")
	}
	return head.LeafID, nil
}

// UniqueSuffix returns the entries of the selected path after its last common
// ancestor with the root-to-target path, oldest first. These are the only
// entries a branch summary may cover.
func (m *Manager) UniqueSuffix(target string) []agent.AgentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	onTarget := map[string]bool{}
	for id := target; id != "" && !onTarget[id]; id = m.view.Nodes[id].ParentID {
		onTarget[id] = true
	}
	var out []agent.AgentMessage
	for _, msg := range m.view.Messages {
		if !onTarget[msg.ID] {
			out = append(out, msg)
		}
	}
	return clone(out)
}

// ChangeBranchWithSummary selects the fork or navigation target and appends
// summary on the new path in the same commit. It rejects the change if the
// selected branch or leaf moved after the suffix was fixed.
func (m *Manager) ChangeBranchWithSummary(ctx context.Context, id, from string, create bool, expectedBranch, expectedLeaf string, summary agent.AgentMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.view.BranchID != expectedBranch || m.view.LeafID != expectedLeaf {
		return product.NewError(product.CodeStateConflict, "history changed while the branch summary was generated")
	}
	if summary.Kind != agent.KindBranchSummary || summary.Summary == nil {
		return product.NewError(product.CodeInvalidArgument, "branch summary entry is invalid")
	}
	if err := summary.Validate(); err != nil {
		return err
	}
	c := activeCursor{BranchID: id, LeafID: from, Create: create}
	if !create {
		head, ok := m.view.Branches[id]
		if !ok {
			return product.NewError(product.CodeNotFound, "branch not found")
		}
		if id == m.view.BranchID {
			return product.NewError(product.CodeStateConflict, "branch is already selected")
		}
		c.LeafID = head.LeafID
	}
	entry := record("message", summary.ID, summary)
	entry.ParentID = c.LeafID
	_, err := m.commit(ctx, []store.Record{record("active_cursor", c.BranchID, c)}, []store.Record{entry}, []agent.Event{
		m.event("branch.changed", "", "", map[string]string{"branchId": c.BranchID, "leafId": c.LeafID}),
		m.event("message.finalized", "", "", summary),
	})
	return err
}

func (m *Manager) setCursor(ctx context.Context, c activeCursor) error {
	if c.BranchID == m.view.BranchID && !c.Create {
		return nil
	}
	_, err := m.commit(ctx, []store.Record{record("active_cursor", c.BranchID, c)}, nil, []agent.Event{m.event("branch.changed", "", "", map[string]string{"branchId": c.BranchID, "leafId": c.LeafID})})
	return err
}
