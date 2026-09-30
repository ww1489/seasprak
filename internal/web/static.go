package web

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	product "github.com/ww1489/seasprak/internal/errors"
)

// static holds the committed Vite build of web/. It contains no credential:
// the browser receives the bearer only from the user and keeps it in memory.
//
//go:embed static
var static embed.FS

// staticPath reports whether an unauthenticated GET/HEAD is answered from the
// embedded page: exactly "/", "/index.html" or anything under "/assets/".
// Everything else, including every /v1 route, stays behind auth.
func staticPath(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	p := r.URL.Path
	return p == "/" || p == "/index.html" || strings.HasPrefix(p, "/assets/")
}

// assetName maps a request path to an embedded file name. Under /assets/ only
// one plain segment is accepted, so encoded or literal traversal is not found.
func assetName(r *http.Request) (string, bool) {
	switch r.URL.Path {
	case "/", "/index.html":
		return "index.html", true
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if r.URL.RawPath != "" || name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
		return "", false
	}
	return "assets/" + name, true
}

// serveStatic writes one embedded file with an explicit MIME type. Paths are
// resolved only inside the embedded tree; fs.ValidPath rejects traversal.
func serveStatic(w http.ResponseWriter, r *http.Request) {
	name, ok := assetName(r)
	if !ok || !fs.ValidPath(name) {
		WriteError(w, product.NewError(product.CodeNotFound, "not found"))
		return
	}
	body, err := static.ReadFile("static/" + name)
	if err != nil {
		WriteError(w, product.NewError(product.CodeNotFound, "not found"))
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	switch path.Ext(name) {
	case ".js":
		ctype = "text/javascript; charset=utf-8"
	case ".css":
		ctype = "text/css; charset=utf-8"
	case ".html":
		ctype = "text/html; charset=utf-8"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	if name == "index.html" {
		h.Set("Cache-Control", "no-store")
		// Only same-origin scripts, styles and fetches; no inline script, no CDN.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	} else {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
