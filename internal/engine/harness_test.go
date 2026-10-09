package engine_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/engine"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

var (
	ctx     = context.Background()
	testNow = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
)

// fakeClock moves only when the test moves it; Wait blocks until then.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[chan struct{}]time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testNow, waiters: map[chan struct{}]time.Time{}}
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

// Set moves the clock to t and wakes the waiters whose time has come.
func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	for ch, until := range c.waiters {
		if !c.now.Before(until) {
			close(ch)
			delete(c.waiters, ch)
		}
	}
}

func (c *fakeClock) Advance(d time.Duration) { c.Set(c.Now().Add(d)) }

// waiterCount returns how many waiters wait on the clock.
func (c *fakeClock) waiterCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// earliestWaiter returns the earliest wake-up time of a waiter.
func (c *fakeClock) earliestWaiter() (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var earliest time.Time
	for _, until := range c.waiters {
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return earliest, !earliest.IsZero()
}

// recorder is the Reporter of the tests.
type recorder struct {
	mu          sync.Mutex
	runs        map[string]int // source/family/success|failure
	inserted    int
	skipped     int
	unmapped    []string
	waited      time.Duration
	rateLimited int
	connections map[string]int
	staleness   map[string]time.Duration
	gaps        map[string]string // source/connection/asset → gap
	keyChecks   map[string]int    // source/result
}

func (r *recorder) RunFinished(source, family string, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := "failure"
	if success {
		result = "success"
	}
	r.runs[source+"/"+family+"/"+result]++
}

func (r *recorder) EntriesInserted(_ string, n int) {
	r.mu.Lock()
	r.inserted += n
	r.mu.Unlock()
}

func (r *recorder) DuplicatesSkipped(_ string, n int) {
	r.mu.Lock()
	r.skipped += n
	r.mu.Unlock()
}

func (r *recorder) UnmappedAsset(_, native string) {
	r.mu.Lock()
	r.unmapped = append(r.unmapped, native)
	r.mu.Unlock()
}

func (r *recorder) BudgetWaited(_ string, d time.Duration) {
	r.mu.Lock()
	r.waited += d
	r.mu.Unlock()
}

func (r *recorder) RateLimited(string) {
	r.mu.Lock()
	r.rateLimited++
	r.mu.Unlock()
}

func (r *recorder) Connections(byStatus map[string]int) {
	r.mu.Lock()
	r.connections = byStatus
	r.mu.Unlock()
}

func (r *recorder) LedgerGap(source, connection, asset, gap string) {
	r.mu.Lock()
	if r.gaps == nil {
		r.gaps = map[string]string{}
	}
	r.gaps[source+"/"+connection+"/"+asset] = gap
	r.mu.Unlock()
}

func (r *recorder) KeyCheckFinished(source, result string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.keyChecks == nil {
		r.keyChecks = map[string]int{}
	}
	r.keyChecks[source+"/"+result]++
}

// keyChecksOf returns the reported key checks by source/result.
func (r *recorder) keyChecksOf() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for k, v := range r.keyChecks {
		out[k] = v
	}
	return out
}

func (r *recorder) gap(source, connection, asset string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, ok := r.gaps[source+"/"+connection+"/"+asset]
	return g, ok
}

func (r *recorder) Staleness(bySource map[string]time.Duration) {
	r.mu.Lock()
	r.staleness = bySource
	r.mu.Unlock()
}

func (r *recorder) snapshot() recorder {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := map[string]int{}
	for k, v := range r.runs {
		runs[k] = v
	}
	return recorder{runs: runs, inserted: r.inserted, skipped: r.skipped, unmapped: append([]string(nil), r.unmapped...),
		waited: r.waited, rateLimited: r.rateLimited, connections: r.connections, staleness: r.staleness}
}

// syncBuffer is the log sink of the engine.
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

// harness is the test database with a tenant, the scripted fake and an engine on the pool of cas_server.
type harness struct {
	owner, server *pgxpool.Pool
	reg           *registry.Registry
	conns         *registry.Connections
	fake          *fake.Connector
	set           *connector.Set
	limiters      *limiter.Set
	vault         *vault.Envelope
	clock         *fakeClock
	rec           *recorder
	log           *syncBuffer
	logger        *slog.Logger
	cfg           engine.Config
	tenantID      uuid.UUID
	credentialID  uuid.UUID
	token         string
}

func setup(t *testing.T) *harness {
	t.Helper()
	h := &harness{owner: testdb.Open(t), clock: newFakeClock(), rec: &recorder{runs: map[string]int{}}, log: &syncBuffer{}}
	testdb.Clean(t)
	h.server = testdb.OpenServer(t)
	h.reg = registry.New(h.owner)
	if _, err := h.reg.AddFakeSource(ctx); err != nil {
		t.Fatal(err)
	}
	tn, err := h.reg.CreateTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.reg.IssueToken(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	h.tenantID, h.token = tn.ID, tok.Value
	if err := h.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).Scan(&h.credentialID); err != nil {
		t.Fatal(err)
	}
	h.fake = fake.New()
	h.set = connector.NewSet()
	h.set.Register(fake.Code, h.fake)
	h.set.RegisterEVM(evm.New([]uint64{31337, 84532}, nil, nil, nil))
	if h.vault, err = vault.New(randomBytes(t, 32), 1); err != nil {
		t.Fatal(err)
	}
	h.logger = slog.New(slog.NewJSONHandler(h.log, nil))
	h.limiters = limiter.NewSet(h.clock, h.rec.BudgetWaited)
	h.conns = registry.NewConnections(h.server, h.vault, h.set, h.limiters, h.clock.Now, 50*time.Millisecond, time.Minute)
	h.cfg = engine.Config{
		MaxPagesPerRun: 20, FailureThreshold: 5, BackoffInitial: 30 * time.Second, BackoffMax: time.Hour,
		Workers: 4, Tick: time.Second, LockRetry: 10 * time.Second, KeyCheckInterval: 24 * time.Hour,
	}
	return h
}

// engine returns a new engine value on db, as cas_server.
func (h *harness) engine(db engine.DB) *engine.Engine {
	return engine.New(h.cfg, engine.Deps{
		DB: db, Vault: h.vault, Connectors: h.set, Limiters: h.limiters, Locker: engine.PGLocker{Pool: h.server},
		Clock: h.clock, Reporter: h.rec, Logger: h.logger,
	})
}

// key is an exchange key generated at run time.
type key struct{ apiKey, apiSecret string }

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// create creates an exchange connection on fake at the time of the clock.
func (h *harness) create(t *testing.T) (uuid.UUID, key) {
	t.Helper()
	k := key{apiKey: hex.EncodeToString(randomBytes(t, 16)), apiSecret: hex.EncodeToString(randomBytes(t, 32))}
	c, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: h.tenantID, CredentialID: h.credentialID, OwnerRef: "owner-1", Source: fake.Code,
		Credentials: connector.Credentials{ExchangeKey: &connector.ExchangeKey{
			APIKey: vault.NewSecret(k.apiKey), APISecret: vault.NewSecret(k.apiSecret),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID, k
}

func (h *harness) pass(t *testing.T, e *engine.Engine) {
	t.Helper()
	if err := e.RunPass(ctx); err != nil {
		t.Fatal(err)
	}
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

// cursorRow is a sync_cursors row.
type cursorRow struct {
	mode, cursor  string
	nextRunAt     time.Time
	lastSuccessAt *time.Time
	lastError     *string
	failures      int
}

func (h *harness) cursor(t *testing.T, id uuid.UUID, stream string) cursorRow {
	t.Helper()
	var c cursorRow
	if err := h.owner.QueryRow(ctx, `SELECT mode, cursor::text, next_run_at, last_success_at, last_error, consecutive_failures
		FROM sync_cursors WHERE connection_id = $1 AND stream = $2`, id, stream).
		Scan(&c.mode, &c.cursor, &c.nextRunAt, &c.lastSuccessAt, &c.lastError, &c.failures); err != nil {
		t.Fatal(err)
	}
	return c
}

func (h *harness) status(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := h.owner.QueryRow(ctx, `SELECT status FROM connections WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// entry is a stored ledger entry.
type entry struct {
	seq                                    int64
	stream, externalID, leg, asset, native string
	amount, groupID                        string
	raw                                    []byte
}

func (h *harness) entries(t *testing.T, id uuid.UUID) []entry {
	t.Helper()
	rows, err := h.owner.Query(ctx, `SELECT seq, stream, external_id, leg, asset, native_asset, amount::text, group_id, raw
		FROM ledger_entries WHERE connection_id = $1 ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.seq, &e.stream, &e.externalID, &e.leg, &e.asset, &e.native, &e.amount, &e.groupID, &e.raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// keys returns external_id/leg of entries, sorted.
func keys(es []entry) []string {
	var ks []string
	for _, e := range es {
		ks = append(ks, e.externalID+"/"+e.leg)
	}
	sort.Strings(ks)
	return ks
}

// fakeCalls returns the page and snapshot calls of a connection as "stream:page".
func (h *harness) fakeCalls(id uuid.UUID) []string {
	var out []string
	for _, c := range h.fake.Calls() {
		if c.Connection == id.String() {
			out = append(out, c.Stream+":"+itoa(c.Page))
		}
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

// pages builds n pages of size entries with unique external IDs under prefix.
func pages(prefix string, n, size int) [][]connector.Entry {
	out := make([][]connector.Entry, n)
	for i := range n {
		for j := range size {
			id := prefix + "-" + itoa(i) + "-" + itoa(j)
			out[i] = append(out[i], fake.NewEntry(id, "SINGLE", "DEPOSIT", "IN", "BTC", "1.5", fake.DefaultTime.Add(time.Duration(i*size+j)*time.Minute)))
		}
	}
	return out
}

// drive runs fn in a goroutine and moves the clock to every wake-up time a waiter asks for, until fn ends.
// It returns how many times the clock was moved.
func (h *harness) drive(t *testing.T, fn func() error) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	moves := 0
	for range 20000 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return moves
		default:
		}
		if until, ok := h.clock.earliestWaiter(); ok {
			h.clock.Set(until)
			moves++
			continue
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the run did not end")
	return moves
}

// apiClient is the gRPC API of cmd/server on the same database and connections, as tenant-a.
func (h *harness) apiClient(t *testing.T) (casv1.ConnectionServiceClient, context.Context) {
	t.Helper()
	return h.apiClientWithMetrics(t, nil)
}

// apiClientWithMetrics is apiClient with grpc_request_seconds registered in reg.
func (h *harness) apiClientWithMetrics(t *testing.T, reg prometheus.Registerer) (casv1.ConnectionServiceClient, context.Context) {
	t.Helper()
	return casv1.NewConnectionServiceClient(h.apiConn(t, reg)), metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.token)
}

// dataClient is the AccountDataService of cmd/server on the same database and connections, as tenant-a.
func (h *harness) dataClient(t *testing.T) (casv1.AccountDataServiceClient, context.Context) {
	t.Helper()
	return casv1.NewAccountDataServiceClient(h.apiConn(t, nil)), metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.token)
}

// apiConn serves the gRPC API of cmd/server over an in-memory listener until the end of the test.
func (h *harness) apiConn(t *testing.T, reg prometheus.Registerer) *grpc.ClientConn {
	t.Helper()
	srv := api.NewServer(api.Deps{Credentials: repository.New(h.server), Connections: h.conns, Logger: h.logger, Metrics: reg})
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return conn
}
