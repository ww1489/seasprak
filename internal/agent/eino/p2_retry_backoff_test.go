package eino

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	product "github.com/ww1489/seasprak/internal/errors"
)

type p2DeadlineContext struct {
	context.Context
	at time.Time
}

func (c p2DeadlineContext) Deadline() (time.Time, bool) { return c.at, true }

func TestP2AttemptRetryTimingDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{{1, 0, 100 * time.Millisecond}, {1, 1, 150 * time.Millisecond}, {2, 0.5, 250 * time.Millisecond}, {100, 1, 10 * time.Second}} {
		timing := retryTiming{now: func() time.Time { return now }, random: func() float64 { return tc.jitter }, remaining: func() time.Duration { return time.Minute }}
		delay, ok := timing.delay(t.Context(), tc.attempt, nil)
		if !ok || delay != tc.want {
			t.Errorf("delay=%v want=%v", delay, tc.want)
		}
	}
	timing := retryTiming{now: func() time.Time { return now }, random: func() float64 { return 0 }, remaining: func() time.Duration { return 99 * time.Millisecond }}
	if _, ok := timing.delay(t.Context(), 1, nil); ok {
		t.Fatal("remaining activity ignored")
	}
	timing.remaining = func() time.Duration { return time.Minute }
	ctx := p2DeadlineContext{Context: t.Context(), at: now.Add(100 * time.Millisecond)}
	if _, ok := timing.delay(ctx, 1, nil); ok {
		t.Fatal("deadline exhaustion ignored")
	}
	ctx.at = now.Add(101 * time.Millisecond)
	if _, ok := timing.delay(ctx, 1, nil); !ok {
		t.Fatal("available deadline incorrectly rejected")
	}
}

func TestP2AttemptRetryHasExplicitBoundedBackoff(t *testing.T) {
	err := product.NewError(product.CodeResourceUnavailable, "trusted injected transient")
	err.Retryable = true
	for _, attempt := range []int{1, 2, 100} {
		rc := &adk.TypedRetryContext[*schema.AgenticMessage]{RetryAttempt: attempt, Err: err}
		d := retryDecision(t.Context(), rc, nil)
		if !d.Retry || d.Backoff < 100*time.Millisecond || d.Backoff > 10*time.Second {
			t.Errorf("attempt=%d explicit backoff=%v", attempt, d.Backoff)
		}
	}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(50*time.Millisecond))
	defer cancel()
	if retryDecision(ctx, &adk.TypedRetryContext[*schema.AgenticMessage]{RetryAttempt: 1, Err: err}, nil).Retry {
		t.Error("retry cannot fit remaining deadline")
	}
}
