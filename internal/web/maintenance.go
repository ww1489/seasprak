package web

import (
	"context"
	"net/http"

	"github.com/ww1489/seasprak/internal/agent"
	"github.com/ww1489/seasprak/internal/sessions"
)

// Maintenance and query routes that do not run the model: registered
// workflows and display metadata.

type workflowDTO struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// InputSchema is the declared input contract; node definitions stay internal.
	InputSchema any `json:"inputSchema,omitempty"`
}

type metadataRequest struct {
	Name   string   `json:"name"`
	Labels []string `json:"labels"`
}

type metadataDTO struct {
	Name     string   `json:"name"`
	Labels   []string `json:"labels"`
	Revision uint64   `json:"revision"`
}

func (r *routes) mountMaintenance(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/sessions/{sid}/workflows", r.listWorkflows)
	mux.HandleFunc("GET /v1/sessions/{sid}/metadata", r.getMetadata)
	// PATCH and PUT both replace name and labels as a whole.
	mux.HandleFunc("PATCH /v1/sessions/{sid}/metadata", r.setMetadata)
	mux.HandleFunc("PUT /v1/sessions/{sid}/metadata", r.setMetadata)
}

// listWorkflows browses the immutable startup registry without a writer.
func (r *routes) listWorkflows(w http.ResponseWriter, req *http.Request) {
	s, release, err := r.catalog.Browse(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	defer release()
	caps, err := s.Capabilities(req.Context())
	if err != nil {
		WriteError(w, err)
		return
	}
	out := []workflowDTO{}
	for _, a := range caps.Agents {
		if a.Kind != agent.AgentKindWorkflow {
			continue
		}
		dto := workflowDTO{Name: a.Name, Version: a.Version, Description: a.Description}
		if len(a.InputSchema) > 0 {
			dto.InputSchema = a.InputSchema
		}
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": out})
}

func (r *routes) getMetadata(w http.ResponseWriter, req *http.Request) {
	m, err := r.catalog.GetMetadata(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectMetadata(m))
}

func (r *routes) setMetadata(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	var body metadataRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	m, _, err := r.catalog.SetMetadata(req.Context(), req.PathValue("sid"), sessions.SetMetadataRequest{IdempotencyKey: key, Name: body.Name, Labels: body.Labels})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectMetadata(m))
}

// sessionEntry adds display metadata to an available entry. Metadata is
// display-only, so an unreadable file leaves the entry without it.
func (r *routes) sessionEntry(ctx context.Context, sid string, available bool) sessionEntryDTO {
	out := sessionEntryDTO{SessionID: sid, Available: available}
	if available {
		if m, err := r.catalog.GetMetadata(ctx, sid); err == nil {
			out.Name, out.Labels = m.Name, projectMetadata(m).Labels
		}
	}
	return out
}

func projectMetadata(m sessions.Metadata) metadataDTO {
	labels := append([]string{}, m.Labels...)
	return metadataDTO{Name: m.Name, Labels: labels, Revision: m.Revision}
}
