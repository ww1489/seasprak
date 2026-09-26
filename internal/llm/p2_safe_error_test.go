package llm

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
)

func TestP2SafeErrorTrustedClassificationSurvivesRedaction(t *testing.T) {
	ctx := withFailureObservation(t.Context())
	noteHTTPFailure(ctx, 503, http.Header{"Retry-After": {"2"}}, nil)
	original := observedModelError(ctx, errors.New("synthetic-private-error"))
	for i := 0; i < 3; i++ {
		original = safeModelError(original)
		info, ok := ModelFailure(original)
		if !ok || info.Kind != "service_unavailable" || info.RetryAfter != 2*time.Second {
			t.Fatal("trusted classification stripped")
		}
	}
	for _, code := range []string{product.CodeStorageUnavailable, product.CodePermissionDenied, product.CodeBudgetExhausted, product.CodeInternal, product.CodeIncompatibleVersion, product.CodeReconciliationRequired} {
		joined := safeModelError(errors.Join(original, product.NewError(code, "private")))
		var pe *product.Error
		if !errors.As(joined, &pe) || pe.Code != code || pe.Retryable {
			t.Fatal("trusted transient bypassed veto")
		}
	}
	spoof := product.NewError(product.CodeResourceUnavailable, "private")
	spoof.Retryable = true
	var pe *product.Error
	if !errors.As(safeModelError(spoof), &pe) || pe.Retryable {
		t.Fatal("provider retry flag trusted")
	}
}
func TestP2SafeRetryAfterBounded(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		raw     string
		want    time.Duration
		exceeds bool
	}{
		{"2", 2 * time.Second, false}, {"999999999", 10 * time.Second, true}, {"-1", 0, false}, {"1.5", 0, false}, {"private", 0, false},
		{now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second, false}, {now.Add(-time.Hour).Format(http.TimeFormat), 0, false},
		{now.Add(11 * time.Second).Format(http.TimeFormat), 10 * time.Second, true}, {"999999999999999999999999", 10 * time.Second, true},
	} {
		if got, exceeds := parseRetryAfter(tc.raw, now); got != tc.want || exceeds != tc.exceeds {
			t.Errorf("duration=%v want=%v exceeds=%v", got, tc.want, exceeds)
		}
	}
}

func TestP2SafeErrorJoinedVetoDominates(t *testing.T) {
	for _, code := range []string{product.CodeStorageUnavailable, product.CodePermissionDenied, product.CodeBudgetExhausted, product.CodeInternal, product.CodeIncompatibleVersion, product.CodeReconciliationRequired} {
		err := errors.Join(product.NewError(product.CodeInvalidArgument, "untrusted"), product.NewError(code, "untrusted"))
		var pe *product.Error
		if !errors.As(safeModelError(err), &pe) || pe.Code != code {
			t.Errorf("strong veto %s lost", code)
		}
	}
	if !errors.Is(safeModelError(errors.Join(errors.New("raw"), context.Canceled)), context.Canceled) {
		t.Fatal("cancel lost")
	}
}
