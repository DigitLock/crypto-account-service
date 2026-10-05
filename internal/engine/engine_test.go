// Phase 6 of docs/test-plan-c1.md (st7a part): the engine and the ledger writer on the pool of
// TEST_DATABASE_URL_SERVER, connections created through the registry, the fake scripted by the row, a fake
// clock. Every pass is called by the test.
package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/engine"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

// C1-T601 — Req: UC-102
func TestT601_FirstSync(t *testing.T) {
	h := setup(t)
	h.fake.SetPages("ops", pages("op", 3, 2))
	id, _ := h.create(t)

	h.pass(t, h.engine(h.server))

	if got := h.fakeCalls(id); !slices.Equal(got, []string{"balances:0", "ops:0", "ops:1", "ops:2"}) {
		t.Errorf("calls = %v, want one snapshot and three pages", got)
	}
	if n := len(h.entries(t, id)); n != 6 {
		t.Errorf("entries = %d, want 6", n)
	}
	if n := h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM snapshot_balances b JOIN balance_snapshots s ON s.id = b.snapshot_id
		WHERE s.connection_id = $1`, id); n != 2 {
		t.Errorf("balances = %d, want 2", n)
	}
	for stream, interval := range map[string]time.Duration{"balances": 15 * time.Minute, "ops": time.Hour} {
		c := h.cursor(t, id, stream)
		if c.lastSuccessAt == nil || !c.lastSuccessAt.Equal(testNow) || c.failures != 0 || c.lastError != nil ||
			!c.nextRunAt.Equal(testNow.Add(interval)) {
			t.Errorf("%s: %+v; want success at %v, next run at +%v", stream, c, testNow, interval)
		}
	}
	if got := h.rec.snapshot().runs; got["fake/balances/success"] != 1 || got["fake/ops/success"] != 1 {
		t.Errorf("reported runs = %v", got)
	}
}

// C1-T602 — Req: UC-102 step 1
func TestT602_Schedule(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	before := len(h.fakeCalls(id))

	h.clock.Set(testNow.Add(15*time.Minute - time.Second))
	h.pass(t, e)
	if got := len(h.fakeCalls(id)); got != before {
		t.Errorf("%d calls before next_run_at", got-before)
	}
	h.clock.Set(testNow.Add(15 * time.Minute))
	h.pass(t, e)
	if got := h.fakeCalls(id)[before:]; !slices.Equal(got, []string{"balances:0"}) {
		t.Errorf("calls at next_run_at = %v, want one snapshot", got)
	}
}

// resetOps puts the cursor of ops back to the start and makes it due.
func (h *harness) resetOps(t *testing.T, id uuid.UUID) {
	t.Helper()
	h.exec(t, `UPDATE sync_cursors SET cursor = '{}', mode = 'BACKFILL', next_run_at = $2
		WHERE connection_id = $1 AND stream = 'ops'`, id, h.clock.Now())
}

// dueNow makes a stream due at the time of the clock.
func (h *harness) dueNow(t *testing.T, id uuid.UUID, stream string) {
	t.Helper()
	h.exec(t, `UPDATE sync_cursors SET next_run_at = $3 WHERE connection_id = $1 AND stream = $2`, id, stream, h.clock.Now())
}

// C1-T603 — Req: FR-106, EC-105
func TestT603_RepeatedSync(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	stored := len(h.entries(t, id))
	before := h.rec.snapshot()

	h.resetOps(t, id)
	h.pass(t, e)
	after := h.rec.snapshot()
	if n := len(h.entries(t, id)); n != stored || after.inserted != before.inserted {
		t.Errorf("entries %d → %d, inserted %d → %d; want no new entry", stored, n, before.inserted, after.inserted)
	}
	if skipped := after.skipped - before.skipped; skipped != stored {
		t.Errorf("skipped = %d, want %d", skipped, stored)
	}
}

// C1-T604 — Req: EC-105, FR-106
func TestT604_OverlappingPage(t *testing.T) {
	h := setup(t)
	a := fake.NewEntry("a", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime)
	b := fake.NewEntry("b", "SINGLE", "DEPOSIT", "IN", "BTC", "2", fake.DefaultTime.Add(time.Minute))
	c := fake.NewEntry("c", "SINGLE", "DEPOSIT", "IN", "BTC", "3", fake.DefaultTime.Add(2*time.Minute))
	h.fake.SetPages("ops", [][]connector.Entry{{a, b}, {b, c}})
	id, _ := h.create(t)

	h.pass(t, h.engine(h.server))
	if got := keys(h.entries(t, id)); !slices.Equal(got, []string{"a/SINGLE", "b/SINGLE", "c/SINGLE"}) {
		t.Errorf("entries = %v", got)
	}
	if r := h.rec.snapshot(); r.inserted != 3 || r.skipped != 1 {
		t.Errorf("inserted %d, skipped %d; want 3 and 1", r.inserted, r.skipped)
	}
	if c := h.cursor(t, id, "ops"); c.cursor != `{"page": 2}` {
		t.Errorf("cursor = %s, want past both pages", c.cursor)
	}
}

// failingCursorDB wraps the database of the engine: inside a transaction the cursor update fails, after the
// entries were inserted. It goes through engine.DB, the interface the production code uses.
type failingCursorDB struct{ engine.DB }

func (d failingCursorDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return failingCursorTx{tx}, nil
}

type failingCursorTx struct{ pgx.Tx }

func (tx failingCursorTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "SET cursor =") {
		return pgconn.CommandTag{}, errors.New("injected failure at the cursor update")
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// C1-T605 — Req: FR-107
func TestT605_EntriesAndCursorTogether(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	before := h.cursor(t, id, "ops")

	h.pass(t, h.engine(failingCursorDB{h.server}))

	if n := len(h.entries(t, id)); n != 0 {
		t.Errorf("entries = %d, want none: they were inserted in the failed transaction", n)
	}
	c := h.cursor(t, id, "ops")
	if c.cursor != before.cursor || c.mode != before.mode {
		t.Errorf("cursor %s %s → %s %s, want unchanged", before.mode, before.cursor, c.mode, c.cursor)
	}
	if c.failures != 1 || c.lastError == nil || !strings.Contains(*c.lastError, "injected failure") {
		t.Errorf("health = %+v, want one failure", c)
	}
}

// C1-T606 — Req: FR-108, EC-106
func TestT606_ResumeAfterAStop(t *testing.T) {
	h := setup(t)
	h.fake.SetPages("ops", pages("op", 5, 3))
	id, _ := h.create(t)

	runCtx, cancel := context.WithCancel(ctx)
	h.fake.SetBefore(func(ctx context.Context, call fake.Call) error {
		if call.Stream == "ops" && call.Page == 2 {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	if err := h.engine(h.server).RunPass(runCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunPass = %v, want context.Canceled", err)
	}
	if n := len(h.entries(t, id)); n != 6 {
		t.Errorf("entries after the stop = %d, want the 6 of pages 1 and 2", n)
	}
	if c := h.cursor(t, id, "ops"); c.cursor != `{"page": 2}` || c.failures != 0 || c.lastSuccessAt != nil {
		t.Errorf("after the stop: %+v; want the cursor at page 2 and no health stored", c)
	}

	h.fake.SetBefore(nil)
	h.pass(t, h.engine(h.server)) // a new engine value
	if got, want := keys(h.entries(t, id)), keysOfPages(pages("op", 5, 3)); !slices.Equal(got, want) {
		t.Errorf("ledger = %v, want every entry of the five pages once", got)
	}
	if got := h.fakeCalls(id); !slices.Equal(got[len(got)-3:], []string{"ops:2", "ops:3", "ops:4"}) {
		t.Errorf("calls = %v, want the new engine to continue at page 2", got)
	}
}

func keysOfPages(ps [][]connector.Entry) []string {
	var es []entry
	for _, p := range ps {
		for _, e := range p {
			es = append(es, entry{externalID: e.ExternalID, leg: e.Leg})
		}
	}
	return keys(es)
}

// C1-T607 — Req: UC-102 Stream modes
func TestT607_ModeSwitch(t *testing.T) {
	h := setup(t)
	h.fake.SetPages("ops", pages("op", 2, 1))
	id, _ := h.create(t)
	if c := h.cursor(t, id, "ops"); c.mode != "BACKFILL" {
		t.Fatalf("first mode = %s, want BACKFILL", c.mode)
	}
	h.pass(t, h.engine(h.server))
	if c := h.cursor(t, id, "ops"); c.mode != "INCREMENTAL" {
		t.Errorf("mode at the end of history = %s, want INCREMENTAL", c.mode)
	}
}

// C1-T608 — Req: ADR-5, §2.1.4
func TestT608_Legs(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	h.resetOps(t, id)
	h.pass(t, e)

	var legs []string
	for _, en := range h.entries(t, id) {
		if en.externalID == "trade-1" {
			legs = append(legs, en.leg)
			if en.groupID != "ops:trade-1" || en.stream != "ops" {
				t.Errorf("leg %s: group_id %s, stream %s; want ops:trade-1 and ops", en.leg, en.groupID, en.stream)
			}
		}
	}
	slices.Sort(legs)
	if !slices.Equal(legs, []string{"BASE", "FEE", "QUOTE"}) {
		t.Errorf("legs of trade-1 = %v, want three after two runs", legs)
	}
}

// C1-T609 — Req: FR-113
func TestT609_RawRecord(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	h.pass(t, h.engine(h.server))

	delivered := map[string]json.RawMessage{}
	for _, p := range fake.DefaultHistory() {
		for _, e := range p {
			delivered[e.ExternalID+"/"+e.Leg] = e.Raw
		}
	}
	stored := h.entries(t, id)
	if len(stored) != len(delivered) {
		t.Fatalf("entries = %d, want %d", len(stored), len(delivered))
	}
	for _, e := range stored {
		var got, want any
		_ = json.Unmarshal(e.raw, &got)
		_ = json.Unmarshal(delivered[e.externalID+"/"+e.leg], &want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s/%s: raw %s, want %s", e.externalID, e.leg, e.raw, delivered[e.externalID+"/"+e.leg])
		}
	}
}

// C1-T610 — Req: FR-111
func TestT610_ImmutableEntries(t *testing.T) {
	h := setup(t)
	first := fake.NewEntry("op-1", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime)
	h.fake.SetPages("ops", [][]connector.Entry{{first}})
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)

	changed := fake.NewEntry("op-1", "SINGLE", "DEPOSIT", "IN", "BTC", "2", fake.DefaultTime)
	h.fake.SetPages("ops", [][]connector.Entry{{changed}})
	h.resetOps(t, id)
	h.pass(t, e)

	stored := h.entries(t, id)
	if len(stored) != 1 || stored[0].amount != "1.000000000000000000" {
		t.Errorf("entries = %+v, want the first amount unchanged", stored)
	}
}

// C1-T611 — Req: EC-110, FR-111
func TestT611_NotFinalYet(t *testing.T) {
	h := setup(t)
	a := fake.NewEntry("a", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime)
	b := fake.NewEntry("b", "SINGLE", "DEPOSIT", "IN", "BTC", "2", fake.DefaultTime.Add(time.Minute))
	h.fake.SetPages("ops", [][]connector.Entry{{a, b}})
	h.fake.SetNotFinal("b", true)
	id, _ := h.create(t)
	e := h.engine(h.server)

	h.pass(t, e)
	if got := keys(h.entries(t, id)); !slices.Equal(got, []string{"a/SINGLE"}) {
		t.Errorf("after the first run: %v, want only the final record", got)
	}
	h.fake.SetNotFinal("b", false)
	h.clock.Advance(time.Hour)
	h.pass(t, e)
	if got := keys(h.entries(t, id)); !slices.Equal(got, []string{"a/SINGLE", "b/SINGLE"}) {
		t.Errorf("after the second run: %v, want the record imported once final", got)
	}
}

// C1-T612 — Req: FR-114. Several connections run from goroutines at once; a reader pulls by seq > last.
func TestT612_SeqInCommitOrder(t *testing.T) {
	h := setup(t)
	h.fake.SetPages("ops", pages("op", 6, 10))
	var ids []uuid.UUID
	for range 5 {
		id, _ := h.create(t)
		ids = append(ids, id)
	}
	e := h.engine(h.server)

	seen := map[int64]bool{}
	var last int64
	read := func() {
		rows, err := h.server.Query(ctx, `SELECT seq FROM ledger_entries WHERE seq > $1 ORDER BY seq`, last)
		if err != nil {
			t.Error(err)
			return
		}
		seqs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			t.Error(err)
			return
		}
		for _, s := range seqs {
			seen[s] = true
			last = s
		}
	}
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				read()
				return
			default:
				read()
			}
		}
	}()

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if err := e.RunConnection(ctx, id); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(stop)
	<-readerDone

	all := h.count(t, `SELECT count(*) FROM ledger_entries`)
	if all != 5*60 {
		t.Fatalf("entries = %d, want 300", all)
	}
	if len(seen) != all {
		t.Errorf("the reader saw %d of %d entries: an entry was committed behind a higher seq", len(seen), all)
	}
}

// C1-T613 — Req: §2.1.4, FR-114
func TestT613_BackfilledOperation(t *testing.T) {
	h := setup(t)
	newer := fake.NewEntry("new-1", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime.AddDate(0, 0, 10))
	h.fake.SetPages("ops", [][]connector.Entry{{newer}})
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)

	older := fake.NewEntry("old-1", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime.AddDate(0, 0, -10))
	h.fake.SetPages("ops", [][]connector.Entry{{older}})
	h.resetOps(t, id)
	h.pass(t, e)

	stored := h.entries(t, id)
	if len(stored) != 2 || stored[0].externalID != "new-1" || stored[1].externalID != "old-1" || stored[1].seq <= stored[0].seq {
		t.Errorf("entries in seq order = %+v, want the older operation after the newer one", stored)
	}
}

// C1-T614 — Req: EC-109, UC-102 step 4
func TestT614_AssetMapping(t *testing.T) {
	h := setup(t)
	h.exec(t, `INSERT INTO asset_aliases (source_id, native_asset, asset) SELECT id, 'XBT', 'BTC' FROM sources WHERE code = 'fake'`)
	h.fake.SetPages("ops", [][]connector.Entry{{
		fake.NewEntry("x", "SINGLE", "DEPOSIT", "IN", "XBT", "1", fake.DefaultTime),
		fake.NewEntry("z", "SINGLE", "DEPOSIT", "IN", "ZZZ", "1", fake.DefaultTime),
	}})
	id, _ := h.create(t)
	h.pass(t, h.engine(h.server))

	for _, e := range h.entries(t, id) {
		want := map[string]string{"x": "BTC", "z": "ZZZ"}[e.externalID]
		if e.asset != want {
			t.Errorf("%s: asset %s (native %s), want %s", e.externalID, e.asset, e.native, want)
		}
	}
	if got := h.rec.snapshot().unmapped; !slices.Equal(got, []string{"ZZZ"}) {
		t.Errorf("unmapped assets reported = %v, want [ZZZ]", got)
	}
	if !strings.Contains(h.log.String(), `"native_asset":"ZZZ"`) {
		t.Errorf("the unmapped asset is not logged:\n%s", h.log.String())
	}
}

// C1-T615 — Req: §2.4
func TestT615_Snapshot(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	var first uuid.UUID
	if err := h.owner.QueryRow(ctx, `SELECT id FROM balance_snapshots WHERE connection_id = $1`, id).Scan(&first); err != nil {
		t.Fatal(err)
	}
	h.clock.Advance(15 * time.Minute)
	h.pass(t, e)

	if n := h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 2 {
		t.Errorf("snapshots = %d, want 2", n)
	}
	for _, q := range []string{
		`SELECT count(*) FROM snapshot_balances WHERE snapshot_id = $1`,
		`SELECT count(*) FROM snapshot_balances WHERE snapshot_id = (SELECT id FROM balance_snapshots WHERE connection_id =
			(SELECT connection_id FROM balance_snapshots WHERE id = $1) ORDER BY created_at DESC LIMIT 1)`,
	} {
		if n := h.count(t, q, first); n != 2 {
			t.Errorf("balances of a snapshot = %d, want 2", n)
		}
	}
}

// latestSnapshot returns the latest snapshot of a connection.
func (h *harness) latestSnapshot(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	var s uuid.UUID
	if err := h.owner.QueryRow(ctx, `SELECT id FROM balance_snapshots WHERE connection_id = $1
		ORDER BY created_at DESC, id LIMIT 1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// C1-T616 — Req: UC-102 step 6, FR-109
func TestT616_FailedSnapshot(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	first := h.latestSnapshot(t, id)

	h.clock.Advance(15 * time.Minute)
	h.fake.FailNext("balances", errors.New("fake: balance call failed"))
	h.pass(t, e)
	if n := h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 1 || h.latestSnapshot(t, id) != first {
		t.Errorf("snapshots = %d; want the previous one to stay the latest", n)
	}
	if c := h.cursor(t, id, "balances"); c.failures != 1 {
		t.Errorf("balances failures = %d, want 1", c.failures)
	}
}

// C1-T617 — Req: UC-102 step 8, §3.1
func TestT617_FailureAndBackoff(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	want := []time.Duration{30, 60, 120, 240, 480, 960, 1920, 3600, 3600}
	for range want {
		h.fake.FailNext("ops", errors.New("fake: unexpected answer"))
	}

	for i, seconds := range want {
		h.clock.Set(h.cursor(t, id, "ops").nextRunAt)
		now := h.clock.Now()
		h.pass(t, e)
		c := h.cursor(t, id, "ops")
		if c.failures != i+1 || c.lastError == nil || *c.lastError != "fake: unexpected answer" ||
			!c.nextRunAt.Equal(now.Add(seconds*time.Second)) {
			t.Fatalf("failure %d: %+v; want counter %d and next run after %ds", i+1, c, i+1, seconds)
		}
	}
	h.clock.Set(h.cursor(t, id, "ops").nextRunAt)
	h.pass(t, e)
	if c := h.cursor(t, id, "ops"); c.failures != 0 || c.lastError != nil {
		t.Errorf("after the success: %+v, want the counter zeroed and no error", c)
	}
}

// C1-T618 — Req: EC-111, §2.3.1, FR-117. Reads are checked with GetConnection.
func TestT618_DegradedAndRecovered(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	api, callCtx := h.apiClient(t)
	h.pass(t, e) // both streams synced once
	balancesNext := h.cursor(t, id, "balances").nextRunAt

	for i := 1; i <= 5; i++ {
		h.fake.FailNext("ops", errors.New("fake: ops failed"))
		h.dueNow(t, id, "ops")
		h.pass(t, e)
		want := "ACTIVE"
		if i == 5 {
			want = "DEGRADED"
		}
		if got := h.status(t, id); got != want {
			t.Fatalf("after %d failures: %s, want %s", i, got, want)
		}
	}
	if got := h.cursor(t, id, "balances").nextRunAt; !got.Equal(balancesNext) {
		t.Errorf("balances next_run_at moved: %v, want %v", got, balancesNext)
	}
	resp, err := api.GetConnection(callCtx, &casv1.GetConnectionRequest{ConnectionId: id.String()})
	if err != nil || resp.GetConnection().GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_DEGRADED {
		t.Errorf("GetConnection while degraded = %v, %v", resp.GetConnection().GetStatus(), err)
	}
	h.dueNow(t, id, "ops")
	h.pass(t, e)
	if got := h.status(t, id); got != "ACTIVE" {
		t.Errorf("after the success: %s, want ACTIVE", got)
	}

	// Both streams over the threshold: ACTIVE only when both recovered.
	for range 5 {
		h.fake.FailNext("ops", errors.New("fake: ops failed"))
		h.fake.FailNext("balances", errors.New("fake: balances failed"))
		h.dueNow(t, id, "ops")
		h.dueNow(t, id, "balances")
		h.pass(t, e)
	}
	if got := h.status(t, id); got != "DEGRADED" {
		t.Fatalf("both streams failing: %s, want DEGRADED", got)
	}
	h.exec(t, `UPDATE sync_cursors SET next_run_at = $2 WHERE connection_id = $1 AND stream = 'balances'`, id, h.clock.Now().Add(time.Hour))
	h.dueNow(t, id, "ops")
	h.pass(t, e)
	if got := h.status(t, id); got != "DEGRADED" {
		t.Errorf("ops recovered, balances still failing: %s, want DEGRADED", got)
	}
	h.dueNow(t, id, "balances")
	h.pass(t, e)
	if got := h.status(t, id); got != "ACTIVE" {
		t.Errorf("both recovered: %s, want ACTIVE", got)
	}

	rows, err := h.owner.Query(ctx, `SELECT action || ':' || (credential_id IS NULL)::text FROM audit_log
		WHERE object_id = $1 AND action <> 'CONNECTION_CREATED' ORDER BY id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	audits, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(audits, []string{"CONNECTION_DEGRADED:true", "CONNECTION_RECOVERED:true", "CONNECTION_DEGRADED:true", "CONNECTION_RECOVERED:true"}) {
		t.Errorf("audit rows = %v", audits)
	}
}

// C1-T619 — Req: EC-108, FR-117
func TestT619_KeyRejectedDuringASync(t *testing.T) {
	for name, c := range map[string]struct {
		err    error
		reason string
	}{
		"key rejected":      {fmt.Errorf("fake: %w", connector.ErrKeyRejected), "key_rejected"},
		"key not read-only": {&connector.KeyNotReadOnlyError{Permissions: []string{"TRADE"}}, "key_not_read_only"},
	} {
		keyErr, reason := c.err, c.reason
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			id, _ := h.create(t)
			e := h.engine(h.server)
			h.fake.FailNext("ops", keyErr)
			h.pass(t, e)

			if got := h.status(t, id); got != "CREDENTIALS_INVALID" {
				t.Fatalf("status = %s, want CREDENTIALS_INVALID", got)
			}
			if c := h.cursor(t, id, "ops"); c.failures != 1 || c.lastError == nil {
				t.Errorf("ops = %+v, want the failure stored", c)
			}
			if n := h.count(t, `SELECT count(*) FROM audit_log WHERE object_id = $1 AND action = 'CREDENTIALS_INVALID'
				AND credential_id IS NULL AND details->>'reason' = $2 AND details->>'stream' = 'ops'`,
				id.String(), reason); n != 1 {
				t.Errorf("CREDENTIALS_INVALID audit rows with reason %s = %d, want 1", reason, n)
			}
			calls := len(h.fakeCalls(id))
			h.clock.Advance(3 * time.Hour)
			h.pass(t, e)
			if got := len(h.fakeCalls(id)); got != calls {
				t.Errorf("%d calls after the key was refused, want none", got-calls)
			}
		})
	}
}

// C1-T622 — Req: FR-110
func TestT622_OneLimiterPerSource(t *testing.T) {
	h := setup(t)
	h.create(t)
	h.create(t)
	h.pass(t, h.engine(h.server))

	var calls []fake.Call
	for _, c := range h.fake.Calls() {
		if !c.Reserved {
			t.Errorf("a %s call of %s carried no reservation", c.Kind, c.Stream)
		}
		if c.Kind != "check" { // the key check of CreateConnection passes a bounded view of the same limiter
			calls = append(calls, c)
		}
	}
	if len(calls) != 6 {
		t.Fatalf("page and snapshot calls = %d, want 3 per connection", len(calls))
	}
	connections := map[string]bool{}
	for _, c := range calls {
		connections[c.Connection] = true
		if c.Limiter != calls[0].Limiter {
			t.Error("the two connections of one source draw on different limiters")
		}
	}
	if len(connections) != 2 {
		t.Errorf("calls of %d connections, want 2", len(connections))
	}
}

// C1-T623 — Req: UC-102 step 2, FR-110. The waiting is driven by the fake clock.
func TestT623_BudgetSpent(t *testing.T) {
	h := setup(t)
	h.fake.SetBudgets([]connector.Budget{{Name: fake.DefaultBudget, Units: 2, Window: time.Minute}})
	h.fake.SetPages("ops", pages("op", 4, 1))
	id, _ := h.create(t)
	e := h.engine(h.server)

	moves := h.drive(t, func() error { return e.RunPass(ctx) })
	if moves != 2 {
		t.Errorf("the run waited %d times, want 2: five calls on 2 units per minute", moves)
	}
	if n := len(h.entries(t, id)); n != 4 {
		t.Errorf("entries = %d, want 4: the run continued after waiting", n)
	}
	if c := h.cursor(t, id, "ops"); c.failures != 0 || c.lastSuccessAt == nil {
		t.Errorf("ops = %+v, want a success", c)
	}
	if w := h.rec.snapshot().waited; w != 2*time.Minute {
		t.Errorf("reported wait = %v, want 2m", w)
	}
}

// C1-T624 — Req: EC-107, FR-110. Two budgets; the waiting is driven by the fake clock.
func TestT624_RateLimitExceeded(t *testing.T) {
	h := setup(t)
	h.fake.SetBudgets([]connector.Budget{
		{Name: "balances-budget", Units: 100, Window: time.Minute},
		{Name: "ops-budget", Units: 100, Window: time.Minute},
	})
	h.fake.SetCost("balances", fake.Cost{Budget: "balances-budget", Units: 1})
	h.fake.SetCost("ops", fake.Cost{Budget: "ops-budget", Units: 1})
	h.fake.SetCost(fake.CheckStream, fake.Cost{Budget: "balances-budget", Units: 1})
	h.fake.FailNext("ops", &connector.RateLimitError{Budget: "ops-budget", Pause: 60 * time.Second})
	id, _ := h.create(t)
	e := h.engine(h.server)

	h.pass(t, e)
	c := h.cursor(t, id, "ops")
	if c.failures != 1 || !c.nextRunAt.Equal(testNow.Add(60*time.Second)) {
		t.Errorf("ops = %+v, want a failure and the next run at the end of the pause, not after the 30 s backoff", c)
	}
	if r := h.rec.snapshot(); r.rateLimited != 1 || r.runs["fake/ops/failure"] != 1 {
		t.Errorf("reported: %d rate limits, runs %v", r.rateLimited, r.runs)
	}

	// Both streams due 10 s later: the other budget goes on, ops waits for the end of the pause.
	h.clock.Advance(10 * time.Second)
	h.dueNow(t, id, "ops")
	h.dueNow(t, id, "balances")
	callsBefore := len(h.fakeCalls(id))
	done := make(chan error, 1)
	go func() { done <- e.RunPass(ctx) }()
	var until time.Time
	for range 2000 {
		var ok bool
		if until, ok = h.clock.earliestWaiter(); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !until.Equal(testNow.Add(60 * time.Second)) {
		t.Fatalf("ops waits until %v, want the end of the pause %v", until, testNow.Add(60*time.Second))
	}
	if got := h.fakeCalls(id)[callsBefore:]; !slices.Equal(got, []string{"balances:0"}) {
		t.Errorf("calls during the pause = %v, want only the other budget", got)
	}
	h.clock.Set(until)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := h.fakeCalls(id)[callsBefore:]; len(got) < 2 || got[1] != "ops:0" {
		t.Errorf("calls after the pause = %v, want ops", got)
	}
}

// C1-T627 — Req: §2.1.1 Connector contract, FR-103
func TestT627_LastErrorWithoutSecrets(t *testing.T) {
	h := setup(t)
	id, k := h.create(t)
	long := strings.Repeat("x", 600)
	h.fake.FailNext("ops", fmt.Errorf("fake: refused key %s with secret %s: %s", k.apiKey, k.apiSecret, long))
	h.pass(t, h.engine(h.server))

	c := h.cursor(t, id, "ops")
	if c.lastError == nil {
		t.Fatal("no last_error")
	}
	if strings.Contains(*c.lastError, k.apiKey) || strings.Contains(*c.lastError, k.apiSecret) ||
		!strings.Contains(*c.lastError, "fake: refused key [redacted] with secret [redacted]") {
		t.Errorf("last_error = %.120q…, want the key and the secret replaced", *c.lastError)
	}
	if n := utf8.RuneCountInString(*c.lastError); n != 500 {
		t.Errorf("last_error has %d characters, want 500", n)
	}
	if out := h.log.String(); strings.Contains(out, k.apiKey) || strings.Contains(out, k.apiSecret) {
		t.Error("the log contains the key or the secret")
	}
}

// C1-T629 — Req: EC-117. internal/ledger checks every rule; this row checks that nothing is stored.
func TestT629_InvalidEntryOrBalance(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.fake.SetPages("ops", nil) // the first run stores the snapshot only
	h.pass(t, e)
	snapshot := h.latestSnapshot(t, id)
	valid := fake.NewEntry("ok", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime)

	invalid := map[string]func(*connector.Entry){
		"amount 0":          func(e *connector.Entry) { e.Amount = "0" },
		"negative amount":   func(e *connector.Entry) { e.Amount = "-1" },
		"19 decimal places": func(e *connector.Entry) { e.Amount = "0." + strings.Repeat("1", 19) },
		"21 integer digits": func(e *connector.Entry) { e.Amount = "1" + strings.Repeat("0", 20) },
		"empty external_id": func(e *connector.Entry) { e.ExternalID = "" },
		"unknown type":      func(e *connector.Entry) { e.Type = "AIRDROP" },
		"unknown leg":       func(e *connector.Entry) { e.Leg = "OTHER" },
		"unknown direction": func(e *connector.Entry) { e.Direction = "SIDEWAYS" },
	}
	failures := h.cursor(t, id, "ops").failures
	for name, mutate := range invalid {
		bad := fake.NewEntry("bad", "SINGLE", "DEPOSIT", "IN", "BTC", "1", fake.DefaultTime)
		mutate(&bad)
		h.fake.SetPages("ops", [][]connector.Entry{{valid, bad}})
		h.exec(t, `UPDATE sync_cursors SET cursor = '{}', next_run_at = $2 WHERE connection_id = $1 AND stream = 'ops'`, id, h.clock.Now())
		h.pass(t, e)
		failures++
		c := h.cursor(t, id, "ops")
		if n := len(h.entries(t, id)); n != 0 || c.failures != failures || c.cursor != "{}" {
			t.Errorf("%s: %d entries, %d failures, cursor %s; want nothing stored and a failure", name, n, c.failures, c.cursor)
		}
	}

	okBalance := connector.Balance{AccountType: "SPOT", NativeAsset: "BTC", Free: "1", Locked: "0"}
	badBalances := map[string]connector.Balance{
		"negative":             {AccountType: "SPOT", NativeAsset: "USDT", Free: "-1", Locked: "0"},
		"19 decimal places":    {AccountType: "SPOT", NativeAsset: "USDT", Free: "0." + strings.Repeat("1", 19), Locked: "0"},
		"21 integer digits":    {AccountType: "SPOT", NativeAsset: "USDT", Free: "1" + strings.Repeat("0", 20), Locked: "0"},
		"unknown account type": {AccountType: "MARGIN", NativeAsset: "USDT", Free: "1", Locked: "0"},
		"same type and asset":  okBalance,
	}
	snapFailures := h.cursor(t, id, "balances").failures
	for name, bad := range badBalances {
		h.fake.SetSnapshot(connector.Snapshot{TakenAt: fake.DefaultTime, Balances: []connector.Balance{okBalance, bad}})
		h.dueNow(t, id, "balances")
		h.pass(t, e)
		snapFailures++
		if n := h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 1 ||
			h.latestSnapshot(t, id) != snapshot || h.cursor(t, id, "balances").failures != snapFailures {
			t.Errorf("%s: %d snapshots; want the previous one to stay the latest and a failure", name, n)
		}
	}
}

// C1-T632 — Req: UC-102 step 6, §2.4
func TestT632_EmptySnapshot(t *testing.T) {
	h := setup(t)
	h.fake.SetSnapshot(connector.Snapshot{TakenAt: fake.DefaultTime})
	id, _ := h.create(t)
	h.pass(t, h.engine(h.server))

	if n := h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM snapshot_balances b JOIN balance_snapshots s ON s.id = b.snapshot_id
		WHERE s.connection_id = $1`, id); n != 0 {
		t.Errorf("balances = %d, want none", n)
	}
	if c := h.cursor(t, id, "balances"); c.failures != 0 || c.lastSuccessAt == nil {
		t.Errorf("balances = %+v, want a success", c)
	}
}

// C1-T633 — Req: UC-105, UC-102 preconditions
func TestT633_DisabledTenantUnavailableSource(t *testing.T) {
	h := setup(t)
	id, _ := h.create(t)
	e := h.engine(h.server)
	h.pass(t, e)
	stored := len(h.entries(t, id))

	assertPaused := func(what string) {
		t.Helper()
		calls := len(h.fakeCalls(id))
		ops, bal := h.cursor(t, id, "ops"), h.cursor(t, id, "balances")
		h.clock.Advance(2 * time.Hour)
		h.pass(t, e)
		if got := len(h.fakeCalls(id)); got != calls {
			t.Errorf("%s: %d calls, want none", what, got-calls)
		}
		if a, b := h.cursor(t, id, "ops"), h.cursor(t, id, "balances"); !reflect.DeepEqual(a, ops) ||
			!reflect.DeepEqual(b, bal) || a.failures != 0 || b.failures != 0 {
			t.Errorf("%s: cursors changed or a failure was counted", what)
		}
	}
	assertResumed := func(what string) {
		t.Helper()
		calls := len(h.fakeCalls(id))
		h.pass(t, e)
		if got := h.fakeCalls(id)[calls:]; !slices.Equal(got, []string{"balances:0", "ops:2"}) {
			t.Errorf("%s: calls = %v, want both streams from their cursors", what, got)
		}
		if n := len(h.entries(t, id)); n != stored {
			t.Errorf("%s: entries = %d, want %d", what, n, stored)
		}
	}

	if _, err := h.reg.DisableTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	assertPaused("tenant disabled")
	if _, err := h.reg.EnableTenant(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	assertResumed("tenant enabled")

	h.exec(t, `UPDATE sources SET enabled = false WHERE code = 'fake'`)
	assertPaused("source disabled")
	h.exec(t, `UPDATE sources SET enabled = true WHERE code = 'fake'`)
	assertResumed("source enabled")
}

// C1-T635 — Req: §2.1.1 Connector contract, §3.1
func TestT635_IntervalFromTheConnector(t *testing.T) {
	h := setup(t)
	streams := fake.DefaultStreams()
	streams[0].Interval = 5 * time.Minute
	streams[1].Interval = 20 * time.Minute
	h.fake.SetStreams(streams)
	id, _ := h.create(t)
	h.pass(t, h.engine(h.server))

	for stream, interval := range map[string]time.Duration{"balances": 5 * time.Minute, "ops": 20 * time.Minute} {
		if c := h.cursor(t, id, stream); !c.nextRunAt.Equal(testNow.Add(interval)) {
			t.Errorf("%s next_run_at = %v, want now + %v", stream, c.nextRunAt, interval)
		}
	}
}

// C1-T636 — Req: UC-102 step 7, §3.1
func TestT636_PageLimitPerRun(t *testing.T) {
	h := setup(t)
	h.cfg.MaxPagesPerRun = 3
	h.fake.SetPages("ops", pages("op", 7, 1))
	id, _ := h.create(t)
	e := h.engine(h.server)

	h.pass(t, e) // run 1: three pages, due at once
	if c := h.cursor(t, id, "ops"); c.failures != 0 || c.lastSuccessAt == nil || !c.nextRunAt.Equal(testNow) {
		t.Errorf("after run 1: %+v; want a success due at once", c)
	}
	h.clock.Advance(15 * time.Minute)
	h.pass(t, e) // run 2: three pages; the balance stream, due since, runs in the same pass
	if c := h.cursor(t, id, "ops"); !c.nextRunAt.Equal(h.clock.Now()) {
		t.Errorf("after run 2: next_run_at %v, want now", c.nextRunAt)
	}
	h.pass(t, e) // run 3: the last page

	want := []string{"balances:0", "ops:0", "ops:1", "ops:2", "ops:3", "ops:4", "ops:5", "balances:0", "ops:6"}
	if got := h.fakeCalls(id); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
	if c := h.cursor(t, id, "ops"); !c.nextRunAt.Equal(h.clock.Now().Add(time.Hour)) || c.mode != "INCREMENTAL" {
		t.Errorf("after run 3: %+v; want the end of the backfill", c)
	}
	if got, want := keys(h.entries(t, id)), keysOfPages(pages("op", 7, 1)); !slices.Equal(got, want) {
		t.Errorf("ledger = %v, want every entry of the seven pages once", got)
	}
	if r := h.rec.snapshot(); r.runs["fake/ops/success"] != 3 {
		t.Errorf("ops runs = %v, want three successes", r.runs)
	}
}
