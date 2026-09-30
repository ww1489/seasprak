package web

import (
	"encoding/json"
	"net/http"
	"strconv"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions"
)

// routes adapts HTTP to the session catalog. Handlers only decode DTOs, check
// the authenticated principal and call the session layer; they never touch a
// store or state manager directly.
type routes struct {
	catalog   *sessions.Catalog
	principal string
}

func newRoutes(catalog *sessions.Catalog, principal string) http.Handler {
	r := &routes{catalog: catalog, principal: principal}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", r.createSession)
	mux.HandleFunc("GET /v1/sessions", r.listSessions)
	mux.HandleFunc("GET /v1/sessions/{sid}", r.getSession)
	mux.HandleFunc("GET /v1/sessions/{sid}/snapshot", r.getSnapshot)
	r.mount(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		WriteError(w, product.NewError(product.CodeNotFound, "route not implemented"))
	})
	return r.guard(mux)
}

// guard repeats the resource-level principal check for every route so that a
// handler mounted without the authentication middleware cannot serve data.
func (r *routes) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if Principal(req.Context()) == "" || Principal(req.Context()) != r.principal {
			WriteError(w, product.NewError(product.CodePermissionDenied, "principal rejected"))
			return
		}
		next.ServeHTTP(w, req)
	})
}

type createSessionRequest struct {
	Workspace string `json:"workspace"`
	Model     string `json:"model"`
}

func (r *routes) createSession(w http.ResponseWriter, req *http.Request) {
	key := req.Header.Values("Idempotency-Key")
	if len(key) != 1 || key[0] == "" {
		WriteError(w, invalid("one Idempotency-Key header is required"))
		return
	}
	var body createSessionRequest
	if err := DecodeJSON(w, req, &body); err != nil {
		WriteError(w, err)
		return
	}
	if body.Model == "" {
		body.Model = sessions.DefaultModelRef
	}
	result, err := r.catalog.Create(req.Context(), sessions.CatalogCreateRequest{IdempotencyKey: key[0], Workspace: body.Workspace, ModelRef: body.Model})
	if err != nil {
		WriteError(w, err)
		return
	}
	status := http.StatusCreated
	if result.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, projectSnapshot(result.Snapshot))
}

func (r *routes) listSessions(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			WriteError(w, invalid("limit is invalid"))
			return
		}
		limit = n
	}
	list, err := r.catalog.List(req.Context(), sessions.CatalogListRequest{After: q.Get("after"), Limit: limit})
	if err != nil {
		WriteError(w, err)
		return
	}
	out := sessionListDTO{Sessions: []sessionEntryDTO{}, Next: list.Next}
	for _, e := range list.Sessions {
		out.Sessions = append(out.Sessions, r.sessionEntry(req.Context(), e.SessionID, e.Available))
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *routes) getSession(w http.ResponseWriter, req *http.Request) {
	snap, err := r.catalog.Snapshot(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, r.sessionEntry(req.Context(), snap.SessionID, true))
}

func (r *routes) getSnapshot(w http.ResponseWriter, req *http.Request) {
	snap, err := r.catalog.Snapshot(req.Context(), req.PathValue("sid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	out := projectSnapshot(snap)
	out.InstanceID = r.catalog.InstanceID()
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
