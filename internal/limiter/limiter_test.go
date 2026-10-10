package limiter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// fakeClock moves only when the test advances it; Wait blocks until then.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[chan struct{}]time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC), waiters: map[chan struct{}]time.Time{}}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Wait(ctx context.Context, until time.Time) error {
	c.mu.Lock()
	if !c.now.Before(until) {
		c.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	c.waiters[ch] = until
	c.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiters, ch)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for ch, until := range c.waiters {
		if !c.now.Before(until) {
			close(ch)
			delete(c.waiters, ch)
		}
	}
}

// waitingUntil waits a few milliseconds at most for a waiter and returns its wake-up time.
func (c *fakeClock) waitingUntil(t *testing.T) time.Time {
	t.Helper()
	for range 2000 {
		c.mu.Lock()
		for _, until := range c.waiters {
			c.mu.Unlock()
			return until
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("nobody waits on the clock")
	return time.Time{}
}

func reserveAsync(l *Limiter, name string, cost int) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.Reserve(context.Background(), name, cost) }()
	return done
}

func assertBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the reservation returned %v while it should wait", err)
	case <-time.After(5 * time.Millisecond):
	}
}

func assertDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("the reservation did not return")
	}
}

// C1-T623 — Req: UC-102 step 2, FR-110. The unit part; internal/engine runs a sync against a small budget.
func TestT623_BudgetSpent(t *testing.T) {
	clock := newFakeClock()
	var waits []time.Duration
	var mu sync.Mutex
	l, err := New([]connector.Budget{{Name: "requests", Units: 2, Window: time.Minute}}, clock, func(d time.Duration) {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	start := clock.Now()
	for range 2 {
		if err := l.Reserve(context.Background(), "requests", 1); err != nil {
			t.Fatal(err)
		}
	}

	done := reserveAsync(l, "requests", 1)
	if got := clock.waitingUntil(t); !got.Equal(start.Add(time.Minute)) {
		t.Errorf("waits until %v, want the next window at %v", got, start.Add(time.Minute))
	}
	clock.Advance(59 * time.Second)
	assertBlocked(t, done)
	clock.Advance(time.Second)
	assertDone(t, done)
	mu.Lock()
	if len(waits) != 1 || waits[0] != time.Minute {
		t.Errorf("reported waits = %v, want one of 1m", waits)
	}
	mu.Unlock()

	if err := l.Reserve(context.Background(), "requests", 3); err == nil {
		t.Error("a cost larger than the whole budget was accepted")
	}
	if err := l.Reserve(context.Background(), "unknown", 1); err == nil {
		t.Error("an unknown budget was accepted")
	}

	t.Run("a waiting reservation honours its context", func(t *testing.T) {
		_ = l.Reserve(context.Background(), "requests", 1) // the window is full now
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- l.Reserve(ctx, "requests", 1) }()
		clock.waitingUntil(t)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Reserve = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the reservation ignored the cancelled context")
		}
	})

	for _, bad := range [][]connector.Budget{
		{{Name: "", Units: 1, Window: time.Second}},
		{{Name: "x", Units: 0, Window: time.Second}},
		{{Name: "x", Units: 1, Window: 0}},
		{{Name: "x", Units: 1, Window: time.Second}, {Name: "x", Units: 2, Window: time.Second}},
	} {
		if _, err := New(bad, clock, nil); err == nil {
			t.Errorf("New accepted %v", bad)
		}
	}
}

// C1-T624 — Req: EC-107, FR-110. The unit part; internal/engine runs the rate-limit answer of a source.
func TestT624_RateLimitPause(t *testing.T) {
	clock := newFakeClock()
	l, err := New([]connector.Budget{
		{Name: "orders", Units: 100, Window: time.Minute},
		{Name: "history", Units: 100, Window: time.Minute},
	}, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := clock.Now()
	l.Pause("orders", 60*time.Second)
	l.Pause("orders", 10*time.Second) // a shorter pause does not shorten the running one

	if err := l.Reserve(context.Background(), "history", 1); err != nil {
		t.Fatalf("the other budget is blocked: %v", err)
	}
	done := reserveAsync(l, "orders", 1)
	if got := clock.waitingUntil(t); !got.Equal(start.Add(60 * time.Second)) {
		t.Errorf("waits until %v, want the end of the pause at %v", got, start.Add(60*time.Second))
	}
	clock.Advance(30 * time.Second)
	assertBlocked(t, done)
	if err := l.Reserve(context.Background(), "history", 1); err != nil {
		t.Fatalf("the other budget is blocked during the pause: %v", err)
	}
	clock.Advance(30 * time.Second)
	assertDone(t, done)
}

// observeLimiter returns a limiter with one budget of 100 units per minute, and the time of its clock at the start.
func observeLimiter(t *testing.T) (*Limiter, *fakeClock, time.Time) {
	t.Helper()
	clock := newFakeClock()
	l, err := New([]connector.Budget{
		{Name: "api", Units: 100, Window: time.Minute},
		{Name: "other", Units: 100, Window: time.Minute},
	}, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l, clock, clock.Now()
}

// X1-T203 — Req: FR-213; X1 D-7; Core Connector contract "Rate limiter". The unit part of Observe; the Binance
// connector reports the used-weight headers in internal/connector/binance.
func TestT203_Observe(t *testing.T) {
	ctx := context.Background()

	t.Run("above the reserved units", func(t *testing.T) {
		l, clock, start := observeLimiter(t)
		if err := l.Reserve(ctx, "api", 10); err != nil {
			t.Fatal(err)
		}
		l.Observe("api", 95)
		if err := l.Reserve(ctx, "api", 5); err != nil {
			t.Fatalf("95 + 5 fits 100: %v", err)
		}
		done := reserveAsync(l, "api", 1)
		if got := clock.waitingUntil(t); !got.Equal(start.Add(time.Minute)) {
			t.Errorf("waits until %v, want the next window at %v", got, start.Add(time.Minute))
		}
		clock.Advance(time.Minute)
		assertDone(t, done)
		if err := l.Reserve(ctx, "other", 100); err != nil {
			t.Errorf("another budget took the observation: %v", err)
		}
	})

	t.Run("below the reserved units", func(t *testing.T) {
		l, clock, _ := observeLimiter(t)
		if err := l.Reserve(ctx, "api", 90); err != nil {
			t.Fatal(err)
		}
		l.Observe("api", 5) // the budget keeps its own larger count
		if err := l.Reserve(ctx, "api", 10); err != nil {
			t.Fatal(err)
		}
		done := reserveAsync(l, "api", 1)
		clock.waitingUntil(t)
		assertBlocked(t, done)
		clock.Advance(time.Minute)
		assertDone(t, done)
	})

	t.Run("before any reservation", func(t *testing.T) {
		l, clock, start := observeLimiter(t)
		clock.Advance(10 * time.Second)
		l.Observe("api", 100) // starts a window now
		done := reserveAsync(l, "api", 1)
		if got := clock.waitingUntil(t); !got.Equal(start.Add(70 * time.Second)) {
			t.Errorf("waits until %v, want the end of the window the observation started: %v", got, start.Add(70*time.Second))
		}
		clock.Advance(time.Minute)
		assertDone(t, done)
	})

	t.Run("after the window ended", func(t *testing.T) {
		l, clock, _ := observeLimiter(t)
		if err := l.Reserve(ctx, "api", 100); err != nil {
			t.Fatal(err)
		}
		clock.Advance(time.Minute)
		l.Observe("api", 40) // a new window with 40
		if err := l.Reserve(ctx, "api", 60); err != nil {
			t.Fatalf("40 + 60 fits the new window: %v", err)
		}
		done := reserveAsync(l, "api", 1)
		clock.waitingUntil(t)
		assertBlocked(t, done)
		clock.Advance(time.Minute)
		assertDone(t, done)
	})

	t.Run("with a pause running", func(t *testing.T) {
		l, clock, start := observeLimiter(t)
		l.Pause("api", 30*time.Second)
		l.Observe("api", 100) // counts; the pause stays
		done := reserveAsync(l, "api", 1)
		if got := clock.waitingUntil(t); !got.Equal(start.Add(30 * time.Second)) {
			t.Errorf("waits until %v, want the end of the pause at %v", got, start.Add(30*time.Second))
		}
		clock.Advance(30 * time.Second)
		if got := clock.waitingUntil(t); !got.Equal(start.Add(time.Minute)) {
			t.Errorf("after the pause waits until %v, want the end of the window at %v", got, start.Add(time.Minute))
		}
		clock.Advance(30 * time.Second)
		assertDone(t, done)
	})

	t.Run("unknown budget and negative used ignored", func(t *testing.T) {
		l, _, _ := observeLimiter(t)
		l.Observe("unknown", 1000)
		l.Observe("api", -1)
		if err := l.Reserve(ctx, "api", 100); err != nil {
			t.Errorf("the budget changed: %v", err)
		}
	})

	t.Run("the bounded limiter delegates", func(t *testing.T) {
		l, _, _ := observeLimiter(t)
		bounded := l.WithMaxWait(time.Millisecond)
		bounded.Observe("api", 100)
		if err := bounded.Reserve(ctx, "api", 1); !errors.Is(err, ErrNoBudget) {
			t.Errorf("Reserve after Observe of the whole budget = %v, want ErrNoBudget", err)
		}
	})
}
