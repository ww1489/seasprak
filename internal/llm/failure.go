package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
)

// FailureClassification contains only product-owned enums and a bounded duration.
// Only the unexported trustedFailure payload created at the observed transport
// boundary is authority; arbitrary Error.Retryable/Details values are ignored.
type FailureClassification struct {
	Kind       string        `json:"kind"`
	RetryAfter time.Duration `json:"retryAfter,omitempty"`
	// A server minimum above the retry cap forbids retry; never send early.
	RetryAfterExceedsLimit bool `json:"retryAfterExceedsLimit,omitempty"`
}
type trustedFailure struct{ FailureClassification }

func classified(info FailureClassification) *product.Error {
	message := "model request failed"
	if info.Kind == "context_overflow" {
		message = "model context overflow; no committed replacement projection is available"
	}
	p := product.NewError(product.CodeResourceUnavailable, message)
	p.Retryable = info.Kind == "rate_limit" || info.Kind == "service_unavailable" || info.Kind == "connection"
	p.Details = trustedFailure{info}
	return p
}

// ModelFailure recognizes private transport classification, traversing every
// joined member. An unrelated or stronger error cannot be hidden by errors.As.
func ModelFailure(err error) (FailureClassification, bool) {
	if err == nil {
		return FailureClassification{}, false
	}
	if e, ok := err.(*product.Error); ok && e != nil && e.Code == product.CodeResourceUnavailable {
		info, trusted := e.Details.(trustedFailure)
		return info.FailureClassification, trusted
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var result FailureClassification
		items := j.Unwrap()
		if len(items) == 0 {
			return result, false
		}
		for _, item := range items {
			info, ok := ModelFailure(item)
			if !ok {
				return FailureClassification{}, false
			}
			if result.Kind != "" && result.Kind != info.Kind {
				return FailureClassification{}, false
			}
			result.Kind = info.Kind
			result.RetryAfter = max(result.RetryAfter, info.RetryAfter)
			result.RetryAfterExceedsLimit = result.RetryAfterExceedsLimit || info.RetryAfterExceedsLimit
		}
		return result, true
	}
	if w, ok := err.(interface{ Unwrap() error }); ok {
		return ModelFailure(w.Unwrap())
	}
	return FailureClassification{}, false
}

func vetoCode(err error) string {
	rank := map[string]int{product.CodeStorageUnavailable: 9, product.CodePermissionDenied: 8, product.CodeBudgetExhausted: 7, product.CodeUnauthenticated: 6, product.CodeStateConflict: 5, product.CodeUnsupportedCapability: 4, product.CodeInvalidArgument: 3, product.CodeInternal: 5, product.CodeIncompatibleVersion: 5, product.CodeIncompatibleResume: 5, product.CodeReconciliationRequired: 5, product.CodeIdempotencyConflict: 5, product.CodeResyncRequired: 5, product.CodeNotFound: 5}
	best := ""
	var visit func(error)
	visit = func(e error) {
		if e == nil {
			return
		}
		if p, ok := e.(*product.Error); ok && p != nil && rank[p.Code] > rank[best] {
			best = p.Code
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, v := range j.Unwrap() {
				visit(v)
			}
		} else if w, ok := e.(interface{ Unwrap() error }); ok {
			visit(w.Unwrap())
		}
	}
	visit(err)
	return best
}

type failureObservedBody struct {
	io.ReadCloser
	ctx    context.Context
	status int
}

func (b *failureObservedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF && b.status >= 200 && b.status < 300 && recoverableTransportError(err) {
		noteHTTPFailure(b.ctx, 0, nil, err)
	}
	return n, err
}

type failureObservation struct {
	mu   sync.Mutex
	info FailureClassification
}
type failureObservationKey struct{}

func withFailureObservation(ctx context.Context) context.Context {
	return context.WithValue(ctx, failureObservationKey{}, &failureObservation{})
}
func noteHTTPFailure(ctx context.Context, status int, header http.Header, err error) {
	o, _ := ctx.Value(failureObservationKey{}).(*failureObservation)
	if o == nil {
		return
	}
	info := FailureClassification{}
	switch status {
	case 429:
		info.Kind = "rate_limit"
	case 500, 502, 503, 504:
		info.Kind = "service_unavailable"
	case 401:
		info.Kind = "authentication"
	case 403:
		info.Kind = "permission"
	case 400, 404, 405, 413, 422:
		info.Kind = "invalid_request"
	}
	if info.Kind == "rate_limit" || info.Kind == "service_unavailable" {
		info.RetryAfter, info.RetryAfterExceedsLimit = parseRetryAfter(header.Get("Retry-After"), time.Now())
	}
	if err != nil && recoverableTransportError(err) {
		info.Kind = "connection"
	}
	o.mu.Lock()
	o.info = info
	o.mu.Unlock()
}
func recoverableTransportError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Timeout()
}
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if len(value) > 128 {
		return 0, true
	}
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return time.Duration(min(seconds, 10)) * time.Second, seconds > 10
	}
	// Overflowing unsigned decimal values are still a server minimum above cap.
	if value != "" && strings.Trim(value, "0123456789") == "" {
		return 10 * time.Second, true
	}
	if date, err := http.ParseTime(value); err == nil {
		delay := date.Sub(now)
		return max(0, min(10*time.Second, delay)), delay > 10*time.Second
	}
	return 0, false
}
func observedModelError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	safe := safeModelError(err)
	if errors.Is(safe, context.Canceled) || errors.Is(safe, context.DeadlineExceeded) || vetoCode(err) != "" {
		return safe
	}
	if _, ok := ModelFailure(safe); ok {
		return safe
	}
	o, _ := ctx.Value(failureObservationKey{}).(*failureObservation)
	if o != nil {
		o.mu.Lock()
		info := o.info
		o.mu.Unlock()
		if info.Kind != "" {
			return classified(info)
		}
	}
	return safe
}
