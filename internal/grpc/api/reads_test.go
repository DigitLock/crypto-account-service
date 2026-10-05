// Phase 7 of docs/test-plan-c1.md: GetBalances, ListLedgerEntries and TriggerSync through the server of
// cmd/server in process, on the pool of cas_server. Data comes from engine runs with the scripted fake and
// the clock of the test, or from rows inserted through the owner pool where the row says so.
package api_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/fake"
	"github.com/DigitLock/crypto-account-service/internal/grpc/api"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
)

func accountClient(conn *grpc.ClientConn) casv1.AccountDataServiceClient {
	return casv1.NewAccountDataServiceClient(conn)
}

// pass runs every due work once with a new engine value at the time of the test clock.
func (e *env) pass(t *testing.T) {
	t.Helper()
	if err := e.engine().RunPass(ctx); err != nil {
		t.Fatal(err)
	}
}

func (e *env) exec(t *testing.T, stmt string, args ...any) {
	t.Helper()
	if _, err := e.owner.Exec(ctx, stmt, args...); err != nil {
		t.Fatal(err)
	}
}

func balancesBy(t *testing.T, ac casv1.AccountDataServiceClient, who caller, req *casv1.GetBalancesRequest) *casv1.GetBalancesResponse {
	t.Helper()
	resp, err := ac.GetBalances(who.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func byOwner(owner string) *casv1.GetBalancesRequest {
	return &casv1.GetBalancesRequest{Selector: &casv1.GetBalancesRequest_OwnerRef{OwnerRef: owner}}
}

func byConnection(id string) *casv1.GetBalancesRequest {
	return &casv1.GetBalancesRequest{Selector: &casv1.GetBalancesRequest_ConnectionId{ConnectionId: id}}
}

// balanceLines renders balances as connection/account type/native asset/free/locked.
func balanceLines(resp *casv1.GetBalancesResponse) []string {
	var out []string
	for _, b := range resp.GetBalances() {
		out = append(out, fmt.Sprintf("%s/%s/%s/%s/%s", b.GetConnectionId()[:8], b.GetAccountType(), b.GetNativeAsset(), b.GetFree(), b.GetLocked()))
	}
	return out
}

// ledger reads every page of a request and returns the entries.
func ledger(t *testing.T, ac casv1.AccountDataServiceClient, who caller, req *casv1.ListLedgerEntriesRequest) []*casv1.LedgerEntry {
	t.Helper()
	var out []*casv1.LedgerEntry
	for range 1000 {
		resp, err := ac.ListLedgerEntries(who.ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, resp.GetEntries()...)
		if !resp.GetHasMore() {
			return out
		}
		req.AfterSeq = resp.GetLastSeq()
	}
	t.Fatal("no last page")
	return nil
}

func keysOf(entries []*casv1.LedgerEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.GetConnectionId()[:8]+"/"+e.GetExternalId()+"/"+e.GetLeg().String())
	}
	sort.Strings(out)
	return out
}

// insertEntries inserts entries g = from..to of a connection through the owner pool.
func (e *env) insertEntries(t *testing.T, tenantID uuid.UUID, connectionID string, from, to int, amount string) {
	t.Helper()
	e.exec(t, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw)
		SELECT $1, $2, 'ops', 'x-' || g, 'SINGLE', 'ops:x-' || g, 'DEPOSIT', 'IN', 'BTC', 'BTC', $5::numeric,
		       '2026-10-01T00:00:00Z'::timestamptz + g * interval '1 second', '{}'
		FROM generate_series($3::int, $4::int) g`, tenantID, connectionID, from, to, amount)
}

// C1-T701 — Req: §2.1.3
func TestT701_BalancesByOwner(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	createExchange(t, c, a, "owner-1")
	createExchange(t, c, a, "owner-1")
	e.pass(t)

	order := listIDs(t, c, a, &casv1.ListConnectionsRequest{})
	resp := balancesBy(t, ac, a, byOwner("owner-1"))
	if len(resp.GetConnections()) != 2 {
		t.Fatalf("connections = %v", resp.GetConnections())
	}
	taken := fake.DefaultSnapshot().TakenAt
	for i, s := range resp.GetConnections() {
		if s.GetConnectionId() != order[i] || s.GetSource() != "fake" || s.GetStale() || !s.GetAsOf().AsTime().Equal(taken) {
			t.Errorf("connection %d = %v; want %s in the order of ListConnections, as of %v, not stale", i, s, order[i], taken)
		}
	}
	var want []string
	for _, id := range order {
		want = append(want, id[:8]+"/ACCOUNT_TYPE_SPOT/BTC/0.5125/0", id[:8]+"/ACCOUNT_TYPE_SPOT/USDT/898.75625/0")
	}
	if got := balanceLines(resp); !slices.Equal(got, want) {
		t.Errorf("balances = %v, want %v", got, want)
	}
	for _, b := range resp.GetBalances() {
		if b.GetAsset() != b.GetNativeAsset() {
			t.Errorf("asset %s for native %s, want the alias", b.GetAsset(), b.GetNativeAsset())
		}
	}
}

// C1-T702 — Req: §2.1.3
func TestT702_BalancesByConnection(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	id, _ := createExchange(t, c, a, "owner-1")
	createExchange(t, c, a, "owner-1")
	e.pass(t)

	resp := balancesBy(t, ac, a, byConnection(id))
	if len(resp.GetConnections()) != 1 || resp.GetConnections()[0].GetConnectionId() != id || len(resp.GetBalances()) != 2 {
		t.Errorf("response = %v, want connection %s only", resp, id)
	}
	for _, b := range resp.GetBalances() {
		if b.GetConnectionId() != id {
			t.Errorf("a balance of %s", b.GetConnectionId())
		}
	}
}

// C1-T703 — Req: §2.1.3
func TestT703_SelectorMissing(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	ac := accountClient(e.realServer(t))
	for name, req := range map[string]*casv1.GetBalancesRequest{
		"neither":                 {},
		"empty owner_ref":         byOwner(""),
		"owner_ref of 129":        byOwner(strings.Repeat("ж", 129)),
		"malformed connection_id": byConnection("connection-1"),
	} {
		_, err := ac.GetBalances(a.ctx, req)
		assertCode(t, name, err, codes.InvalidArgument)
	}
	if _, err := ac.GetBalances(a.ctx, byOwner(strings.Repeat("ж", 128))); err != nil {
		t.Errorf("owner_ref of 128: %v", err)
	}
}

// C1-T704 — Req: §2.1.3
func TestT704_NeverSynced(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	createExchange(t, c, a, "owner-1")
	createWallet(t, c, a, "owner-1")

	resp := balancesBy(t, ac, a, byOwner("owner-1"))
	if len(resp.GetConnections()) != 2 || len(resp.GetBalances()) != 0 {
		t.Fatalf("response = %v, want two connections and no balances", resp)
	}
	for _, s := range resp.GetConnections() {
		if !s.GetStale() || s.GetAsOf() != nil {
			t.Errorf("%v, want stale and no as_of", s)
		}
	}
}

// C1-T705 — Req: FR-109, §2.1.3. stale_after 30 min in sources.config of fake; the test clock decides.
func TestT705_StaleFlag(t *testing.T) {
	e := setup(t)
	e.exec(t, `UPDATE sources SET config = '{"stale_after": "30m"}' WHERE code = 'fake'`)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	id, _ := createExchange(t, c, a, "owner-1")
	e.pass(t)
	read := func() *casv1.GetBalancesResponse { return balancesBy(t, ac, a, byConnection(id)) }

	first := read()
	if first.GetConnections()[0].GetStale() {
		t.Errorf("after the sync: stale, want fresh")
	}
	e.now = testNow.Add(15 * time.Minute)
	e.fake.FailNext("balances", errors.New("fake: balance call failed"))
	e.pass(t)
	e.now = testNow.Add(31 * time.Minute)
	stale := read()
	if !stale.GetConnections()[0].GetStale() || !slices.Equal(balanceLines(stale), balanceLines(first)) {
		t.Errorf("31 min after the last success: %v; want stale with the same balances", stale)
	}

	hourOld := e.now.Add(-time.Hour)
	e.fake.SetSnapshot(connector.Snapshot{TakenAt: hourOld, Balances: fake.DefaultSnapshot().Balances})
	e.exec(t, `UPDATE sync_cursors SET next_run_at = $2 WHERE connection_id = $1 AND stream = 'balances'`, id, e.now)
	e.pass(t)
	fresh := read().GetConnections()[0]
	if fresh.GetStale() || !fresh.GetAsOf().AsTime().Equal(hourOld) {
		t.Errorf("after the success: %v; want fresh with as_of one hour old", fresh)
	}
}

// C1-T706 — Req: §3.1. A source without stale_after; internal/connector holds the parsing part.
func TestT706_StaleAfterDefault(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	id, _ := createExchange(t, c, a, "owner-1")
	e.pass(t)

	for minutes, want := range map[int]bool{29: false, 31: true} {
		e.now = testNow.Add(time.Duration(minutes) * time.Minute)
		if got := balancesBy(t, ac, a, byConnection(id)).GetConnections()[0].GetStale(); got != want {
			t.Errorf("after %d min without a sync: stale %v, want %v", minutes, got, want)
		}
	}
}

// C1-T707 — Req: §2.1.3
func TestT707_LatestSnapshotOnly(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	id, _ := createExchange(t, c, a, "owner-1")
	for i, balances := range [][]connector.Balance{
		{{AccountType: "SPOT", NativeAsset: "BTC", Free: "1", Locked: "0"}},
		{{AccountType: "SPOT", NativeAsset: "BTC", Free: "2", Locked: "0"}, {AccountType: "SPOT", NativeAsset: "USDT", Free: "3", Locked: "0"}},
		{{AccountType: "FUNDING", NativeAsset: "ETH", Free: "4", Locked: "1"}},
	} {
		e.now = testNow.Add(time.Duration(i) * 15 * time.Minute)
		e.fake.SetSnapshot(connector.Snapshot{TakenAt: e.now, Balances: balances})
		e.pass(t)
	}
	if n := count(t, e, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id); n != 3 {
		t.Fatalf("snapshots = %d, want 3", n)
	}
	resp := balancesBy(t, ac, a, byConnection(id))
	if got := balanceLines(resp); !slices.Equal(got, []string{id[:8] + "/ACCOUNT_TYPE_FUNDING/ETH/4/1"}) {
		t.Errorf("balances = %v, want those of the latest snapshot only", got)
	}
	if !resp.GetConnections()[0].GetAsOf().AsTime().Equal(e.now) {
		t.Errorf("as_of = %v, want the latest snapshot", resp.GetConnections()[0].GetAsOf().AsTime())
	}
}

// C1-T708 — Req: §2.1.1
func TestT708_NoCallToASource(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	id, _ := createExchange(t, c, a, "owner-1")
	e.pass(t)
	calls := len(e.fake.Calls())

	balancesBy(t, ac, a, byConnection(id))
	balancesBy(t, ac, a, byOwner("owner-1"))
	ledger(t, ac, a, &casv1.ListLedgerEntriesRequest{})
	ledger(t, ac, a, &casv1.ListLedgerEntriesRequest{ConnectionId: id})
	if got := len(e.fake.Calls()); got != calls {
		t.Errorf("the reads made %d calls of the fake, want none", got-calls)
	}
}

// C1-T709 — Req: §2.1.3
func TestT709_UnknownOwnerUnknownConnection(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	ac := accountClient(e.realServer(t))
	if resp := balancesBy(t, ac, a, byOwner("nobody")); len(resp.GetConnections()) != 0 || len(resp.GetBalances()) != 0 {
		t.Errorf("unknown owner: %v, want an empty response", resp)
	}
	_, err := ac.GetBalances(a.ctx, byConnection(uuid.NewString()))
	assertCode(t, "unknown connection", err, codes.NotFound)
}

// C1-T710 — Req: §2.1.4, FR-114
func TestT710_LedgerOrder(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	createExchange(t, client(conn), a, "owner-1")
	e.pass(t)
	ac := accountClient(conn)

	all, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	entries := all.GetEntries()
	if len(entries) != 6 || all.GetHasMore() || all.GetLastSeq() != entries[5].GetSeq() {
		t.Fatalf("all: %d entries, has_more %v, last_seq %d", len(entries), all.GetHasMore(), all.GetLastSeq())
	}
	if !slices.IsSortedFunc(entries, func(x, y *casv1.LedgerEntry) int { return int(x.GetSeq() - y.GetSeq()) }) {
		t.Error("entries are not in ascending seq")
	}

	page, _ := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{PageSize: 2})
	if len(page.GetEntries()) != 2 || !page.GetHasMore() || page.GetLastSeq() != entries[1].GetSeq() {
		t.Errorf("page of 2: %d entries, has_more %v, last_seq %d", len(page.GetEntries()), page.GetHasMore(), page.GetLastSeq())
	}
	after, _ := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{AfterSeq: entries[2].GetSeq()})
	if len(after.GetEntries()) != 3 || after.GetEntries()[0].GetSeq() != entries[3].GetSeq() {
		t.Errorf("after_seq = third: %d entries", len(after.GetEntries()))
	}
	beyond := entries[5].GetSeq() + 100
	empty, _ := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{AfterSeq: beyond})
	if len(empty.GetEntries()) != 0 || empty.GetLastSeq() != beyond || empty.GetHasMore() {
		t.Errorf("after the last: %v; want no entry and last_seq = after_seq", empty)
	}
	_, err = ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{AfterSeq: -1})
	assertCode(t, "negative after_seq", err, codes.InvalidArgument)
}

// C1-T711 — Req: FR-114
func TestT711_IncrementalPull(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	id, _ := createExchange(t, client(conn), a, "owner-1")
	e.insertEntries(t, a.tenantID, id, 1, 250, "1")
	ac := accountClient(conn)

	seen := map[int64]int{}
	var last int64
	for page := 0; ; page++ {
		resp, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{AfterSeq: last, PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, en := range resp.GetEntries() {
			seen[en.GetSeq()]++
		}
		last = resp.GetLastSeq()
		if page == 0 {
			e.insertEntries(t, a.tenantID, id, 251, 280, "1") // committed between two pages
		}
		if !resp.GetHasMore() {
			break
		}
	}
	if len(seen) != 280 {
		t.Errorf("the reader saw %d entries, want 280", len(seen))
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("seq %d read %d times", seq, n)
		}
	}
}

// C1-T712 — Req: §2.1.4
func TestT712_Filters(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	c, ac := client(conn), accountClient(conn)
	idA, _ := createExchange(t, c, a, "owner-1")
	idB, _ := createExchange(t, c, a, "owner-2")
	e.pass(t)
	at := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(fake.DefaultTime.Add(d)) }
	deposit := casv1.LedgerEntryType_LEDGER_ENTRY_TYPE_DEPOSIT
	trade := casv1.LedgerEntryType_LEDGER_ENTRY_TYPE_TRADE

	for name, c := range map[string]struct {
		req  *casv1.ListLedgerEntriesRequest
		want []string
	}{
		"owner_ref":   {&casv1.ListLedgerEntriesRequest{OwnerRef: "owner-1"}, keysFor(idA, "dep-1", "dep-2", "trade-1/BASE", "trade-1/QUOTE", "trade-1/FEE", "wd-1")},
		"connection":  {&casv1.ListLedgerEntriesRequest{ConnectionId: idB}, keysFor(idB, "dep-1", "dep-2", "trade-1/BASE", "trade-1/QUOTE", "trade-1/FEE", "wd-1")},
		"types":       {&casv1.ListLedgerEntriesRequest{Types: []casv1.LedgerEntryType{deposit}}, append(keysFor(idA, "dep-1", "dep-2"), keysFor(idB, "dep-1", "dep-2")...)},
		"time window": {&casv1.ListLedgerEntriesRequest{OccurredFrom: at(30 * time.Minute), OccurredTo: at(time.Hour)}, append(keysFor(idA, "dep-2"), keysFor(idB, "dep-2")...)},
		"owner and type": {&casv1.ListLedgerEntriesRequest{OwnerRef: "owner-1", Types: []casv1.LedgerEntryType{trade}},
			keysFor(idA, "trade-1/BASE", "trade-1/QUOTE")},
		"connection and from": {&casv1.ListLedgerEntriesRequest{ConnectionId: idA, OccurredFrom: at(time.Hour)},
			keysFor(idA, "trade-1/BASE", "trade-1/QUOTE", "trade-1/FEE", "wd-1")},
	} {
		got := keysOf(ledger(t, ac, a, c.req))
		want := slices.Clone(c.want)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}

	_, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{ConnectionId: uuid.NewString()})
	assertCode(t, "unknown connection", err, codes.NotFound)
	for name, types := range map[string][]casv1.LedgerEntryType{
		"unspecified type": {casv1.LedgerEntryType_LEDGER_ENTRY_TYPE_UNSPECIFIED},
		"unknown type":     {casv1.LedgerEntryType(99)},
	} {
		_, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{Types: types})
		assertCode(t, name, err, codes.InvalidArgument)
	}
	_, err = ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{OwnerRef: strings.Repeat("ж", 129)})
	assertCode(t, "owner_ref of 129", err, codes.InvalidArgument)
}

// keysFor renders the keys of keysOf: an external_id alone is the SINGLE leg.
func keysFor(id string, items ...string) []string {
	var out []string
	for _, it := range items {
		ext, leg, ok := strings.Cut(it, "/")
		if !ok {
			leg = "SINGLE"
		}
		out = append(out, id[:8]+"/"+ext+"/LEDGER_LEG_"+leg)
	}
	return out
}

// C1-T713 — Req: §2.1.1
func TestT713_PageSize(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	id, _ := createExchange(t, client(conn), a, "owner-1")
	e.insertEntries(t, a.tenantID, id, 1, 600, "1")
	ac := accountClient(conn)

	for name, c := range map[string]struct {
		req  *casv1.ListLedgerEntriesRequest
		want int
	}{
		"absent": {&casv1.ListLedgerEntriesRequest{}, 100},
		"0":      {&casv1.ListLedgerEntriesRequest{PageSize: 0}, 100},
		"500":    {&casv1.ListLedgerEntriesRequest{PageSize: 500}, 500},
		"501":    {&casv1.ListLedgerEntriesRequest{PageSize: 501}, 500},
	} {
		resp, err := ac.ListLedgerEntries(a.ctx, c.req)
		if err != nil || len(resp.GetEntries()) != c.want || !resp.GetHasMore() {
			t.Errorf("page_size %s: %d entries, %v; want %d and more", name, len(resp.GetEntries()), err, c.want)
		}
	}
	_, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{PageSize: -1})
	assertCode(t, "page_size -1", err, codes.InvalidArgument)
}

// C1-T714 — Req: §2.1.4
func TestT714_EntryFields(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	id, _ := createExchange(t, client(conn), a, "owner-1")
	e.pass(t)

	delivered := map[string]connector.Entry{}
	for _, p := range fake.DefaultHistory() {
		for _, en := range p {
			delivered[en.ExternalID+"/"+en.Leg] = en
		}
	}
	entries := ledger(t, accountClient(conn), a, &casv1.ListLedgerEntriesRequest{})
	if len(entries) != len(delivered) {
		t.Fatalf("entries = %d, want %d", len(entries), len(delivered))
	}
	for _, got := range entries {
		leg := strings.TrimPrefix(got.GetLeg().String(), "LEDGER_LEG_")
		want, ok := delivered[got.GetExternalId()+"/"+leg]
		if !ok || got.GetSeq() <= 0 || got.GetConnectionId() != id ||
			got.GetType().String() != "LEDGER_ENTRY_TYPE_"+want.Type ||
			got.GetDirection().String() != "LEDGER_DIRECTION_"+want.Direction ||
			got.GetAsset() != want.NativeAsset || got.GetNativeAsset() != want.NativeAsset ||
			got.GetAmount() != want.Amount || got.GetGroupId() != "ops:"+want.ExternalID ||
			!got.GetOccurredAt().AsTime().Equal(want.OccurredAt) {
			t.Errorf("entry %v does not match %+v", got, want)
		}
	}
	if fd := (&casv1.LedgerEntry{}).ProtoReflect().Descriptor().Fields().ByName("raw"); fd != nil {
		t.Error("LedgerEntry has a raw field")
	}
}

// C1-T715 — Req: §2.1.1, §2.4, handoff §4. The amounts are inserted through the owner pool.
func TestT715_ExactAmounts(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	id, _ := createExchange(t, client(conn), a, "owner-1")
	amounts := []string{"0.000000000000000001", "99999999999999999999.999999999999999999", "0.012500"}
	for i, amount := range amounts {
		e.insertEntries(t, a.tenantID, id, i+1, i+1, amount)
	}
	var snapshot uuid.UUID
	if err := e.owner.QueryRow(ctx, `INSERT INTO balance_snapshots (connection_id, taken_at) VALUES ($1, $2) RETURNING id`,
		id, e.now).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	e.exec(t, `INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked) VALUES
		($1, 'SPOT', 'BTC', 'BTC', 0.012500, 0),
		($1, 'SPOT', 'USDT', 'USDT', 99999999999999999999.999999999999999999, 0.000000000000000001)`, snapshot)

	ac := accountClient(conn)
	var got []string
	for _, en := range ledger(t, ac, a, &casv1.ListLedgerEntriesRequest{}) {
		got = append(got, en.GetAmount())
	}
	want := []string{"0.000000000000000001", "99999999999999999999.999999999999999999", "0.0125"}
	if !slices.Equal(got, want) {
		t.Errorf("entry amounts = %v, want %v", got, want)
	}
	balanceResp := balancesBy(t, ac, a, byConnection(id))
	balances := balanceLines(balanceResp)
	wantBalances := []string{
		id[:8] + "/ACCOUNT_TYPE_SPOT/BTC/0.0125/0",
		id[:8] + "/ACCOUNT_TYPE_SPOT/USDT/99999999999999999999.999999999999999999/0.000000000000000001",
	}
	if !slices.Equal(balances, wantBalances) {
		t.Errorf("balances = %v, want %v", balances, wantBalances)
	}
	amountsRead := slices.Clone(got)
	for _, b := range balanceResp.GetBalances() {
		amountsRead = append(amountsRead, b.GetFree(), b.GetLocked())
	}
	if all := strings.Join(amountsRead, " "); strings.ContainsAny(all, "eE+") {
		t.Errorf("an amount has an exponent: %s", all)
	}
}

// C1-T716 — Req: FR-120
func TestT716_TriggerSync(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	idA, _ := createExchange(t, c, a, "owner-1")
	idB, _ := createExchange(t, c, a, "owner-1")
	e.pass(t)
	e.now = testNow.Add(5 * time.Minute) // synced, not due
	callsOf := func(id string) []string {
		var out []string
		for _, call := range e.fake.Calls() {
			if call.Connection == id {
				out = append(out, call.Stream)
			}
		}
		return out
	}
	audits := count(t, e, `SELECT count(*) FROM audit_log`)

	before := len(callsOf(idA))
	if _, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: idA}); err != nil {
		t.Fatal(err)
	}
	var manual time.Time
	if err := e.owner.QueryRow(ctx, `SELECT last_manual_sync_at FROM connections WHERE id = $1`, idA).Scan(&manual); err != nil {
		t.Fatal(err)
	}
	if !manual.Equal(e.now) || count(t, e, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1 AND next_run_at = $2`, idA, e.now) != 2 {
		t.Errorf("last_manual_sync_at %v; want now and every stream due now", manual)
	}
	if n := count(t, e, `SELECT count(*) FROM audit_log`); n != audits {
		t.Errorf("TriggerSync wrote %d audit rows, want none", n-audits)
	}
	callsB := len(callsOf(idB))
	e.pass(t)
	if got := callsOf(idA)[before:]; !slices.Equal(got, []string{"balances", "ops"}) || len(callsOf(idB)) != callsB {
		t.Errorf("next tick: calls of A %v, of B %d; want both streams of A only", got, len(callsOf(idB))-callsB)
	}

	t.Run("a running stream is not run again", func(t *testing.T) {
		e.exec(t, `UPDATE sync_cursors SET next_run_at = $2 WHERE connection_id = $1 AND stream = 'ops'`, idB, e.now)
		entered, release := make(chan struct{}), make(chan struct{})
		e.fake.SetBefore(func(_ context.Context, call fake.Call) error {
			if call.Connection == idB && call.Kind == "page" {
				close(entered)
				<-release
			}
			return nil
		})
		done := make(chan struct{})
		go func() {
			defer close(done)
			e.pass(t)
		}()
		<-entered
		if _, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: idB}); err != nil {
			t.Errorf("TriggerSync during the run: %v", err)
		}
		close(release)
		<-done
		e.fake.SetBefore(nil)

		before := len(callsOf(idB))
		e.pass(t)
		if got := callsOf(idB)[before:]; !slices.Equal(got, []string{"balances"}) {
			t.Errorf("next tick: calls of B %v; want its other stream only", got)
		}
	})
}

// C1-T717 — Req: EC-112, FR-120
func TestT717_Cooldown(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	idA, _ := createExchange(t, c, a, "owner-1")
	idB, _ := createExchange(t, c, a, "owner-1")
	trigger := func(cl casv1.ConnectionServiceClient, at time.Duration, id string) error {
		e.now = testNow.Add(at)
		_, err := cl.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: id})
		return err
	}

	assertCode(t, "first call", trigger(c, 0, idA), codes.OK)
	assertCode(t, "second call after 30 s", trigger(c, 30*time.Second, idA), codes.ResourceExhausted)
	assertCode(t, "the other connection", trigger(c, 30*time.Second, idB), codes.OK)
	assertCode(t, "after 60 s", trigger(c, 60*time.Second, idA), codes.OK)
	restarted := client(dial(t, api.NewServer(e.deps(e.server))))
	assertCode(t, "after a restart inside the cooldown", trigger(restarted, 90*time.Second, idA), codes.ResourceExhausted)

	t.Run("of two concurrent calls one is accepted", func(t *testing.T) {
		e.now = testNow.Add(10 * time.Minute)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				_, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: idB})
				errs <- err
			}()
		}
		codesSeen := []codes.Code{status.Code(<-errs), status.Code(<-errs)}
		slices.Sort(codesSeen)
		if !slices.Equal(codesSeen, []codes.Code{codes.OK, codes.ResourceExhausted}) {
			t.Errorf("concurrent calls = %v, want one OK and one RESOURCE_EXHAUSTED", codesSeen)
		}
	})
}

// C1-T718 — Req: §2.1.1. Checked before the cooldown.
func TestT718_TriggerSyncOnAStoppedConnection(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id, _ := createExchange(t, c, a, "owner-1")
	e.exec(t, `UPDATE connections SET status = 'CREDENTIALS_INVALID', last_manual_sync_at = $2 WHERE id = $1`, id, e.now)
	_, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: id})
	assertReason(t, "stopped connection", err, codes.FailedPrecondition, "cas/CREDENTIALS_INVALID")
}

// C1-T719 — Req: §3.2. 100 000 entries and 50 connections inserted with generate_series.
func TestT719_ReadLatency(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	e.exec(t, `INSERT INTO connections (tenant_id, owner_ref, source_id, external_account, created_at)
		SELECT $1, 'owner-' || (g % 5), (SELECT id FROM sources WHERE code = 'fake'), 'account-' || g, $2
		FROM generate_series(1, 50) g`, a.tenantID, e.now)
	e.exec(t, `INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at, last_success_at)
		SELECT id, 'balances', 'INCREMENTAL', '{}', $1, $1 FROM connections`, e.now)
	e.exec(t, `INSERT INTO balance_snapshots (connection_id, taken_at, created_at)
		SELECT c.id, $1::timestamptz - s * interval '15 minutes', $1::timestamptz - s * interval '15 minutes'
		FROM connections c CROSS JOIN generate_series(0, 3) s`, e.now)
	e.exec(t, `INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
		SELECT b.id, 'SPOT', a.code, a.code, 1.5, 0 FROM balance_snapshots b CROSS JOIN (VALUES ('BTC'), ('USDT'), ('ETH')) a(code)`)
	e.exec(t, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw)
		SELECT $1, c.id, 'ops', 'x-' || g, 'SINGLE', 'ops:x-' || g, 'DEPOSIT', 'IN', 'BTC', 'BTC', 1.25,
		       '2026-10-01T00:00:00Z'::timestamptz + g * interval '1 second', '{}'
		FROM connections c CROSS JOIN generate_series(1, 2000) g`, a.tenantID)
	e.exec(t, `ANALYZE connections, sync_cursors, balance_snapshots, snapshot_balances, ledger_entries`)
	if n := count(t, e, `SELECT count(*) FROM ledger_entries`); n != 100000 {
		t.Fatalf("entries = %d, want 100 000", n)
	}
	var maxSeq int64
	if err := e.owner.QueryRow(ctx, `SELECT max(seq) FROM ledger_entries`).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	var some string
	if err := e.owner.QueryRow(ctx, `SELECT id::text FROM connections LIMIT 1`).Scan(&some); err != nil {
		t.Fatal(err)
	}
	logPlans(t, e, a, maxSeq, some)

	ac := accountClient(conn)
	measure := func(name string, call func(i int) error) {
		var d []time.Duration
		for i := range 200 {
			start := time.Now()
			if err := call(i); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			d = append(d, time.Since(start))
		}
		slices.Sort(d)
		p50, p95 := d[len(d)*50/100], d[len(d)*95/100]
		t.Logf("%s: p50 %v, p95 %v", name, p50.Round(time.Microsecond), p95.Round(time.Microsecond))
		if p95 > 100*time.Millisecond {
			t.Errorf("%s: p95 %v above 100 ms", name, p95)
		}
	}
	rng := rand.New(rand.NewPCG(1, 2))
	measure("GetBalances", func(i int) error {
		_, err := ac.GetBalances(a.ctx, byOwner(fmt.Sprintf("owner-%d", i%5)))
		return err
	})
	measure("ListLedgerEntries", func(i int) error {
		_, err := ac.ListLedgerEntries(a.ctx, &casv1.ListLedgerEntriesRequest{AfterSeq: rng.Int64N(maxSeq), PageSize: 100})
		return err
	})
}

// logPlans logs EXPLAIN ANALYZE of the two read queries as cas_server, with the SQL of the generated code.
func logPlans(t *testing.T, e *env, a caller, maxSeq int64, connectionID string) {
	t.Helper()
	src, err := os.ReadFile("../../repository/reads.sql.go")
	if err != nil {
		t.Fatal(err)
	}
	query := func(name string) string {
		m := regexp.MustCompile("(?s)const " + name + " = `-- name: [^\n]*\n(.*?)`").FindSubmatch(src)
		if m == nil {
			t.Fatalf("no query %s", name)
		}
		return string(m[1])
	}
	explain := func(title, sql string, args ...any) {
		rows, err := e.server.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY ON) "+sql, args...)
		if err != nil {
			t.Fatalf("explain %s: %v", title, err)
		}
		var plan []string
		for rows.Next() {
			var line string
			_ = rows.Scan(&line)
			plan = append(plan, line)
		}
		rows.Close()
		t.Logf("%s:\n%s", title, strings.Join(plan, "\n"))
	}
	owner := "owner-1"
	explain("ListBalanceConnections (owner_ref)", query("listBalanceConnections"), a.tenantID, &owner, nil)
	explain("ListLedgerPage (after_seq, page 100)", query("listLedgerPage"), a.tenantID, maxSeq/2, nil, nil, nil, nil, nil, int32(101))
	explain("ListLedgerPage (connection_id, page 100)", query("listLedgerPage"), a.tenantID, int64(0), nil, &connectionID, nil, nil, nil, int32(101))
}

// C1-T720 — Req: §2.1.3, §2.4
func TestT720_EmptySnapshotInARead(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	conn := e.realServer(t)
	id, _ := createExchange(t, client(conn), a, "owner-1")
	ac := accountClient(conn)
	e.pass(t) // a snapshot with balances
	if len(balancesBy(t, ac, a, byConnection(id)).GetBalances()) != 2 {
		t.Fatal("no balances after the first snapshot")
	}
	e.now = testNow.Add(15 * time.Minute)
	e.fake.SetSnapshot(connector.Snapshot{TakenAt: e.now})
	e.pass(t)

	resp := balancesBy(t, ac, a, byConnection(id))
	s := resp.GetConnections()[0]
	if s.GetAsOf() == nil || !s.GetAsOf().AsTime().Equal(e.now) || s.GetStale() || len(resp.GetBalances()) != 0 {
		t.Errorf("after an empty snapshot: %v, %d balances; want as_of, fresh and no balances", s, len(resp.GetBalances()))
	}
}

// C1-T721 — Req: §2.1.1
func TestT721_TriggerSyncWithoutStreams(t *testing.T) {
	e := setup(t)
	a := e.caller(t, "tenant-a")
	c := client(e.realServer(t))
	id := createWallet(t, c, a, "owner-1")
	calls := len(e.fake.Calls())

	if _, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: id}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := c.TriggerSync(a.ctx, &casv1.TriggerSyncRequest{ConnectionId: id})
	assertCode(t, "second call inside the cooldown", err, codes.ResourceExhausted)
	e.pass(t)
	if len(e.fake.Calls()) != calls || count(t, e, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1`, id) != 0 {
		t.Error("something ran for a connection without streams")
	}
}
