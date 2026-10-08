package engine_test

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/engine"
)

// companion records the runs of a companion function of the engine: how many started, how many run now, how many
// ended. An ending companion takes a moment, so a Run that does not wait for it returns before it is counted.
type companion struct {
	mu                     sync.Mutex
	started, active, ended int
}

func (c *companion) run(ctx context.Context) {
	c.mu.Lock()
	c.started++
	c.active++
	c.mu.Unlock()
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	c.active--
	c.ended++
	c.mu.Unlock()
}

func (c *companion) counts() (started, active, ended int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started, c.active, c.ended
}

// S3-T613, the engine part — Req: SRS — Core §3.2; S3 D-8, S3 D-41. A companion of the engine runs only while its
// instance holds the engine lock: the instance with the lock starts it once, the standby instance never; a lost lock
// ends it, the lock taken again starts it again; Run returns only after it ended.
func TestT613_CompanionFollowsTheLock(t *testing.T) {
	h := setup(t)
	logA, logB := &syncBuffer{}, &syncBuffer{}
	compA, compB := &companion{}, &companion{}
	newEngine := func(c *companion, log *syncBuffer) *engine.Engine {
		return engine.New(h.cfg, engine.Deps{
			DB: h.server, Vault: h.vault, Connectors: h.set, Limiters: h.limiters, Locker: engine.PGLocker{Pool: h.server},
			Clock: h.clock, Reporter: &recorder{runs: map[string]int{}}, Logger: slog.New(slog.NewJSONHandler(log, nil)),
			Companions: []func(context.Context){c.run},
		})
	}
	stopA := startRun(t, newEngine(compA, logA))
	eventually(t, "A takes the lock and starts its companion", func() bool { s, a, _ := compA.counts(); return s == 1 && a == 1 })
	stopB := startRun(t, newEngine(compB, logB))
	eventually(t, "A waits for its tick and B for its lock retry", func() bool { return h.clock.waiterCount() == 2 })
	if s, _, _ := compB.counts(); s != 0 || strings.Contains(logB.String(), "engine lock taken") {
		t.Fatal("the standby instance started its companion")
	}

	// The session of the lock ends: A finds it at its next tick and its companion ends; the instance that takes the
	// lock next starts its own.
	var ended bool
	if err := h.server.QueryRow(ctx, `SELECT bool_and(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory'
		AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid`, engine.EngineLockKey).Scan(&ended); err != nil || !ended {
		t.Fatalf("end the session of the lock: %v, %v", ended, err)
	}
	eventually(t, "A's companion ends with the lock", func() bool {
		h.clock.Advance(h.cfg.Tick)
		_, a, e := compA.counts()
		return strings.Contains(logA.String(), "engine lock lost") && a == 0 && e == 1
	})
	eventually(t, "a companion runs again with the lock taken again", func() bool {
		h.clock.Advance(h.cfg.Tick)
		_, a, _ := compA.counts()
		_, b, _ := compB.counts()
		return a+b == 1
	})

	stopA()
	stopB()
	for name, c := range map[string]*companion{"A": compA, "B": compB} {
		if s, a, e := c.counts(); a != 0 || s != e {
			t.Errorf("%s after Run returned: %d started, %d active, %d ended; want every companion ended", name, s, a, e)
		}
	}
	if sA, _, _ := compA.counts(); sA > 2 {
		t.Errorf("A started its companion %d times, want at most once per lock", sA)
	}
}
