// Package limiter is the rate limiter of one source (SRS — Core §2.1.1 Connector contract, FR-110).
// The engine builds one per source from the budgets its connector declares. A budget holds a number of
// cost units per time window; a reservation that does not fit waits for the next window; a pause the
// source demands blocks its budget until it ends while the other budgets go on. Time and waiting come
// from an injected clock.
package limiter

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// Clock is the time of the limiter and the engine. Wait returns when the clock reaches until, or with the
// error of ctx when ctx ends first.
type Clock interface {
	Now() time.Time
	Wait(ctx context.Context, until time.Time) error
}

// SystemClock is the Clock of the running server.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Wait sleeps until until or until ctx ends.
func (SystemClock) Wait(ctx context.Context, until time.Time) error {
	t := time.NewTimer(time.Until(until))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type budget struct {
	units       int
	window      time.Duration
	used        int
	windowStart time.Time
	pausedUntil time.Time
}

// Limiter is the limiter of one source. It is safe for concurrent use.
type Limiter struct {
	clock  Clock
	onWait func(time.Duration)

	mu      sync.Mutex
	budgets map[string]*budget
}

var _ connector.Limiter = (*Limiter)(nil)

// New returns a limiter with the budgets of a source. onWait, when not nil, is told how long each
// reservation waited.
func New(budgets []connector.Budget, clock Clock, onWait func(time.Duration)) (*Limiter, error) {
	l := &Limiter{clock: clock, onWait: onWait, budgets: make(map[string]*budget, len(budgets))}
	for _, b := range budgets {
		if b.Name == "" || b.Units < 1 || b.Window <= 0 {
			return nil, fmt.Errorf("limiter: budget %q needs a name, at least 1 unit and a positive window", b.Name)
		}
		if _, dup := l.budgets[b.Name]; dup {
			return nil, fmt.Errorf("limiter: budget %q declared twice", b.Name)
		}
		l.budgets[b.Name] = &budget{units: b.Units, window: b.Window}
	}
	return l, nil
}

// Reserve takes cost units of a budget, waiting for the next window or the end of a pause when needed.
// A cost larger than the whole budget, or an unknown budget, is an error, not an endless wait.
func (l *Limiter) Reserve(ctx context.Context, name string, cost int) error {
	var waited time.Duration
	defer func() {
		if waited > 0 && l.onWait != nil {
			l.onWait(waited)
		}
	}()
	for {
		until, err := l.tryReserve(name, cost)
		if err != nil || until.IsZero() {
			return err
		}
		start := l.clock.Now()
		if err := l.clock.Wait(ctx, until); err != nil {
			return err
		}
		waited += l.clock.Now().Sub(start)
	}
}

// tryReserve takes the units when they fit now; otherwise it returns the time to wait for.
func (l *Limiter) tryReserve(name string, cost int) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.budgets[name]
	switch {
	case !ok:
		return time.Time{}, fmt.Errorf("limiter: unknown budget %q", name)
	case cost < 1:
		return time.Time{}, fmt.Errorf("limiter: cost %d of budget %q must be at least 1", cost, name)
	case cost > b.units:
		return time.Time{}, fmt.Errorf("limiter: cost %d exceeds budget %q of %d units", cost, name, b.units)
	}
	now := l.clock.Now()
	if now.Before(b.pausedUntil) {
		return b.pausedUntil, nil
	}
	if b.windowStart.IsZero() || !now.Before(b.windowStart.Add(b.window)) {
		b.windowStart, b.used = now, 0
	}
	if b.used+cost > b.units {
		return b.windowStart.Add(b.window), nil
	}
	b.used += cost
	return time.Time{}, nil
}

// Pause blocks a budget for d from now; a longer pause already set stays. An unknown budget is ignored.
func (l *Limiter) Pause(name string, d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.budgets[name]
	if !ok {
		return
	}
	if until := l.clock.Now().Add(d); until.After(b.pausedUntil) {
		b.pausedUntil = until
	}
}
