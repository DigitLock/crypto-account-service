// Phase 4 of docs/test-plan-x1.md through the engine and the read API: the balance stream of a Binance connection
// against the fake Binance server on the fixtures of testdata/fixtures/binance/; X1-T107 engine part and X1-T209
// engine half. The names carry the X1 row.
package engine_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DigitLock/crypto-account-service/internal/engine"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
)

// snapshotRun returns the calls of a snapshot fixture without its time call: in the engine the time was read by
// the key check of the creation.
func snapshotRun(t *testing.T, name string) []httpfixture.Call {
	t.Helper()
	return calls(t, name, 1)
}

// withSpot returns run with the account answer replaced by one with these balances: asset, free, locked.
func withSpot(t *testing.T, run []httpfixture.Call, balances ...[3]string) []httpfixture.Call {
	t.Helper()
	var list []map[string]string
	for _, b := range balances {
		list = append(list, map[string]string{"asset": b[0], "free": b[1], "locked": b[2]})
	}
	body, err := json.Marshal(map[string]any{"uid": 100000001, "accountType": "SPOT", "balances": list})
	if err != nil {
		t.Fatal(err)
	}
	out := slices.Clone(run)
	out[0].Body = body
	return out
}

// failingAt returns run cut after its call i, whose answer is status with the headers and body given.
func failingAt(run []httpfixture.Call, i, status int, headers map[string]string, body string) []httpfixture.Call {
	out := slices.Clone(run[:i+1])
	out[i].HTTPStatus, out[i].Headers, out[i].Body = status, headers, nil
	if body != "" {
		out[i].Body = []byte(body)
	}
	return out
}

// binanceScenario serves the creation of a read-only key followed by the runs given, points the source at it and
// creates the connection at the time of the clock.
func (h *harness) binanceScenario(t *testing.T, runs ...[]httpfixture.Call) (*httpfixture.Server, uuid.UUID, key) {
	t.Helper()
	all := calls(t, "key_read_only.json", 0)
	for _, r := range runs {
		all = append(all, r...)
	}
	srv := httpfixture.Serve(t, httpfixture.File{Description: "creation and balance runs", Calls: all}, nil)
	h.withBinance(t, srv.URL(), "")
	id, k, err := h.createBinance(t)
	if err != nil {
		t.Fatal(err)
	}
	return srv, id, k
}

// rowsOf renders the balances of GetBalances as "TYPE native asset free/locked", sorted.
func rowsOf(balances []*casv1.Balance) []string {
	var out []string
	for _, b := range balances {
		out = append(out, fmt.Sprintf("%s %s %s %s/%s", strings.TrimPrefix(b.GetAccountType().String(), "ACCOUNT_TYPE_"),
			b.GetNativeAsset(), b.GetAsset(), b.GetFree(), b.GetLocked()))
	}
	slices.Sort(out)
	return out
}

// fullRows are the balances of snapshot_full.json with the aliases of migration 000010.
var fullRows = []string{
	"EARN_FLEXIBLE USDT USDT 12.5/0",
	"EARN_LOCKED AXS AXS 0/129.99202928",
	"EARN_LOCKED DOT DOT 0/5",
	"FUNDING USDT USDT 1.5/1",
	"SPOT BTC BTC 0.001/0",
	"SPOT LDO LDO 3.5/0",
	"SPOT TSTX TSTX 1/0",
	"SPOT USDT USDT 20/1.5",
}

// serverTimeOf is the server time of the time call of the creation, in key_read_only.json.
func serverTimeOf(t *testing.T) time.Time {
	t.Helper()
	var body struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(calls(t, "key_read_only.json", 0)[0].Body, &body); err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(body.ServerTime).UTC()
}

// X1-T415 — Req: US-203, US-204; Core §2.1.3. A snapshot of all four account types: GetBalances returns the rows of
// SPOT, FUNDING, EARN_FLEXIBLE and EARN_LOCKED, as_of and stale false.
func TestT415_GetBalances(t *testing.T) {
	h := setup(t)
	srv, id, k := h.binanceScenario(t, snapshotRun(t, "snapshot_full.json"))
	h.pass(t, h.engine(h.server))
	srv.AssertAllServed()
	conn, balances := h.balancesOf(t, id)
	if conn.GetStale() || conn.GetAsOf() == nil {
		t.Errorf("connection %v, want as_of and not stale", conn)
	}
	if got := rowsOf(balances); !slices.Equal(got, fullRows) {
		t.Errorf("balances =\n %v\nwant\n %v", got, fullRows)
	}
	assertKeyNotLogged(t, h, k)
}

// X1-T408, X1-T409 — Req: UC-202 step 5; EC-208; Core EC-109. Through the engine: an LD code without a flexible
// position (LDXYZ) and an asset without an alias (TSTX) are stored under their native code; each counts in
// unmapped_assets_total.
func TestT408_UnmappedCodesCounted(t *testing.T) {
	h := setup(t)
	run := withSpot(t, snapshotRun(t, "snapshot_full.json"), [3]string{"LDXYZ", "2", "0"}, [3]string{"TSTX", "1", "0"})
	_, id, _ := h.binanceScenario(t, run)
	h.pass(t, h.engine(h.server))
	_, balances := h.balancesOf(t, id)
	got := rowsOf(balances)
	for _, want := range []string{"SPOT LDXYZ LDXYZ 2/0", "SPOT TSTX TSTX 1/0"} {
		if !slices.Contains(got, want) {
			t.Errorf("balances %v, want %s", got, want)
		}
	}
	if unmapped := h.rec.snapshot().unmapped; !slices.Equal(unmapped, []string{"LDXYZ", "TSTX"}) {
		t.Errorf("unmapped assets reported = %v, want LDXYZ and TSTX", unmapped)
	}
}

// X1-T410 — Req: FR-204, EC-215, EC-211; Core FR-109. A snapshot exists; then each of the four sources fails in
// turn, with an error answer and with 429: no snapshot is written, GetBalances returns the previous one, stale once
// the last success is older than stale_after (2 × 15 min).
func TestT410_OneSourceFails(t *testing.T) {
	h := setup(t)
	full := snapshotRun(t, "snapshot_full.json") // account, funding, flexible, locked
	retry := map[string]string{"Retry-After": "30"}
	failures := [][]httpfixture.Call{
		failingAt(full, 0, 500, nil, ""),
		failingAt(full, 0, 429, retry, ""),
		snapshotRun(t, "snapshot_funding_fails.json"),
		failingAt(full, 1, 500, nil, ""),
		snapshotRun(t, "snapshot_flexible_fails.json"),
		failingAt(full, 2, 429, retry, ""),
		snapshotRun(t, "snapshot_locked_fails.json"),
		failingAt(full, 3, 429, retry, ""),
	}
	srv, id, _ := h.binanceScenario(t, append([][]httpfixture.Call{full}, failures...)...)
	e := h.engine(h.server)
	h.pass(t, e)
	first, _ := h.balancesOf(t, id)
	firstAsOf := first.GetAsOf().AsTime()

	for i := range failures {
		// 15 min after the success, then two hours apart: beyond every backoff and pause.
		h.clock.Advance(map[bool]time.Duration{true: 15 * time.Minute, false: 2 * time.Hour}[i == 0])
		h.pass(t, e)
		cur := h.cursor(t, id, "balances")
		if cur.failures != i+1 || h.snapshots(t, id) != 1 {
			t.Fatalf("failure %d: %d failures, %d snapshots; want %d and the previous snapshot only", i+1, cur.failures,
				h.snapshots(t, id), i+1)
		}
		conn, balances := h.balancesOf(t, id)
		if !conn.GetAsOf().AsTime().Equal(firstAsOf) || !slices.Equal(rowsOf(balances), fullRows) {
			t.Errorf("failure %d: GetBalances %v, want the previous snapshot", i+1, conn)
		}
		if wantStale := i > 0; conn.GetStale() != wantStale {
			t.Errorf("failure %d: stale %v, want %v", i+1, conn.GetStale(), wantStale)
		}
	}
	srv.AssertAllServed()
	if got := h.rec.snapshot().rateLimited; got != 4 {
		t.Errorf("rate-limited runs = %d, want 4: spot, funding, flexible, locked", got)
	}
}

// X1-T411 — Req: EC-228; Core §2.1.3; X1 D-9. A snapshot with BTC in SPOT; the next answers without BTC: the new
// snapshot has no row of BTC and GetBalances returns none: zero.
func TestT411_BalanceDroppedToZero(t *testing.T) {
	h := setup(t)
	full := snapshotRun(t, "snapshot_full.json")
	without := withSpot(t, full, [3]string{"USDT", "20", "1.5"})
	_, id, _ := h.binanceScenario(t, full, without)
	e := h.engine(h.server)
	h.pass(t, e)
	h.clock.Advance(15 * time.Minute)
	h.pass(t, e)
	if n := h.snapshots(t, id); n != 2 {
		t.Fatalf("%d snapshots, want 2", n)
	}
	_, balances := h.balancesOf(t, id)
	for _, r := range rowsOf(balances) {
		if strings.HasPrefix(r, "SPOT BTC") {
			t.Errorf("a SPOT row of BTC after it dropped to zero: %s", r)
		}
	}
}

// X1-T412, the engine part — Req: UC-202; X1 D-9. as_of of GetBalances is the server time at the first request of
// the run: the Binance clock of the creation's time call plus the time passed since, not the clock of the engine.
func TestT412_AsOfIsTheServerTime(t *testing.T) {
	h := setup(t)
	_, id, _ := h.binanceScenario(t, snapshotRun(t, "snapshot_full.json"))
	h.pass(t, h.engine(h.server))
	conn, _ := h.balancesOf(t, id)
	server := serverTimeOf(t)
	if asOf := conn.GetAsOf().AsTime(); asOf.Before(server) || asOf.After(server.Add(10*time.Second)) {
		t.Errorf("as_of %v, want the server time %v (a few seconds later at most), not the engine clock %v", asOf, server, h.clock.Now())
	}
}

// X1-T413, the engine part — Req: Core EC-117. Answers with a balance abc, a negative one, one with 19 decimals: each
// run fails, the snapshot is refused as a whole, the previous one stays.
func TestT413_MalformedAmountRefused(t *testing.T) {
	h := setup(t)
	full := snapshotRun(t, "snapshot_full.json")
	amounts := []string{"abc", "-1.5", "0.1234567890123456789"}
	runs := [][]httpfixture.Call{full}
	for _, a := range amounts {
		runs = append(runs, withSpot(t, full, [3]string{"USDT", a, "0"}))
	}
	srv, id, _ := h.binanceScenario(t, runs...)
	e := h.engine(h.server)
	h.pass(t, e)
	for i, a := range amounts {
		h.clock.Advance(2 * time.Hour)
		h.pass(t, e)
		cur := h.cursor(t, id, "balances")
		if cur.failures != i+1 || h.snapshots(t, id) != 1 || cur.lastError == nil || strings.Contains(*cur.lastError, a) {
			t.Errorf("%s: %+v, %d snapshots; want a failure without the amount, the previous snapshot only", a, cur, h.snapshots(t, id))
		}
	}
	srv.AssertAllServed()
	if _, balances := h.balancesOf(t, id); !slices.Equal(rowsOf(balances), fullRows) {
		t.Errorf("balances %v, want the previous snapshot", rowsOf(balances))
	}
}

// X1-T414 — Req: UC-202 trigger; US-210; Core FR-120, FR-122, EC-119. The declared stream balances is due at the
// creation; a run sets the next one after sync_interval.balances (15 min, or the value of sources.config);
// TriggerSync makes it due and runs it. A connection created without the cursor (st4) gets it at the engine start.
func TestT414_StreamAndTrigger(t *testing.T) {
	h := setup(t)
	full := snapshotRun(t, "snapshot_full.json")
	srv, id, _ := h.binanceScenario(t, full, full)
	created := h.clock.Now()
	if cur := h.cursor(t, id, "balances"); cur.mode != "INCREMENTAL" || cur.cursor != "{}" || !cur.nextRunAt.Equal(created) {
		t.Fatalf("balances at creation: %+v, want INCREMENTAL {} due at %s", cur, created)
	}
	e := h.engine(h.server)
	h.pass(t, e)
	if cur := h.cursor(t, id, "balances"); h.snapshots(t, id) != 1 || !cur.nextRunAt.Equal(created.Add(15*time.Minute)) {
		t.Fatalf("first run: %d snapshots, next %s; want 1 and %s", h.snapshots(t, id), cur.nextRunAt, created.Add(15*time.Minute))
	}

	h.exec(t, `UPDATE sources SET config = config || '{"sync_interval": {"balances": "20m"}}' WHERE code = 'binance'`)
	h.clock.Advance(2 * time.Minute)
	api, callCtx := h.apiClient(t)
	if _, err := api.TriggerSync(callCtx, &casv1.TriggerSyncRequest{ConnectionId: id.String()}); err != nil {
		t.Fatal(err)
	}
	h.pass(t, e)
	srv.AssertAllServed()
	if cur := h.cursor(t, id, "balances"); h.snapshots(t, id) != 2 || !cur.nextRunAt.Equal(h.clock.Now().Add(20*time.Minute)) {
		t.Errorf("triggered run: %d snapshots, next %s; want 2 and %s", h.snapshots(t, id), cur.nextRunAt, h.clock.Now().Add(20*time.Minute))
	}

	t.Run("an existing connection without the cursor gets it at the engine start", func(t *testing.T) {
		h.exec(t, `DELETE FROM sync_cursors WHERE connection_id = $1`, id)
		e := h.engine(h.server)
		e.CreateMissingCursors(ctx)
		if cur := h.cursor(t, id, "balances"); cur.mode != "INCREMENTAL" || !cur.nextRunAt.Equal(h.clock.Now()) {
			t.Errorf("balances after the start: %+v, want INCREMENTAL due now", cur)
		}
	})
}

// t107Kinds are the answers of X1-T106 that a run of the engine can get from the fake server, without a key
// rejection: the key rejections stop the connection and run one each.
var t107Kinds = []struct {
	status int
	body   string
}{
	{500, ""},
	{400, `{"code":-1001,"msg":"Internal error; unable to process your request. Please try again."}`},
	{400, `{"code":-1008,"msg":"Server is currently overloaded with other requests."}`},
	{403, ""},
	{400, `{"code":-1100,"msg":"Illegal characters found in a parameter."}`},
	{429, ""},
}

var t107KeyKinds = []struct {
	status int
	body   string
}{
	{401, ""},
	{400, `{"code":-2015,"msg":"Invalid API-key, IP, or permissions for action."}`},
	{400, `{"code":-2014,"msg":"API-key format invalid."}`},
	{400, `{"code":-1022,"msg":"Signature for this request is not valid."}`},
}

// assertNoSecretStored: the key, the secret and every signature sent appear in no last_error, audit row or log line.
func assertNoSecretStored(t *testing.T, h *harness, srv *httpfixture.Server, id uuid.UUID, k key) {
	t.Helper()
	forbidden := []string{k.apiKey, k.apiSecret, "signature="}
	for _, r := range srv.Received() {
		if _, sig, ok := strings.Cut(r.RawQuery, "&signature="); ok {
			forbidden = append(forbidden, sig)
		}
	}
	var stored string
	if err := h.owner.QueryRow(ctx, `SELECT coalesce((SELECT string_agg(coalesce(last_error, ''), ' ') FROM sync_cursors
		WHERE connection_id = $1), '') || ' ' || coalesce((SELECT string_agg(details::text, ' ') FROM audit_log WHERE object_id = $2), '')`,
		id, id.String()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, s := range forbidden {
		if strings.Contains(stored, s) || strings.Contains(h.log.String(), s) {
			t.Errorf("a secret or a signature in last_error, the audit rows or the log (%d characters)", len(s))
		}
	}
}

// X1-T107, the engine part — Req: SRS — Binance §3.2 Security; Core Connector contract Secrets; FR-217; X1 D-24.
// Runs of the engine that fail on each answer kind of X1-T106: no key, secret or signature in last_error, an audit
// row or the log.
func TestT107_EngineRunsKeepNoSecret(t *testing.T) {
	t.Run("failures that keep the connection", func(t *testing.T) {
		h := setup(t)
		var runs [][]httpfixture.Call
		for _, kind := range t107Kinds {
			runs = append(runs, failingAt(snapshotRun(t, "snapshot_full.json"), 0, kind.status, nil, kind.body))
		}
		srv, id, k := h.binanceScenario(t, runs...)
		e := h.engine(h.server)
		h.pass(t, e)
		for range len(t107Kinds) - 1 {
			h.clock.Advance(2 * time.Hour)
			h.pass(t, e)
		}
		srv.AssertAllServed()
		if cur := h.cursor(t, id, "balances"); cur.failures != len(t107Kinds) || cur.lastError == nil {
			t.Errorf("balances %+v, want %d failures", cur, len(t107Kinds))
		}
		assertNoSecretStored(t, h, srv, id, k)
	})
	for _, kind := range t107KeyKinds {
		t.Run(fmt.Sprintf("key rejected %d %s", kind.status, kind.body), func(t *testing.T) {
			h := setup(t)
			srv, id, k := h.binanceScenario(t, failingAt(snapshotRun(t, "snapshot_full.json"), 0, kind.status, nil, kind.body))
			h.pass(t, h.engine(h.server))
			srv.AssertAllServed()
			if got := h.status(t, id); got != "CREDENTIALS_INVALID" {
				t.Errorf("status %s, want CREDENTIALS_INVALID", got)
			}
			assertNoSecretStored(t, h, srv, id, k)
		})
	}
}

// X1-T209, the engine half — Req: SRS — Binance §2.5.1; Core §2.5.1; X1 D-16, X1 D-29. A run that ends in a rate
// limit adds 1 to rate_limit_rejections_total{source="binance"}; the connector counts the answer in
// binance_rate_limit_responses_total (X1 st3).
func TestT209_RateLimitRejectionsOfARun(t *testing.T) {
	h := setup(t)
	_, _, _ = h.binanceScenario(t, snapshotRun(t, "snapshot_funding_fails.json"))
	reg := prometheus.NewRegistry()
	e := h.engineWith(engine.NewPromReporter(reg), h.logger)
	h.pass(t, e)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var got float64
	for _, f := range families {
		if f.GetName() != "rate_limit_rejections_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "source" && l.GetValue() == "binance" {
					got = m.GetCounter().GetValue()
				}
			}
		}
	}
	if got != 1 {
		t.Errorf(`rate_limit_rejections_total{source="binance"} = %v, want 1`, got)
	}
}
