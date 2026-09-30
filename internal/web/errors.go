package web

import (
	"encoding/json"
	"errors"
	"net/http"

	product "github.com/ww1489/seasprak/internal/errors"
)

func invalid(message string) error { return product.NewError(product.CodeInvalidArgument, message) }
func unavailable(message string) error {
	return product.NewError(product.CodeResourceUnavailable, message)
}

// WriteError exposes only a known code and fixed message, never backend details.
func WriteError(w http.ResponseWriter, err error) {
	code := product.CodeInternal
	var pe *product.Error
	if errors.As(err, &pe) && pe != nil {
		code = pe.Code
	}
	status := http.StatusInternalServerError
	switch code {
	case product.CodeInvalidArgument:
		status = 400
	case product.CodeUnauthenticated:
		status = 401
	case product.CodePermissionDenied:
		status = 403
	case product.CodeNotFound:
		status = 404
	case product.CodeStateConflict, product.CodeIdempotencyConflict, product.CodeIncompatibleVersion, product.CodeIncompatibleResume, product.CodeReconciliationRequired:
		status = 409
	case product.CodeResyncRequired:
		status = 410
	case product.CodeUnsupportedCapability, product.CodeBudgetExhausted:
		status = 422
	case product.CodeStorageUnavailable, product.CodeResourceUnavailable:
		status = 503
	default:
		code = product.CodeInternal
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="seasprak"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *product.Error `json:"error"`
	}{product.NewError(code, http.StatusText(status))})
}
