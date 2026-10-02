package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/ww1489/seasprak/internal/codeagent"
)

type branchDTO struct {
	BranchID string `json:"branchId"`
	LeafID   string `json:"leafId"`
	ForkedAt string `json:"forkedAt,omitempty"`
	Active   bool   `json:"active"`
}

type forkRequest struct {
	BranchID    string `json:"branchId"`
	FromEntryID string `json:"fromEntryId"`
	// Summarize carries a summary of the abandoned unique suffix.
	Summarize bool `json:"summarize,omitempty"`
}

type activateRequest struct {
	Summarize bool `json:"summarize,omitempty"`
}

type messagePageDTO struct {
	Messages []messageDTO `json:"messages"`
	Next     string       `json:"next,omitempty"`
}

func (r *routes) mountHistory(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions/{sid}/branches", r.listBranches)
	mux.HandleFunc("POST /v1/sessions/{sid}/branches", r.forkBranch)
	mux.HandleFunc("POST /v1/sessions/{sid}/branches/{bid}/activate", r.activateBranch)
	mux.HandleFunc("GET /v1/sessions/{sid}/messages", r.listMessages)
	mux.HandleFunc("POST /v1/sessions/{sid}/compactions", r.compact)
}

type compactRequest struct {
	Reason string `json:"reason,omitempty"`
}

func (r *routes) compact(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body compactRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	if body.Reason != "" && body.Reason != "manual" {
		WriteError(w, invalid("only manual compaction can be requested"))
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	// Generation outlives the HTTP request; the result is read via operations.
	receipt, err := s.Compact(context.WithoutCancel(req.Context()), codeagent.CompactRequest{IdempotencyKey: key, Reason: "manual"})
	r.acceptedOrError(w, receipt, err)
}

// historySession returns the writer if one is open, else a read-only view
// closed by the returned func, so browsing never opens a writer.
func (r *routes) historySession(w http.ResponseWriter, req *http.Request) (*codeagent.AgentSession, func(), bool) {
	s, done, err := r.catalog.Browse(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return nil, nil, false
	}
	return s, done, true
}

func (r *routes) listBranches(w http.ResponseWriter, req *http.Request) {
	s, done, ok := r.historySession(w, req)
	if !ok {
		return
	}
	defer done()
	list, err := s.ListBranches(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	out := []branchDTO{}
	for _, b := range list {
		out = append(out, branchDTO{BranchID: b.BranchID, LeafID: b.LeafID, ForkedAt: b.ForkedAt, Active: b.Active})
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
}

func (r *routes) listMessages(w http.ResponseWriter, req *http.Request) {
	limit := 0
	if raw := req.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			WriteError(w, invalid("limit is invalid"))
			return
		}
		limit = n
	}
	s, done, ok := r.historySession(w, req)
	if !ok {
		return
	}
	defer done()
	page, err := s.ListMessages(req.Context(), req.URL.Query().Get("after"), limit)
	if err != nil {
		WriteError(w, err)
		return
	}
	out := messagePageDTO{Messages: []messageDTO{}, Next: page.Next}
	for _, m := range page.Messages {
		out.Messages = append(out.Messages, projectMessage(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *routes) forkBranch(w http.ResponseWriter, req *http.Request) {
	var body forkRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	if err := s.ForkBranchWithSummary(req.Context(), body.BranchID, body.FromEntryID, body.Summarize); err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"branchId": body.BranchID})
}

func (r *routes) activateBranch(w http.ResponseWriter, req *http.Request) {
	var body activateRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	s, ok := r.writer(w, req)
	if !ok {
		return
	}
	if err := s.NavigateBranchWithSummary(req.Context(), req.PathValue("bid"), body.Summarize); err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"branchId": req.PathValue("bid")})
}
