package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	product "github.com/ww1489/seasprak/internal/errors"
)

type principalKey struct{}

const localPrincipal = "local"

// Principal is populated only after the local HTTP access boundary succeeds.
func Principal(ctx context.Context) string { p, _ := ctx.Value(principalKey{}).(string); return p }

func authorize(authority, token string, next http.Handler) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Pin the literal listener authority; neither forwarded headers nor DNS
		// aliases expand the local service's authority.
		if r.Host != authority {
			WriteError(w, product.NewError(product.CodePermissionDenied, "host rejected"))
			return
		}
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && origins[0] != "http://"+authority) {
			WriteError(w, product.NewError(product.CodePermissionDenied, "origin rejected"))
			return
		}
		// The embedded page carries no secret; it is the only content served
		// before authentication, after the same Host/Origin checks. Requests
		// that present credentials take the authenticated path unchanged.
		if len(r.Header.Values("Authorization")) == 0 && staticPath(r) {
			serveStatic(w, r)
			return
		}
		auth := r.Header.Values("Authorization")
		if len(auth) != 1 {
			WriteError(w, product.NewError(product.CodeUnauthenticated, "authentication required"))
			return
		}
		actual := sha256.Sum256([]byte(auth[0]))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
			WriteError(w, product.NewError(product.CodeUnauthenticated, "authentication required"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, localPrincipal)))
	})
}
