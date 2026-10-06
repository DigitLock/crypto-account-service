package decision

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// In-memory coordination of one card-auth instance (SRS — Card Spend §3.2 Parallel work, §4 issue 6): the
// per-card lock and the waiters of a repeated auth_id. The database constraints are the second line.

// key names a card_ref or an auth_id of a tenant.
type key struct {
	tenant uuid.UUID
	id     string
}

// cardLocks is one mutex per card, taken with a deadline. A card is named by its card_ref, which is known
// before the card is read (step 5 comes before step 6). Unused entries are removed.
type cardLocks struct {
	mu    sync.Mutex
	locks map[key]*cardLock
}

type cardLock struct {
	sem   chan struct{} // one slot: held while full
	users int           // holders and waiters
}

func newCardLocks() *cardLocks { return &cardLocks{locks: map[key]*cardLock{}} }

// acquire takes the lock of k or gives up when ctx ends. On success the caller must call the returned release.
func (l *cardLocks) acquire(ctx context.Context, k key) (release func(), ok bool) {
	l.mu.Lock()
	cl := l.locks[k]
	if cl == nil {
		cl = &cardLock{sem: make(chan struct{}, 1)}
		l.locks[k] = cl
	}
	cl.users++
	l.mu.Unlock()

	done := func() {
		l.mu.Lock()
		cl.users--
		if cl.users == 0 {
			delete(l.locks, k)
		}
		l.mu.Unlock()
	}
	select {
	case cl.sem <- struct{}{}:
		return func() { <-cl.sem; done() }, true
	case <-ctx.Done():
		done()
		return nil, false
	}
}

// calls joins the requests of one auth_id: the one whose insert wins publishes its decision, the others wait
// for it. Every request joins before it reads the row and leaves when it answers, so a request that finds the
// authorization in progress always shares the call of the request deciding it in this instance.
type calls struct {
	mu    sync.Mutex
	calls map[key]*call
}

type call struct {
	done   chan struct{}
	result Decision
	users  int
}

func newCalls() *calls { return &calls{calls: map[key]*call{}} }

// join returns the call of k, created when absent.
func (c *calls) join(k key) *call {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.calls[k]
	if cl == nil {
		cl = &call{done: make(chan struct{})}
		c.calls[k] = cl
	}
	cl.users++
	return cl
}

// leave releases a joined call; the last one removes it.
func (c *calls) leave(k key, cl *call) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl.users--
	if cl.users == 0 && c.calls[k] == cl {
		delete(c.calls, k)
	}
}

// publish hands the decision to every waiter. Called by the request whose insert won, after the decision is
// stored; only the first publication counts.
func (cl *call) publish(d Decision) {
	select {
	case <-cl.done:
	default:
		cl.result = d
		close(cl.done)
	}
}

// wait returns the published decision, or false when ctx ends first.
func (cl *call) wait(ctx context.Context) (Decision, bool) {
	select {
	case <-cl.done:
		return cl.result, true
	case <-ctx.Done():
		return Decision{}, false
	}
}
