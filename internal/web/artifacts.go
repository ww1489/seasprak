package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ww1489/seasprak/internal/config"
	product "github.com/ww1489/seasprak/internal/errors"
)

// Attachment routes save and read user material. Saving never submits input
// or calls a model; an artifactId is published only after a durable save.
// The body is the raw content with its media type in Content-Type; the
// optional display name travels in the "name" query parameter. There is no
// multipart form path, so the byte limit applies to the whole request body.

type attachmentDTO struct {
	ArtifactID string `json:"artifactId"`
	MimeType   string `json:"mimeType"`
	Size       int64  `json:"size"`
	Name       string `json:"name,omitempty"`
}

func (r *routes) mountAttachments(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/sessions/{sid}/attachments", r.saveAttachment)
	mux.HandleFunc("GET /v1/sessions/{sid}/attachments/{aid}", r.readAttachment)
}

func (r *routes) saveAttachment(w http.ResponseWriter, req *http.Request) {
	key, ok := idempotencyKey(w, req)
	if !ok {
		return
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil {
		WriteError(w, invalid("Content-Type is required"))
		return
	}
	// Only a UTF-8 charset parameter on a text type is accepted.
	for k, v := range params {
		if k != "charset" || !strings.EqualFold(v, "utf-8") || strings.HasPrefix(mediaType, "image/") {
			WriteError(w, invalid("Content-Type parameters are not supported"))
			return
		}
	}
	if len(req.URL.Query()["name"]) > 1 {
		WriteError(w, invalid("one name is allowed"))
		return
	}
	if req.ContentLength > config.WebAttachmentBytes {
		writeTooLarge(w)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, config.WebAttachmentBytes)
	data, err := io.ReadAll(req.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeTooLarge(w)
			return
		}
		WriteError(w, invalid("attachment body cannot be read"))
		return
	}
	rec, dup, err := r.catalog.SaveAttachment(req.Context(), req.PathValue("sid"), SaveAttachmentRequest{IdempotencyKey: key, MimeType: mediaType, Name: req.URL.Query().Get("name"), Content: data})
	if err != nil {
		WriteError(w, err)
		return
	}
	status := http.StatusCreated
	if dup {
		status = http.StatusOK
	}
	writeJSON(w, status, attachmentDTO{ArtifactID: rec.ArtifactID, MimeType: rec.MimeType, Size: rec.Size, Name: rec.Name})
}

// readAttachment serves verified bytes with single-range support. Unknown IDs,
// IDs from another session and path-shaped IDs are all not_found.
func (r *routes) readAttachment(w http.ResponseWriter, req *http.Request) {
	rec, data, err := r.catalog.ReadAttachment(req.Context(), req.PathValue("sid"), req.PathValue("aid"))
	if err != nil {
		WriteError(w, err)
		return
	}
	size := int64(len(data))
	start, end, partial, ok := parseRange(req.Header.Get("Range"), size)
	h := w.Header()
	h.Set("Accept-Ranges", "bytes")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-store")
	if !ok {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		writeErrorStatus(w, http.StatusRequestedRangeNotSatisfiable, product.CodeInvalidArgument)
		return
	}
	h.Set("Content-Type", rec.MimeType)
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("ETag", `"`+rec.SHA256+`"`)
	h.Set("Content-Disposition", contentDisposition(rec.Name))
	h.Set("Content-Length", strconv.FormatInt(end-start, 10))
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
		h.Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10)+"/"+strconv.FormatInt(size, 10))
	}
	w.WriteHeader(status)
	if req.Method != http.MethodHead {
		_, _ = w.Write(data[start:end])
	}
}

// parseRange accepts an absent header or exactly one "bytes=" range and
// returns the half-open byte interval. Anything else is unsatisfiable.
func parseRange(header string, size int64) (start, end int64, partial, ok bool) {
	if header == "" {
		return 0, size, false, true
	}
	spec, found := strings.CutPrefix(header, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false, false
	}
	first, last, found := strings.Cut(strings.TrimSpace(spec), "-")
	if !found {
		return 0, 0, false, false
	}
	num := func(s string) (int64, bool) {
		if s == "" || strings.TrimLeft(s, "0123456789") != "" || len(s) > 18 {
			return 0, false
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	if first == "" { // suffix range: last N bytes
		n, valid := num(last)
		if !valid || n == 0 || size == 0 {
			return 0, 0, false, false
		}
		return max(size-n, 0), size, true, true
	}
	s, valid := num(first)
	if !valid || s >= size {
		return 0, 0, false, false
	}
	e := size - 1
	if last != "" {
		if e, valid = num(last); !valid || e < s {
			return 0, 0, false, false
		}
		e = min(e, size-1)
	}
	return s, e + 1, true, true
}

// contentDisposition always forces download. The display name is reduced to
// one segment without separators, quotes or controls before encoding.
func contentDisposition(name string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f, r == '/', r == '\\', r == '"', r == ':', r == ';':
			return '_'
		}
		return r
	}, name)
	clean = strings.Trim(clean, ". ")
	if clean == "" {
		clean = "attachment"
	}
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": clean}); v != "" {
		return v
	}
	return "attachment"
}

// writeTooLarge reports an oversized body as 413 with the invalid_argument code.
func writeTooLarge(w http.ResponseWriter) {
	writeErrorStatus(w, http.StatusRequestEntityTooLarge, product.CodeInvalidArgument)
}

// writeErrorStatus writes the fixed error body for statuses WriteError does
// not derive from a code (413, 416).
func writeErrorStatus(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *product.Error `json:"error"`
	}{product.NewError(code, http.StatusText(status))})
}
