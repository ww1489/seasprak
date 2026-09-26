package eino

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/ww1489/seasprak/internal/llm"
)

// retryTiming is local to an execution, never a shared mutable test clock.
// Eino remains the owner of waiting and invocation; this only calculates delay.
type retryTiming struct {
	now       func() time.Time
	random    func() float64
	remaining func() time.Duration
}

func (p retryTiming) delay(ctx context.Context, attempt int, err error) (time.Duration, bool) {
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	random := rand.Float64
	if p.random != nil {
		random = p.random
	}
	base := 100 * time.Millisecond
	for n := 1; n < attempt && base < 10*time.Second; n++ {
		base = min(10*time.Second, base*2)
	}
	delay := min(10*time.Second, base+time.Duration(float64(base)*0.5*max(0, min(1, random()))))
	if info, ok := llm.ModelFailure(err); ok {
		if info.RetryAfterExceedsLimit {
			return 0, false
		}
		delay = max(delay, info.RetryAfter)
	}
	remaining := time.Duration(1<<63 - 1)
	if p.remaining != nil {
		remaining = p.remaining()
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining = min(remaining, deadline.Sub(now()))
	}
	// Never shorten a server-requested minimum and send early. If waiting would
	// consume all remaining execution time, end the attempt chain instead.
	if delay >= remaining || ctx.Err() != nil {
		return 0, false
	}
	return delay, true
}
