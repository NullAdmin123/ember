// Package faketime provides manually advanced deadlines for deterministic tests.
package faketime

import (
	"context"
	"errors"
	"sync"
	"time"
)

type timer struct {
	at      time.Time
	fn      func()
	stopped bool
}

// Clock advances callbacks synchronously without sleeping or creating sockets.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}

// New starts a test-owned clock at an explicit instant.
func New(now time.Time) *Clock { return &Clock{now: now} }

// Now returns the current simulated instant.
func (clock *Clock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

type deadlineContext struct {
	context.Context
	deadline time.Time
}

// Deadline exposes the simulated bound rather than a real wall-clock timer.
func (ctx deadlineContext) Deadline() (time.Time, bool) { return ctx.deadline, true }

// Err distinguishes deadline expiry from explicit or inherited cancellation.
func (ctx deadlineContext) Err() error {
	if errors.Is(context.Cause(ctx.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ctx.Context.Err()
}

// WithTimeout mirrors parent cancellation and earlier deadlines, but expiry
// occurs only when Advance reaches the bound. Cancel stops the pending callback.
func (clock *Clock) WithTimeout(parent context.Context, duration time.Duration) (context.Context, context.CancelFunc) {
	now := clock.Now()
	deadline := now.Add(duration)
	if earlier, ok := parent.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	base, cancel := context.WithCancelCause(parent)
	ctx := deadlineContext{Context: base, deadline: deadline}
	clock.mu.Lock()
	pending := &timer{at: deadline, fn: func() { cancel(context.DeadlineExceeded) }}
	if deadline.After(now) && base.Err() == nil {
		clock.timers = append(clock.timers, pending)
	} else if base.Err() == nil {
		cancel(context.DeadlineExceeded)
	}
	clock.mu.Unlock()
	return ctx, func() {
		clock.mu.Lock()
		pending.stopped = true
		clock.mu.Unlock()
		cancel(context.Canceled)
	}
}

// Advance runs due callbacks in deadline order before returning at the target.
func (clock *Clock) Advance(duration time.Duration) {
	target := clock.Now().Add(duration)
	for {
		clock.mu.Lock()
		chosen := -1
		for i, pending := range clock.timers {
			if !pending.stopped && !pending.at.After(target) && (chosen < 0 || pending.at.Before(clock.timers[chosen].at)) {
				chosen = i
			}
		}
		if chosen < 0 {
			clock.now = target
			clock.mu.Unlock()
			return
		}
		pending := clock.timers[chosen]
		clock.timers = append(clock.timers[:chosen], clock.timers[chosen+1:]...)
		clock.now = pending.at
		clock.mu.Unlock()
		pending.fn()
	}
}
