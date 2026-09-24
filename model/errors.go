package model

import "fmt"

const (
	CodeInvalidArgument        = "invalid_argument"
	CodeUnauthenticated        = "unauthenticated"
	CodePermissionDenied       = "permission_denied"
	CodeNotFound               = "not_found"
	CodeStateConflict          = "state_conflict"
	CodeIdempotencyConflict    = "idempotency_conflict"
	CodeUnsupportedCapability  = "unsupported_capability"
	CodeBudgetExhausted        = "budget_exhausted"
	CodeStorageUnavailable     = "storage_unavailable"
	CodeIncompatibleVersion    = "incompatible_version"
	CodeIncompatibleResume     = "incompatible_resume"
	CodeReconciliationRequired = "reconciliation_required"
	CodeResyncRequired         = "resync_required"
	CodeResourceUnavailable    = "resource_unavailable"
	CodeInternal               = "internal_error"
)

// Error is the public product error. Details must not contain secrets.
type Error struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Retryable bool              `json:"retryable"`
	Refs      map[string]string `json:"refs,omitempty"`
	Details   any               `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func Errorf(code, format string, args ...any) *Error {
	return NewError(code, fmt.Sprintf(format, args...))
}

func AsError(err error) (*Error, bool) {
	if err == nil {
		return nil, false
	}
	pe, ok := err.(*Error)
	return pe, ok
}
