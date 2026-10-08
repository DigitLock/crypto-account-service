// Phase 6 of docs/test-plan-s3.md, rows T613 and T614 and the metric parts of T604 and T607: the reconciliation
// worker on the pool of cas_server. The treasury, its cursor, its entries and the authorizations are rows written
// by the owner role: the worker reads tables only. The rules on Anvil are covered in internal/reconcile.
package reconciler_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/engine"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/reconcile"
	"github.com/DigitLock/crypto-account-service/internal/reconciler"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

var ctx = context.Background()

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	owner, server *pgxpool.Pool
	reg           *registry.Registry
	connectors    *connector.Set
	metrics       *prometheus.Registry
	log           *syncBuffer
	worker        *reconciler.Worker
	treasury      uuid.UUID
}

// setup migrates and empties the test database, creates the tenants cas-platform, tenant-a and tenant-b, and a
// worker on the pool of cas_server with its own metrics registry. The configs of the seeded sources are restored at
// the end of the test.
func setup(t *testing.T) *harness {
	t.Helper()
	h := &harness{owner: testdb.Open(t), log: &syncBuffer{}, metrics: prometheus.NewRegistry()}
	testdb.Clean(t)
	h.server = testdb.OpenServer(t)
	h.reg = registry.New(h.owner)
	for _, name := range []string{"cas-platform", "tenant-a", "tenant-b"} {
		if _, err := h.reg.CreateTenant(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.owner.Query(ctx, `SELECT code, config FROM sources`)
	if err != nil {
		t.Fatal(err)
	}
	saved := map[string][]byte{}
	for rows.Next() {
		var code string
		var config []byte
		if err := rows.Scan(&code, &config); err != nil {
			t.Fatal(err)
		}
		saved[code] = config
	}
	t.Cleanup(func() {
		for code, config := range saved {
			h.exec(t, `UPDATE sources SET config = $2 WHERE code = $1`, code, config)
		}
	})
	h.connectors = connector.NewSet()
	h.connectors.RegisterEVM(evm.New([]uint64{31337, 84532}, nil, nil, nil))
	h.worker = h.newWorker(h.log, h.metrics, time.Second)
	return h
}

func (h *harness) newWorker(log *syncBuffer, metrics prometheus.Registerer, tick time.Duration) *reconciler.Worker {
	return reconciler.New(reconciler.Config{
		DB: h.server, Connectors: h.connectors, Clock: limiter.SystemClock{}, Tick: tick,
		Logger: slog.New(slog.NewJSONHandler(log, nil)), Metrics: metrics,
	})
}

func (h *harness) exec(t *testing.T, stmt string, args ...any) {
	t.Helper()
	if _, err := h.owner.Exec(ctx, stmt, args...); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.owner.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// treasuryOn creates the treasury connection of cas-platform on a source and names it in treasury_connection. Its
// streams are not due: the engines of these tests run no stream.
func (h *harness) treasuryOn(t *testing.T, source string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.owner.QueryRow(ctx, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account, label)
		SELECT t.id, 'treasury', s.id, $2, 'Treasury' FROM tenants t, sources s WHERE t.name = 'cas-platform' AND s.code = $1
		RETURNING id`, source, "0x"+randomHex(t, 20)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('treasury_connection', $2::text) WHERE code = $1`,
		source, id.String())
	return id
}

// moveCursor writes the logs cursor of the treasury on anvil: next_block and last_time 10:00 + next_block s.
func (h *harness) moveCursor(t *testing.T, nextBlock int) {
	t.Helper()
	lastTime := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC).Add(time.Duration(nextBlock) * time.Second)
	h.exec(t, `INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at)
		VALUES ($1, 'logs', 'INCREMENTAL', jsonb_build_object('next_block', $2::bigint, 'last_hash', '0x01',
			'last_time', $3::text), now() + interval '1 year')
		ON CONFLICT (connection_id, stream) DO UPDATE SET cursor = EXCLUDED.cursor`,
		h.treasury, nextBlock, lastTime.Format(time.RFC3339))
}

// authorization writes an authorization of a tenant on anvil, received at 09:00 and valid until 09:00:04, with
// chainAuthID (64 hex characters), or a random one when empty; it returns the chain_auth_id as 0x hex.
func (h *harness) authorization(t *testing.T, tenant, authID, status string, amount int64, chainAuthID string) string {
	t.Helper()
	if chainAuthID == "" {
		chainAuthID = randomHex(t, 32)
	}
	h.exec(t, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, token_amount, chain_id, status, received_at,
		valid_until) SELECT id, $2, decode($3, 'hex'), $4, 31337, $5, '2026-10-07T09:00:00Z', '2026-10-07T09:00:04Z'
		FROM tenants WHERE name = $1`, tenant, authID, chainAuthID, amount, status)
	return "0x" + chainAuthID
}

// event writes a Debited (CARD_DEBIT IN) or Refunded (CARD_REFUND OUT) entry of the treasury, as the connector
// stores it; id is the authId or the refundId as 0x hex.
func (h *harness) event(t *testing.T, kind, id string, amount int64) {
	t.Helper()
	tx := "0x" + randomHex(t, 32)
	direction, key, name := "IN", "authId", "Debited"
	if kind == "CARD_REFUND" {
		direction, key, name = "OUT", "refundId", "Refunded"
	}
	h.exec(t, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw) SELECT tenant_id, id, 'logs', $2 || ':1', 'SINGLE', 'logs:' || $2 || ':1',
		$3, $4, 'USDC', '0x00000000000000000000000000000000000A6130', $7::numeric / 1000000, '2026-10-07T09:30:00Z',
		jsonb_build_object('transactionHash', $2::text, 'event', $5::text,
			'args', jsonb_build_object($6::text, $8::text, 'amount', $7::text))
		FROM connections WHERE id = $1`, h.treasury, tx, kind, direction, name, key, amount, id)
}

func (h *harness) runs(t *testing.T) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM reconciliation_runs`)
}

// S3-T613 — Req: Card Spend UC-4 trigger; Core §3.2; S3 D-8. A pass of the worker: no treasury_connection, nothing
// and no log line; the cursor of T without a run yet: one run per tenant, an INFO line each; the cursor unchanged for
// two ticks: no run; the cursor moved: one run per tenant. A treasury never indexed: no run, one WARN line; an error
// of a source: an ERROR line each pass, and the other source still runs.
func TestT613_WorkerTrigger(t *testing.T) {
	h := setup(t)
	h.worker.Pass(ctx)
	if h.runs(t) != 0 || h.log.String() != "" {
		t.Fatalf("without a treasury: %d runs, log %q", h.runs(t), h.log.String())
	}

	h.treasury = h.treasuryOn(t, "anvil")
	h.moveCursor(t, 121)
	h.event(t, "CARD_DEBIT", h.authorization(t, "tenant-a", "a1", "DEBIT_CONFIRMED", 20_000_000, ""), 20_000_000)
	h.authorization(t, "tenant-b", "b1", "APPROVED", 8_000_000, "")
	h.treasuryOn(t, "base-sepolia") // never indexed: no logs cursor

	h.worker.Pass(ctx)
	if n := h.runs(t); n != 3 {
		t.Fatalf("%d runs after the first pass, want one per tenant: 3", n)
	}
	for _, tenant := range []string{"tenant-a", "tenant-b", "cas-platform"} {
		if !strings.Contains(h.log.String(), `"msg":"reconciliation run stored","source":"anvil","tenant":"`+tenant+`"`) {
			t.Errorf("no INFO line of the run of %s: %s", tenant, h.log.String())
		}
	}
	if !strings.Contains(h.log.String(), `"mismatches":"MISSING_DEBIT 1"`) || !strings.Contains(h.log.String(), `"mismatches":"no mismatches"`) {
		t.Errorf("the INFO lines do not carry the mismatch counts: %s", h.log.String())
	}

	for range 2 {
		h.worker.Pass(ctx)
	}
	if n := h.runs(t); n != 3 {
		t.Errorf("%d runs after two passes with the cursor unchanged, want 3", n)
	}
	warn := `"level":"WARN","msg":"reconciliation waits: the treasury connection has no stored logs yet","source":"base-sepolia"`
	if n := strings.Count(h.log.String(), warn); n != 1 {
		t.Errorf("%d WARN lines of the treasury never indexed, want 1: %s", n, h.log.String())
	}
	if n := h.count(t, `SELECT count(*) FROM reconciliation_runs r JOIN sources s ON s.id = r.source_id WHERE s.code = 'base-sepolia'`); n != 0 {
		t.Errorf("%d runs of the treasury never indexed", n)
	}

	h.moveCursor(t, 131)
	h.worker.Pass(ctx)
	if n := h.count(t, `SELECT count(*) FROM reconciliation_runs WHERE to_block = 130`); n != 3 || h.runs(t) != 6 {
		t.Errorf("after the cursor moved: %d runs at to_block 130, %d in all; want 3 and 6", n, h.runs(t))
	}

	// A source that keeps failing: one ERROR line per source and error text (owner's decision on st7b); a changed
	// text gives a second line; after a success, the first text gives a line again.
	setTreasury := func(ref string) {
		t.Helper()
		h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('treasury_connection', $1::text) WHERE code = 'base-sepolia'`, ref)
	}
	errorLines := func(text string) int {
		return strings.Count(h.log.String(), `"level":"ERROR","msg":"reconciliation failed; retried at the next tick",`+
			`"source":"base-sepolia","error":"`+text+`"`)
	}
	const malformed, missing = "treasury_connection of the source is not a connection ID",
		"the treasury connection of the source does not exist"
	setTreasury("not-a-uuid")
	h.moveCursor(t, 141)
	for range 3 {
		h.worker.Pass(ctx)
	}
	if n := errorLines(malformed); n != 1 {
		t.Errorf("%d ERROR lines of base-sepolia after three failing passes, want 1: %s", n, h.log.String())
	}
	if n := h.count(t, `SELECT count(*) FROM reconciliation_runs WHERE to_block = 140`); n != 3 {
		t.Errorf("anvil beside a failing source: %d runs at to_block 140, want 3", n)
	}

	setTreasury(uuid.NewString()) // a connection that does not exist: another text
	for range 2 {
		h.worker.Pass(ctx)
	}
	if n, m := errorLines(malformed), errorLines(missing); n != 1 || m != 1 {
		t.Errorf("after the error text changed: %d and %d ERROR lines, want 1 and 1: %s", n, m, h.log.String())
	}

	// A success of base-sepolia: its treasury indexed; then the first failure again is logged again.
	indexed := h.treasuryOn(t, "base-sepolia")
	h.exec(t, `INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at) VALUES ($1, 'logs', 'INCREMENTAL',
		'{"next_block": 11, "last_hash": "0x01", "last_time": "2026-10-07T10:00:00Z"}', now() + interval '1 year')`, indexed)
	h.worker.Pass(ctx)
	if n := h.count(t, `SELECT count(*) FROM reconciliation_runs r JOIN sources s ON s.id = r.source_id
		WHERE s.code = 'base-sepolia'`); n != 1 {
		t.Fatalf("%d runs of base-sepolia after its treasury was indexed, want the platform run", n)
	}
	setTreasury("not-a-uuid")
	for range 2 {
		h.worker.Pass(ctx)
	}
	if n := errorLines(malformed); n != 2 {
		t.Errorf("%d ERROR lines of the malformed reference after a success and a new failure, want 2: %s", n, h.log.String())
	}
}

// S3-T613, with the engine lock — Req: Core §3.2; S3 D-8, S3 D-41. Two engines of server on one database, each with
// its worker as companion: only the instance that holds the lock runs its worker; the standby runs none. After the
// cursor moves, one run per tenant is stored, and no more while it stays.
func TestT613_WorkerRunsWithTheLock(t *testing.T) {
	h := setup(t)
	h.treasury = h.treasuryOn(t, "anvil")
	h.moveCursor(t, 121)
	h.authorization(t, "tenant-a", "a1", "APPROVED", 8_000_000, "")
	v, err := vault.New([]byte(randomHex(t, 16)), 1)
	if err != nil {
		t.Fatal(err)
	}
	var started [2]atomic.Int32
	newEngine := func(i int) *engine.Engine {
		w := h.newWorker(&syncBuffer{}, nil, 10*time.Millisecond)
		return engine.New(engine.Config{
			MaxPagesPerRun: 1, FailureThreshold: 5, BackoffInitial: time.Second, BackoffMax: time.Minute, Workers: 1,
			Tick: 10 * time.Millisecond, LockRetry: 10 * time.Millisecond, KeyCheckInterval: 24 * time.Hour,
		}, engine.Deps{
			DB: h.server, Vault: v, Connectors: h.connectors, Limiters: limiter.NewSet(limiter.SystemClock{}, nil),
			Locker: engine.PGLocker{Pool: h.server}, Clock: limiter.SystemClock{}, Reporter: nopReporter{},
			Logger: slog.New(slog.NewJSONHandler(&syncBuffer{}, nil)),
			Companions: []func(context.Context){func(ctx context.Context) {
				started[i].Add(1)
				w.Run(ctx)
			}},
		})
	}
	runCtx, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		done.Wait()
	})
	done.Go(func() { _ = newEngine(0).Run(runCtx) })
	eventually(t, "the first instance takes the lock and runs its worker", func() bool { return h.runs(t) == 2 })
	done.Go(func() { _ = newEngine(1).Run(runCtx) })
	time.Sleep(200 * time.Millisecond) // about 20 ticks of each instance

	if started[0].Load() != 1 || started[1].Load() != 0 {
		t.Errorf("workers started: %d with the lock, %d standby; want 1 and 0", started[0].Load(), started[1].Load())
	}
	if n := h.runs(t); n != 2 {
		t.Errorf("%d runs while the cursor stays, want the first 2", n)
	}
	h.moveCursor(t, 131)
	eventually(t, "the moved cursor gives one run per tenant", func() bool { return h.runs(t) == 4 })
	time.Sleep(100 * time.Millisecond)
	if n := h.runs(t); n != 4 {
		t.Errorf("%d runs after the move, want 4", n)
	}
}

type nopReporter struct{}

func (nopReporter) RunFinished(string, string, bool)         {}
func (nopReporter) EntriesInserted(string, int)              {}
func (nopReporter) DuplicatesSkipped(string, int)            {}
func (nopReporter) UnmappedAsset(string, string)             {}
func (nopReporter) BudgetWaited(string, time.Duration)       {}
func (nopReporter) RateLimited(string)                       {}
func (nopReporter) Connections(map[string]int)               {}
func (nopReporter) Staleness(map[string]time.Duration)       {}
func (nopReporter) LedgerGap(string, string, string, string) {}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not happen", what)
}

// gauge returns reconciliation_mismatches by "source/type".
func (h *harness) gauge(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := h.metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "reconciliation_mismatches" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			out[labels["source"]+"/"+labels["type"]] = m.GetGauge().GetValue()
		}
	}
	return out
}

func wantGauge(t *testing.T, got map[string]float64, nonZero map[string]float64) {
	t.Helper()
	if len(got) != len(reconciler.MismatchTypes) {
		t.Errorf("%d series %v, want one per type of anvil only: %d", len(got), got, len(reconciler.MismatchTypes))
	}
	for _, typ := range reconciler.MismatchTypes {
		if v, ok := got["anvil/"+typ]; !ok || v != nonZero[typ] {
			t.Errorf("reconciliation_mismatches{anvil,%s} = %v (%v), want %v", typ, v, ok, nonZero[typ])
		}
	}
}

// S3-T614, with the metric parts of S3-T604 and S3-T607 — Req: Card Spend §2.5.1, FR-22; S3 D-9. An unknown debit
// and an unknown refund of the treasury, a MISSING_DEBIT of A and one of B: the gauge holds UNKNOWN_DEBIT 1,
// UNKNOWN_REFUND 1, MISSING_DEBIT 2 (summed over the tenants) and 0 for every other type of anvil, none for a source
// without a run. A run stored beside the worker, as casctl stores it, with nothing new: the values stay, they do not
// grow. The unknown debit becomes known: the newer run clears it and UNKNOWN_DEBIT returns to 0.
func TestT614_MismatchGauge(t *testing.T) {
	h := setup(t)
	h.treasury = h.treasuryOn(t, "anvil")
	h.moveCursor(t, 121)
	unknownAuth, unknownRefund := randomHex(t, 32), randomHex(t, 32)
	h.event(t, "CARD_DEBIT", "0x"+unknownAuth, 3_000_000)
	h.event(t, "CARD_REFUND", "0x"+unknownRefund, 1_000_000)
	h.authorization(t, "tenant-a", "a1", "APPROVED", 8_000_000, "")
	h.authorization(t, "tenant-b", "b1", "APPROVED", 9_000_000, "")

	h.worker.Pass(ctx)
	want := map[string]float64{reconcile.UnknownDebit: 1, reconcile.UnknownRefund: 1, reconcile.MissingDebit: 2}
	wantGauge(t, h.gauge(t), want)

	if _, err := reconcile.Source(ctx, h.server, "anvil"); err != nil {
		t.Fatal(err)
	}
	h.worker.Pass(ctx)
	if n := h.runs(t); n != 6 {
		t.Fatalf("%d runs, want 3 of the worker and 3 stored beside it", n)
	}
	wantGauge(t, h.gauge(t), want)

	// The unknown debit is the debit of a new authorization of A: no longer unknown, and matched.
	h.authorization(t, "tenant-a", "a2", "DEBIT_CONFIRMED", 3_000_000, unknownAuth)
	h.moveCursor(t, 131)
	h.worker.Pass(ctx)
	want = map[string]float64{reconcile.UnknownRefund: 1, reconcile.MissingDebit: 2}
	wantGauge(t, h.gauge(t), want)

	// The worker's Run ends, as when the lock is lost or the process stops: no series is left. A new Run sets the
	// gauge again after its first pass.
	run := func() (stop func()) {
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.worker.Run(runCtx)
		}()
		return func() {
			cancel()
			<-done
		}
	}
	stop := run()
	eventually(t, "the first pass of the Run", func() bool { return len(h.gauge(t)) == len(reconciler.MismatchTypes) })
	stop()
	if got := h.gauge(t); len(got) != 0 {
		t.Errorf("series after Run ended: %v, want none", got)
	}
	stop = run()
	eventually(t, "the first pass of a new Run", func() bool { return len(h.gauge(t)) == len(reconciler.MismatchTypes) })
	stop()
	h.worker.Pass(ctx)
	wantGauge(t, h.gauge(t), want)
}
