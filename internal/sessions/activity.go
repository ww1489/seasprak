package sessions

import (
	"context"
	"errors"
	"sync"
	"time"

	product "github.com/ww1489/seasprak/internal/errors"
	"github.com/ww1489/seasprak/internal/sessions/state"
)

type activityTimer interface{ Stop() bool }
type activityClock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) activityTimer
}
type systemActivityClock struct{}

func (systemActivityClock) Now() time.Time { return time.Now() }
func (systemActivityClock) AfterFunc(d time.Duration, fn func()) activityTimer {
	return time.AfterFunc(d, fn)
}

// One lease measures one trace's active execution, not individual tools. The
// journal owns amounts; this object owns only monotonic sampling and timers.
// Renewal replaces the reservation: settle elapsed through sample, then reserve
// at most one second from sample. Append latency consumes that reservation.
// The old expiry stays armed during Append. Failure or late success cancels;
// a successful late commit is retained for settlement/crash charging, but never
// revives a cancelled context. No timer consults the mailbox or Manager lock.
type activityLease struct {
	mu              sync.Mutex
	clock           activityClock
	limit           time.Duration
	record          state.ActivityBudget
	sample          time.Time
	epoch           uint64
	committed       bool
	closed          bool
	expiry, renewal activityTimer
	wake            chan uint64
	stop, done      chan struct{}
	err             error
}

func (l *activityLease) cancelLocked(frame *execution, err error) {
	if l.err == nil {
		l.err = err
	}
	frame.cancel()
}
func activityExhausted() error {
	return product.NewError(product.CodeBudgetExhausted, "activity reservation expired")
}
func (l *activityLease) armExpiryLocked(frame *execution, deadline time.Time) {
	if l.expiry != nil {
		l.expiry.Stop()
	}
	if l.renewal != nil {
		l.renewal.Stop()
	}
	l.epoch++
	epoch := l.epoch
	l.expiry = l.clock.AfterFunc(deadline.Sub(l.clock.Now()), func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if !l.closed && l.epoch == epoch {
			l.cancelLocked(frame, activityExhausted())
		}
	})
}

// remaining samples the same committed lease clock, without entering the
// mailbox or ledger. Reservation renewal never refunds elapsed active time.
func (l *activityLease) remaining() time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.committed || l.closed || l.err != nil || !l.clock.Now().Before(l.sample.Add(l.record.Reserved)) {
		return 0
	}
	return max(0, l.limit-l.record.Settled-l.record.Uncertain-max(0, l.clock.Now().Sub(l.sample)))
}

func (l *activityLease) allowed() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || !l.committed || !l.clock.Now().Before(l.sample.Add(l.record.Reserved)) {
		return activityExhausted()
	}
	return l.err
}
func (rt *runtime) beginActivity(frame *execution) error {
	err := rt.do(context.Background(), func(rt *runtime) error {
		if !rt.matchesExecution(frame.scope) {
			return product.NewError(product.CodeStateConflict, "activity execution is not active")
		}
		if err := frame.ctx.Err(); err != nil {
			return err
		}
		clock := rt.clock
		if clock == nil {
			clock = systemActivityClock{}
		}
		tr := rt.manager.View().Traces[frame.scope.TraceID]
		l := &activityLease{clock: clock, limit: tr.Limits.ActivityBudget, record: tr.Activity, wake: make(chan uint64, 1), stop: make(chan struct{}), done: make(chan struct{})}
		frame.activity = l
		sample := clock.Now()
		// Before the first append there is no permitted execution, but the watchdog
		// still bounds a blocked append. Only commit success opens the work gate.
		grant := min(time.Second, tr.Limits.ActivityBudget-tr.Activity.Settled-tr.Activity.Uncertain)
		if grant > 0 {
			l.mu.Lock()
			l.armExpiryLocked(frame, sample.Add(grant))
			l.mu.Unlock()
		}
		return rt.commitActivity(frame, sample, 0)
	})
	l := frame.activity
	if l == nil {
		return err
	}
	if err != nil {
		close(l.done)
		return err
	}
	go func() {
		defer close(l.done)
		for {
			select {
			case <-l.stop:
				return
			case <-frame.ctx.Done():
				return
			case epoch := <-l.wake:
				err := rt.do(context.Background(), func(rt *runtime) error {
					if !rt.matchesExecution(frame.scope) {
						return product.NewError(product.CodeStateConflict, "activity execution changed")
					}
					l.mu.Lock()
					if l.closed || epoch != l.epoch {
						l.mu.Unlock()
						return nil
					}
					sample := l.clock.Now()
					elapsed := sample.Sub(l.sample)
					l.mu.Unlock()
					if err := frame.ctx.Err(); err != nil {
						return err
					}
					if err := l.allowed(); err != nil {
						return err
					}
					return rt.commitActivity(frame, sample, elapsed)
				})
				if err != nil {
					l.mu.Lock()
					l.cancelLocked(frame, err)
					l.mu.Unlock()
					return
				}
			}
		}
	}()
	return nil
}

// Called only by the mailbox, always with the lease mutex released for Append.
func (rt *runtime) commitActivity(frame *execution, sample time.Time, elapsed time.Duration) error {
	l := frame.activity
	l.mu.Lock()
	revision := l.record.Revision
	l.mu.Unlock()
	record, err := rt.manager.ReserveActivity(context.Background(), frame.scope.TraceID, frame.scope.ExecutionID, revision, elapsed)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record = record
	l.sample = sample
	l.committed = true
	if l.closed {
		return nil
	} // Real exit raced a successful append; endActivity settles it.
	if frame.ctx.Err() != nil || !l.clock.Now().Before(sample.Add(record.Reserved)) {
		l.cancelLocked(frame, activityExhausted())
		return l.err
	}
	l.armExpiryLocked(frame, sample.Add(record.Reserved))
	epoch := l.epoch
	// Half-window renewal leaves the old watchdog in charge while committing.
	// The final tiny remainder is allowed to expire rather than creating a
	// nanosecond renewal loop at the exact budget boundary.
	delay := record.Reserved/2 - l.clock.Now().Sub(sample)
	if record.Reserved > time.Millisecond {
		if delay < 0 {
			delay = 0
		}
		l.renewal = l.clock.AfterFunc(delay, func() {
			select {
			case l.wake <- epoch:
			default:
			}
		})
	}
	return nil
}
func (rt *runtime) endActivity(frame *execution) error {
	l := frame.activity
	if l == nil {
		return nil
	}
	l.mu.Lock()
	stoppedAt := l.clock.Now()
	l.closed = true
	l.epoch++
	if l.expiry != nil {
		l.expiry.Stop()
	}
	if l.renewal != nil {
		l.renewal.Stop()
	}
	close(l.stop)
	l.mu.Unlock()
	<-l.done // Join a renewal already inside Append, outside the mailbox.
	l.mu.Lock()
	record, sample, committed, cause := l.record, l.sample, l.committed, l.err
	l.mu.Unlock()
	if !committed {
		return cause
	}
	err := rt.do(context.Background(), func(rt *runtime) error {
		if !rt.matchesExecution(frame.scope) {
			return product.NewError(product.CodeStateConflict, "activity settlement execution changed")
		}
		return rt.manager.SettleActivity(context.Background(), frame.scope.TraceID, frame.scope.ExecutionID, record.Revision, stoppedAt.Sub(sample))
	})
	return errors.Join(cause, err)
}
