package web

import (
	"context"
	"net/http"
)

// Maintenance routes manage display metadata without running a model.
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
	mux.HandleFunc("GET /v1/sessions/{sid}/metadata", r.getMetadata)
	mux.HandleFunc("PATCH /v1/sessions/{sid}/metadata", r.setMetadata)
	mux.HandleFunc("PUT /v1/sessions/{sid}/metadata", r.setMetadata)
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
	m, _, err := r.catalog.SetMetadata(req.Context(), req.PathValue("sid"), SetMetadataRequest{IdempotencyKey: key, Name: body.Name, Labels: body.Labels})
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projectMetadata(m))
}

// sessionEntry leaves unreadable display metadata out of an available entry.
func (r *routes) sessionEntry(ctx context.Context, sid string, available bool) sessionEntryDTO {
	out := sessionEntryDTO{SessionID: sid, Available: available}
	if available {
		if m, err := r.catalog.GetMetadata(ctx, sid); err == nil {
			out.Name, out.Labels = m.Name, projectMetadata(m).Labels
		}
	}
	return out
}
func projectMetadata(m Metadata) metadataDTO {
	labels := append([]string{}, m.Labels...)
	return metadataDTO{Name: m.Name, Labels: labels, Revision: m.Revision}
}
