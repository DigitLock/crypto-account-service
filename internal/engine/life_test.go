// Phase 6 of docs/test-plan-c1.md (st7b part): the engine lock, the worker pool, the key check, cursors of
// streams declared later and the metrics. Run is driven by the fake clock; no test sleeps for its ticks.
package engine_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/engine"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/health"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/registry"
)

// engineWith returns an engine value with its own reporter and logger.
func (h *harness) engineWith(rep engine.Reporter, logger *slog.Logger) *engine.Engine {
	return engine.New(h.cfg, engine.Deps{
		DB: h.server, Vault: h.vault, Connectors: h.set, Limiters: h.limiters, Locker: engine.PGLocker{Pool: h.server},
		Clock: h.clock, Reporter: rep, Logger: logger,
	})
}

// startRun runs e.Run until the returned stop is called or the test ends.
func startRun(t *testing.T, e *engine.Engine) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not return after its context ended")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// eventually polls cond for up to 5 s, a millisecond apart.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 5000 {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s did not happen", what)
}

// checkCalls returns the account checks of the key of k.
func (h *harness) checkCalls(k key) []fake.Call {
	var out []fake.Call
	for _, c := range h.fake.Calls() {
		if c.Kind == "check" && c.Account == fake.AccountOf(k.apiKey) {
			out = append(out, c)
		}
	}
	return out
}

// scriptChecks makes the account check answer per API key; a key without a script is read-only.
func (h *harness) scriptChecks(byKey map[string]func() (connector.AccountInfo, error)) {
	h.fake.SetCheck(func(_ context.Context, _ connector.Source, cred connector.Credentials) (connector.AccountInfo, error) {
		apiKey := cred.ExchangeKey.APIKey.Value()
		if fn, ok := byKey[apiKey]; ok {
			return fn()
		}
		return connector.AccountInfo{Identity: fake.AccountOf(apiKey), Permissions: []string{"READ"}}, nil
	})
}

func (h *harness) checkedAt(t *testing.T, id uuid.UUID) *time.Time {
	t.Helper()
	var at *time.Time
	if err := h.owner.QueryRow(ctx, `SELECT permissions_checked_at FROM connections WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// C1-T620 — Req: FR-119, EC-116, EC-108, FR-117
func TestT620_PeriodicKeyCheck(t *testing.T) {
	h := setup(t)
	a, keyA := h.create(t)
	b, keyB := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)

	readOnly := func() (connector.AccountInfo, error) {
		return connector.AccountInfo{Identity: fake.AccountOf(keyA.apiKey), Permissions: []string{"READ"}}, nil
	}
	h.scriptChecks(map[string]func() (connector.AccountInfo, error){
		keyA.apiKey: readOnly,
		keyB.apiKey: func() (connector.AccountInfo, error) { return connector.AccountInfo{}, connector.ErrKeyRejected },
	})
	h.clock.Set(testNow.Add(24 * time.Hour))
	callsB := len(h.fakeCalls(b))
	h.pass(t, e)

	if at := h.checkedAt(t, a); at == nil || !at.Equal(testNow.Add(24*time.Hour)) || h.status(t, a) != "ACTIVE" {
		t.Errorf("A: checked at %v, status %s; want the check stored and ACTIVE", at, h.status(t, a))
	}
	if h.status(t, b) != "CREDENTIALS_INVALID" || len(h.fakeCalls(b)) != callsB {
		t.Errorf("B: status %s, %d stream calls; want CREDENTIALS_INVALID and no stream run", h.status(t, b), len(h.fakeCalls(b))-callsB)
	}
	assertInvalidAudit(t, h, b, "key_rejected", nil)

	h.scriptChecks(map[string]func() (connector.AccountInfo, error){
		keyA.apiKey: func() (connector.AccountInfo, error) {
			return connector.AccountInfo{Identity: fake.AccountOf(keyA.apiKey), Permissions: []string{"READ", "TRADE"}}, nil
		},
	})
	h.clock.Set(testNow.Add(48 * time.Hour))
	callsA := len(h.fakeCalls(a))
	h.pass(t, e)
	if h.status(t, a) != "CREDENTIALS_INVALID" || len(h.fakeCalls(a)) != callsA {
		t.Errorf("A: status %s, %d stream calls; want CREDENTIALS_INVALID and no stream run", h.status(t, a), len(h.fakeCalls(a))-callsA)
	}
	assertInvalidAudit(t, h, a, "key_not_read_only", []any{"TRADE"})
	h.clock.Advance(3 * time.Hour)
	h.pass(t, e)
	if len(h.fakeCalls(a)) != callsA {
		t.Error("the streams of A run after its key was refused")
	}
}

func assertInvalidAudit(t *testing.T, h *harness, id uuid.UUID, reason string, permissions []any) {
	t.Helper()
	var details []byte
	if err := h.owner.QueryRow(ctx, `SELECT details FROM audit_log WHERE object_id = $1 AND action = 'CREDENTIALS_INVALID'
		AND credential_id IS NULL`, id.String()).Scan(&details); err != nil {
		t.Fatalf("CREDENTIALS_INVALID audit row: %v", err)
	}
	var d map[string]any
	_ = json.Unmarshal(details, &d)
	if d["reason"] != reason || (permissions != nil && !slices.Equal(anySlice(d["permissions"]), permissions)) {
		t.Errorf("audit details = %v; want reason %s, permissions %v", d, reason, permissions)
	}
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// C1-T621 — Req: FR-119, UC-102
func TestT621_KeyCheckScope(t *testing.T) {
	h := setup(t)
	id, k := h.create(t)
	wallet, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: h.tenantID, CredentialID: h.credentialID, OwnerRef: "owner-1", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: "0x" + hex.EncodeToString(randomBytes(t, 20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := h.engine(h.server)
	h.pass(t, e)
	before := len(h.checkCalls(k))

	h.clock.Set(testNow.Add(24*time.Hour - time.Second))
	h.pass(t, e)
	if got := len(h.checkCalls(k)) - before; got != 0 {
		t.Errorf("%d checks before KEY_CHECK_INTERVAL", got)
	}
	h.clock.Set(testNow.Add(24 * time.Hour))
	h.pass(t, e)
	checks := h.checkCalls(k)[before:]
	if len(checks) != 1 || !checks[0].Reserved {
		t.Errorf("checks at KEY_CHECK_INTERVAL = %+v, want one that passed the limiter", checks)
	}
	if at := h.checkedAt(t, wallet.ID); at != nil {
		t.Errorf("the wallet was checked at %v", at)
	}
	if at := h.checkedAt(t, id); at == nil || !at.Equal(testNow.Add(24*time.Hour)) {
		t.Errorf("checked at %v, want now", at)
	}
}

// C1-T625 — Req: §3.2, UC-102. Instead of TriggerSync, which comes in st8, a stream of a running connection
// is made due through the owner pool.
func TestT625_ParallelWork(t *testing.T) {
	h := setup(t)
	var ids []uuid.UUID
	for range 6 {
		id, _ := h.create(t)
		ids = append(ids, id)
	}

	var mu sync.Mutex
	inConn := map[string]int{}
	inStream := map[string]int{}
	maxConns, maxPerConn, maxPerStream := 0, 0, 0
	release := make(chan struct{})
	h.fake.SetBefore(func(ctx context.Context, call fake.Call) error {
		mu.Lock()
		inConn[call.Connection]++
		inStream[call.Connection+"/"+call.Stream]++
		active := 0
		for _, n := range inConn {
			if n > 0 {
				active++
			}
			maxPerConn = max(maxPerConn, n)
		}
		maxConns = max(maxConns, active)
		maxPerStream = max(maxPerStream, inStream[call.Connection+"/"+call.Stream])
		mu.Unlock()
		defer func() {
			mu.Lock()
			inConn[call.Connection]--
			inStream[call.Connection+"/"+call.Stream]--
			mu.Unlock()
		}()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	inFlight := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, c := range inConn {
			if c > 0 {
				n++
			}
		}
		return n
	}

	e := h.engine(h.server)
	stop := startRun(t, e)
	eventually(t, "four connections in flight", func() bool { return inFlight() == 4 })

	// A stream of a running connection becomes due and a new pass starts while the first still runs.
	running := h.fake.Calls()[len(h.fake.Calls())-1].Connection
	h.exec(t, `UPDATE sync_cursors SET next_run_at = $2 WHERE connection_id = $1 AND stream = 'ops'`, running, h.clock.Now())
	until, _ := h.clock.earliestWaiter()
	h.clock.Set(until)
	time.Sleep(5 * time.Millisecond)
	if n := inFlight(); n != 4 {
		t.Errorf("%d connections in flight after a second pass, want 4", n)
	}

	close(release)
	eventually(t, "every connection synced", func() bool {
		if until, ok := h.clock.earliestWaiter(); ok {
			h.clock.Set(until)
		}
		return h.count(t, `SELECT count(*) FROM sync_cursors WHERE stream = 'ops' AND last_success_at IS NOT NULL`) == len(ids)
	})
	stop()

	mu.Lock()
	defer mu.Unlock()
	if maxConns != 4 || maxPerConn != 1 || maxPerStream != 1 {
		t.Errorf("at most %d connections at a time (want 4), %d calls of one connection (want 1), %d of one stream (want 1)",
			maxConns, maxPerConn, maxPerStream)
	}
}

// C1-T626 — Req: §3.2. The engine part: two engines on one database; cmd/server runs two servers.
func TestT626_OneEngine(t *testing.T) {
	h := setup(t)
	h.create(t)
	logA, logB := &syncBuffer{}, &syncBuffer{}
	recA, recB := &recorder{runs: map[string]int{}}, &recorder{runs: map[string]int{}}
	a := h.engineWith(recA, slog.New(slog.NewJSONHandler(logA, nil)))
	b := h.engineWith(recB, slog.New(slog.NewJSONHandler(logB, nil)))

	stopA := startRun(t, a)
	eventually(t, "A takes the lock and runs", func() bool { return len(recA.snapshot().runs) > 0 })
	stopB := startRun(t, b)
	eventually(t, "A waits for its tick and B for its lock retry", func() bool { return h.clock.waiterCount() == 2 })
	if strings.Contains(logB.String(), "engine lock taken") || len(recB.snapshot().runs) != 0 {
		t.Fatal("B took the lock or ran streams while A holds it")
	}

	t.Run("a lost lock is detected", func(t *testing.T) {
		// The session of the lock ends: A finds it at its next tick, stops its runs and takes the lock again.
		// As cas_server: a role may end the sessions of its own role, and the lock session is one.
		var ended bool
		if err := h.server.QueryRow(ctx, `SELECT bool_and(pg_terminate_backend(pid)) FROM pg_locks WHERE locktype = 'advisory'
			AND classid = ($1::bigint >> 32)::oid AND objid = ($1::bigint & 4294967295)::oid`, engine.EngineLockKey).Scan(&ended); err != nil || !ended {
			t.Fatalf("end the session of the lock: %v, %v", ended, err)
		}
		eventually(t, "A detects the loss", func() bool {
			h.clock.Advance(h.cfg.Tick)
			return strings.Contains(logA.String(), "engine lock lost")
		})
		eventually(t, "the lock is taken again", func() bool {
			h.clock.Advance(h.cfg.Tick)
			return strings.Count(logA.String()+logB.String(), "engine lock taken") >= 2
		})
	})

	holds := func(log *syncBuffer) bool {
		return strings.Count(log.String(), "engine lock taken") > strings.Count(log.String(), "engine lock released")
	}
	holder, other, otherLog := stopA, stopB, logB
	if holds(logB) {
		holder, other, otherLog = stopB, stopA, logA
	}
	takenBefore := strings.Count(otherLog.String(), "engine lock taken")
	holder()
	eventually(t, "the other engine takes over within SYNC_LOCK_RETRY", func() bool {
		if until, ok := h.clock.earliestWaiter(); ok {
			h.clock.Set(until)
		}
		return strings.Count(otherLog.String(), "engine lock taken") > takenBefore
	})
	other()
}

// C1-T628 — Req: FR-112, ADR-2
func TestT628_SourceIsAConnectorAndSeedData(t *testing.T) {
	root := filepath.Join("..", "..")
	literal := regexp.MustCompile(`"(fake|binance|anvil|base-sepolia)"`)
	for _, pkg := range []string{"internal/engine", "internal/ledger", "internal/limiter", "internal/grpc/api"} {
		cmd := exec.Command("go", "list", "-deps", "./"+pkg)
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s: %v", pkg, err)
		}
		for dep := range strings.Lines(string(out)) {
			if strings.Contains(dep, "/internal/connector/") {
				t.Errorf("%s depends on the connector implementation %s", pkg, strings.TrimSpace(dep))
			}
		}
		files, _ := filepath.Glob(filepath.Join(root, pkg, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if m := literal.Find(src); m != nil {
				t.Errorf("%s names the source %s", f, m)
			}
		}
	}
}

// C1-T630 — Req: FR-122, EC-119
func TestT630_StreamDeclaredLater(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	h.pass(t, h.engine(h.server))
	ops, balances := h.cursor(t, id, "ops"), h.cursor(t, id, "balances")

	streams := append(fake.DefaultStreams(), connector.Stream{
		Name: "trades:XBTUSDT", Family: "trades", Interval: time.Hour, FirstMode: connector.ModeBackfill,
		FirstCursor: json.RawMessage(`{"from": 0}`),
	})
	h.fake.SetStreams(streams)
	h.clock.Advance(time.Minute)
	h.engine(h.server).CreateMissingCursors(ctx)

	c := h.cursor(t, id, "trades:XBTUSDT")
	if c.mode != "BACKFILL" || c.cursor != `{"from": 0}` || !c.nextRunAt.Equal(h.clock.Now()) {
		t.Errorf("new cursor = %+v; want the first mode and cursor, due at once", c)
	}
	if got := h.cursor(t, id, "ops"); got.cursor != ops.cursor || !got.nextRunAt.Equal(ops.nextRunAt) {
		t.Error("the cursor of ops changed")
	}
	if got := h.cursor(t, id, "balances"); !got.nextRunAt.Equal(balances.nextRunAt) {
		t.Error("the cursor of balances changed")
	}

	t.Run("Run creates it when it takes the lock", func(t *testing.T) {
		h.exec(t, `DELETE FROM sync_cursors WHERE connection_id = $1 AND stream = 'trades:XBTUSDT'`, id)
		startRun(t, h.engine(h.server))
		eventually(t, "the cursor is created", func() bool {
			return h.count(t, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1 AND stream = 'trades:XBTUSDT'`, id) == 1
		})
	})
}

// C1-T631 — Req: EC-118
func TestT631_KeyCheckSourceUnreachable(t *testing.T) {
	h := setup(t)
	id, k := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	before := len(h.checkCalls(k))
	checks := func() int { return len(h.checkCalls(k)) - before }

	start := testNow.Add(24 * time.Hour)
	h.clock.Set(start)
	h.fake.FailNext(fake.CheckStream, connector.ErrUnreachable,
		&connector.RateLimitError{Pause: 5 * time.Minute})
	h.pass(t, e)
	if checks() != 1 || h.status(t, id) != "ACTIVE" || !h.checkedAt(t, id).Equal(testNow) {
		t.Fatalf("after the unreachable check: %d checks, status %s, checked at %v; want 1, ACTIVE, unchanged",
			checks(), h.status(t, id), h.checkedAt(t, id))
	}
	h.clock.Set(start.Add(29 * time.Second))
	h.pass(t, e)
	if checks() != 1 {
		t.Errorf("the check was repeated before its backoff of 30 s")
	}
	h.clock.Set(start.Add(30 * time.Second)) // the rate limit answer: the next attempt after its pause of 5 min
	h.pass(t, e)
	h.clock.Set(start.Add(30*time.Second + 4*time.Minute))
	h.pass(t, e)
	if checks() != 2 || h.status(t, id) != "ACTIVE" || !h.checkedAt(t, id).Equal(testNow) {
		t.Errorf("after the rate limit: %d checks, status %s; want 2, ACTIVE, unchanged", checks(), h.status(t, id))
	}

	h.pass(t, h.engine(h.server)) // a new engine value: the count lives in memory, it checks at once
	if checks() != 3 || !h.checkedAt(t, id).Equal(h.clock.Now()) {
		t.Errorf("after the restart: %d checks, checked at %v; want 3 and now", checks(), h.checkedAt(t, id))
	}
}

// C1-T634 — Req: §2.5.1. One scripted scenario, then the text of /metrics from the health handler.
func TestT634_Metrics(t *testing.T) {
	h := setup(t)
	reg := health.NewRegistry()
	prom := engine.NewPromReporter(reg)
	h.limiters = limiter.NewSet(h.clock, prom.BudgetWaited)
	h.conns = registry.NewConnections(h.server, h.vault, h.set, h.limiters, h.clock.Now, 50*time.Millisecond)
	h.fake.SetBudgets([]connector.Budget{{Name: fake.DefaultBudget, Units: 4, Window: time.Minute}})
	e := h.engineWith(prom, h.logger)

	id, _ := h.create(t)
	h.pass(t, e) // success of both streams: 6 entries; the window of 4 units is spent
	zzz := fake.NewEntry("zzz-1", "SINGLE", "DEPOSIT", "IN", "ZZZ", "1", fake.DefaultTime)
	h.fake.SetPages("ops", append(fake.DefaultHistory(), []connector.Entry{zzz}))
	h.resetOps(t, id)
	waits := h.drive(t, func() error { return e.RunPass(ctx) }) // 6 duplicates, 1 asset without alias
	h.fake.FailNext("ops", errors.New("fake: ops failed"))
	h.dueNow(t, id, "ops")
	waits += h.drive(t, func() error { return e.RunPass(ctx) }) // a failure
	h.fake.FailNext("ops", &connector.RateLimitError{Pause: time.Minute})
	h.dueNow(t, id, "ops")
	waits += h.drive(t, func() error { return e.RunPass(ctx) }) // a rate-limit answer

	stopped, _ := h.create(t)
	h.exec(t, `UPDATE connections SET status = 'CREDENTIALS_INVALID' WHERE id = $1`, stopped)
	e.RefreshGauges(ctx)

	client, callCtx := h.apiClientWithMetrics(t, reg)
	if _, err := client.ListSources(callCtx, &casv1.ListSourcesRequest{}); err != nil {
		t.Fatal(err)
	}
	_, _ = client.ListSources(ctx, &casv1.ListSourcesRequest{}) // refused by authentication, measured too

	srv := httptest.NewServer(health.NewHandler(h.logger, reg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)

	staleness := h.clock.Now().Sub(testNow).Seconds()
	want := []string{
		`sync_runs_total{result="success",source="fake",stream="balances"} 1`,
		`sync_runs_total{result="success",source="fake",stream="ops"} 2`,
		`sync_runs_total{result="failure",source="fake",stream="ops"} 2`,
		`ledger_entries_inserted_total{source="fake"} 7`,
		`ledger_duplicates_skipped_total{source="fake"} 6`,
		`unmapped_assets_total{source="fake"} 1`,
		`rate_limit_rejections_total{source="fake"} 1`,
		`rate_limit_wait_seconds_count{source="fake"} ` + itoa(waits),
		`rate_limit_wait_seconds_sum{source="fake"} ` + itoa(waits*60),
		`connections{status="ACTIVE"} 1`,
		`connections{status="DEGRADED"} 0`,
		`connections{status="CREDENTIALS_INVALID"} 1`,
		`sync_staleness_seconds{source="fake"} ` + itoa(int(staleness)),
		`sync_staleness_seconds{source="anvil"} 0`,
		`grpc_request_seconds_count{method="ConnectionService/ListSources"} 2`,
	}
	if waits != 2 {
		t.Errorf("the scenario waited %d times for the budget, want 2", waits)
	}
	for _, line := range want {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("/metrics lacks %q", line)
		}
	}
	if t.Failed() {
		t.Logf("/metrics:\n%s", grepMetrics(text))
	}
}

func grepMetrics(text string) string {
	var out []string
	for line := range strings.Lines(text) {
		if !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "go_") && !strings.HasPrefix(line, "process_") &&
			!strings.Contains(line, "_bucket{") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return strings.Join(out, "\n")
}
