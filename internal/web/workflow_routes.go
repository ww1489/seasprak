package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	product "github.com/ww1489/seasprak/internal/errors"
)

type workflowDefinitionDTO struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}
type createWorkflowBody struct {
	Workspace string          `json:"workspace"`
	Workflow  string          `json:"workflow"`
	Version   string          `json:"version"`
	Input     json.RawMessage `json:"input"`
}

func (r *routes) mountWorkflowRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/workflows", r.workflowDefinitions)
	mux.HandleFunc("POST /v1/workflow-runs", r.createWorkflow)
	mux.HandleFunc("GET /v1/workflow-runs", r.listWorkflowRuns)
	mux.HandleFunc("GET /v1/workflow-runs/{rid}", r.workflowSnapshot)
	mux.HandleFunc("GET /v1/workflow-runs/{rid}/snapshot", r.workflowSnapshot)
	mux.HandleFunc("POST /v1/workflow-runs/{rid}/open", r.openWorkflow)
	mux.HandleFunc("POST /v1/workflow-runs/{rid}/pause", r.workflowControl)
	mux.HandleFunc("POST /v1/workflow-runs/{rid}/cancel", r.workflowControl)
	mux.HandleFunc("POST /v1/workflow-runs/{rid}/resume", r.workflowControl)
	mux.HandleFunc("POST /v1/workflow-runs/{rid}/interactions/{iid}/responses", r.workflowRespond)
	mux.HandleFunc("GET /v1/workflow-runs/{rid}/operations/{oid}", r.workflowOperation)
	mux.HandleFunc("GET /v1/workflow-runs/{rid}/events", r.workflowEvents)
}
func (r *routes) workflowDefinitions(w http.ResponseWriter, req *http.Request) {
	out := struct {
		Workflows []workflowDefinitionDTO `json:"workflows"`
	}{Workflows: []workflowDefinitionDTO{}}
	for _, id := range sortedKeys(r.workflows.options) {
		definition := r.workflows.options[id].Definition
		out.Workflows = append(out.Workflows, workflowDefinitionDTO{definition.Name, definition.Version, definition.Description, append(json.RawMessage(nil), definition.InputSchema...)})
	}
	writeJSON(w, http.StatusOK, out)
}
func (r *routes) createWorkflow(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body createWorkflowBody
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	snapshot, err := r.workflows.Create(req.Context(), workflowCreateRequest{IdempotencyKey: key, Workspace: body.Workspace, Workflow: body.Workflow, Version: body.Version, Input: body.Input})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, snapshot)
}
func (r *routes) listWorkflowRuns(w http.ResponseWriter, req *http.Request) {
	limit := 0
	if raw := req.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			WriteError(w, invalid("limit is invalid"))
			return
		}
		limit = value
	}
	list, err := r.workflows.List(req.Context(), CatalogListRequest{After: req.URL.Query().Get("after"), Limit: limit})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
func (r *routes) workflowSnapshot(w http.ResponseWriter, req *http.Request) {
	snapshot, err := r.workflows.Snapshot(req.Context(), req.PathValue("rid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	out, err := projectWorkflowSnapshot(snapshot)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
func (r *routes) openWorkflow(w http.ResponseWriter, req *http.Request) {
	var body struct{}
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	run, err := r.workflows.Writer(req.Context(), req.PathValue("rid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	snapshot, err := run.Snapshot(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	if snapshot.RunID != req.PathValue("rid") {
		WriteError(w, product.NewError(product.CodeInternal, "workflow root differs"))
		return
	}
	out, err := projectWorkflowSnapshot(snapshot)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
