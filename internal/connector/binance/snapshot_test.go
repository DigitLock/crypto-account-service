package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/httpfixture"
	"github.com/DigitLock/crypto-account-service/internal/ledger"
)

// fixture reads a committed fixture of testdata/fixtures/binance/.
func fixture(t *testing.T, name string) httpfixture.File {
	t.Helper()
	f, err := httpfixture.Read(filepath.Join(fixturesDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// ldoAlias is the alias row of the real asset LDO, as migration 000010 has it.
var ldoAlias = connector.Alias{NativeAsset: "LDO", Asset: "LDO"}

// snapshotOf serves f and runs FetchSnapshot with the aliases given, on a recording limiter.
func snapshotOf(t *testing.T, f httpfixture.File, aliases ...connector.Alias) (connector.Snapshot, error, *harness, *httpfixture.Server) {
	t.Helper()
	srv := httpfixture.Serve(t, f, weights)
	h := newHarness(t, srv.URL(), nil)
	src := source(`{"base_url": "` + srv.URL() + `"}`)
	src.Aliases = aliases
	snap, err := h.c.FetchSnapshot(context.Background(), connector.Connection{
		ID: "0b0b0b0b-0000-4000-8000-000000000001", Source: src, Account: "100000001", Key: h.s.key, Limiter: h.lim,
	})
	srv.AssertAllServed()
	return snap, err, h, srv
}

// rows renders the balances of one account type as "ASSET free/locked", in the order of the snapshot.
func rows(snap connector.Snapshot, accountType string) []string {
	var out []string
	for _, b := range snap.Balances {
		if b.AccountType == accountType {
			out = append(out, b.NativeAsset+" "+b.Free+"/"+b.Locked)
		}
	}
	return out
}

func assertRows(t *testing.T, snap connector.Snapshot, accountType string, want ...string) {
	t.Helper()
	if got := rows(snap, accountType); !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", accountType, got, want)
	}
}

// replaceCall returns f with the answer of the call at index i replaced by answer.
func replaceCall(f httpfixture.File, i int, answer httpfixture.Call) httpfixture.File {
	f.Calls = slices.Clone(f.Calls)
	f.Calls[i] = answer
	return f
}

// setBody returns a call with a body built from v.
func setBody(t *testing.T, c httpfixture.Call, v any) httpfixture.Call {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	c.Body = b
	return c
}

// spotWith returns the account call of snapshot_full.json with these balances: asset, free, locked.
func spotWith(t *testing.T, f httpfixture.File, balances ...[3]string) httpfixture.Call {
	t.Helper()
	var list []map[string]string
	for _, b := range balances {
		list = append(list, map[string]string{"asset": b[0], "free": b[1], "locked": b[2]})
	}
	return setBody(t, f.Calls[1], map[string]any{"uid": 100000001, "accountType": "SPOT", "balances": list})
}

// flexibleWith returns a flexible page of these assets and amounts, total as given.
func flexibleWith(t *testing.T, c httpfixture.Call, total int, positions ...[2]string) httpfixture.Call {
	t.Helper()
	list := []map[string]string{}
	for _, p := range positions {
		list = append(list, map[string]string{"asset": p[0], "totalAmount": p[1]})
	}
	return setBody(t, c, map[string]any{"rows": list, "total": total})
}

// X1-T401 — Req: UC-202 step 1; X1 D-9, X1 D-35. account with omitZeroBalances=true; one SPOT row per balance with
// free or locked above 0; free and locked as given.
func TestT401_Spot(t *testing.T) {
	snap, err, _, srv := snapshotOf(t, fixture(t, "snapshot_full.json"), ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	if q := srv.Received()[1].RawQuery; !strings.HasPrefix(q, "omitZeroBalances=true&recvWindow=5000&timestamp=") {
		t.Errorf("account query %s", q)
	}
	assertRows(t, snap, AccountSpot, "BTC 0.001/0", "LDO 3.5/0", "TSTX 1/0", "USDT 20/1.5")
}

// X1-T402 — Req: UC-202 step 2. FUNDING: free as given, locked = locked + freeze + withdrawing; the parameters of
// the POST in the query; a zero row dropped.
func TestT402_Funding(t *testing.T) {
	snap, err, _, srv := snapshotOf(t, fixture(t, "snapshot_full.json"), ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	if r := srv.Received()[2]; r.Method != "POST" || !strings.HasPrefix(r.RawQuery, "recvWindow=5000&timestamp=") {
		t.Errorf("funding request %s %s", r.Method, r.RawQuery)
	}
	assertRows(t, snap, AccountFunding, "USDT 1.5/1")
}

// X1-T403 — Req: UC-202 step 3. Two flexible positions of USDT: EARN_FLEXIBLE, totalAmount in free summed, locked 0.
func TestT403_EarnFlexible(t *testing.T) {
	snap, err, _, _ := snapshotOf(t, fixture(t, "snapshot_full.json"), ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, snap, AccountEarnFlexible, "USDT 12.5/0")
}

// X1-T404 — Req: UC-202 step 4. Three locked positions, two of AXS: EARN_LOCKED, amount in locked summed per asset:
// two rows.
func TestT404_EarnLocked(t *testing.T) {
	snap, err, _, _ := snapshotOf(t, fixture(t, "snapshot_full.json"), ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, snap, AccountEarnLocked, "AXS 0/129.99202928", "DOT 0/5")
}

// reservedBy sums the reservations of a recording limiter per budget, besides the time call.
func reservedBy(h *harness) map[string]int {
	out := map[string]int{}
	for _, e := range h.lim.log() {
		var budget string
		var cost int
		if _, err := fmt.Sscanf(e, "reserve %s %d", &budget, &cost); err == nil {
			out[budget] += cost
		}
	}
	return out
}

// X1-T405 — Req: UC-202 steps 3–4; §3.2 Performance; X1 D-29. 150 flexible positions in pages of 100 with total:
// pages current 1 and 2, 150 reserved per page; the whole snapshot reserves 20 in api, 1 in the funding budget and
// 150 per Earn page besides the time call. snapshot_paged.json: a page of 100 and one of 1. The paging stops at total
// or at a short page; more than 100 pages fail the snapshot.
func TestT405_Paging(t *testing.T) {
	f := fixture(t, "snapshot_paged.json")
	positions := func(n int, asset string) [][2]string {
		out := make([][2]string, n)
		for i := range out {
			out[i] = [2]string{asset, "1"}
		}
		return out
	}
	f150 := replaceCall(f, 3, flexibleWith(t, f.Calls[3], 150, positions(100, "USDT")...))
	f150 = replaceCall(f150, 4, flexibleWith(t, f.Calls[4], 150, positions(50, "BTC")...))
	snap, err, h, srv := snapshotOf(t, f150)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, snap, AccountEarnFlexible, "BTC 50/0", "USDT 100/0")
	for i, current := range []string{"current=1", "current=2"} {
		if q := srv.Received()[3+i].RawQuery; !strings.HasPrefix(q, "size=100&"+current+"&recvWindow=5000") {
			t.Errorf("page %d query %s", i+1, q)
		}
	}
	want := map[string]int{
		BudgetAPI: 1 + 20, SAPIBudget(endpointFunding.path): 1, SAPIBudget(endpointFlexible.path): 300,
		SAPIBudget(endpointLocked.path): 150,
	}
	if got := reservedBy(h); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("reserved = %v, want %v", got, want)
	}

	t.Run("snapshot_paged.json", func(t *testing.T) {
		snap, err, _, _ := snapshotOf(t, f)
		if err != nil {
			t.Fatal(err)
		}
		assertRows(t, snap, AccountEarnFlexible, "BTC 0.5/0", "USDT 100/0")
		assertRows(t, snap, AccountEarnLocked, "DOT 0/5")
	})

	t.Run("a short page ends the paging before total", func(t *testing.T) {
		short := replaceCall(f, 3, flexibleWith(t, f.Calls[3], 500, positions(99, "USDT")...))
		short.Calls = slices.Delete(slices.Clone(short.Calls), 4, 5)
		if _, err, _, _ := snapshotOf(t, short); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("more than 100 pages fail the snapshot", func(t *testing.T) {
		g := f
		g.Calls = slices.Clone(f.Calls[:3])
		for page := 1; page <= maxEarnPages; page++ {
			c := f.Calls[3]
			c.Query = fmt.Sprintf("size=100&current=%d&recvWindow=5000", page)
			g.Calls = append(g.Calls, flexibleWith(t, c, 1_000_000, positions(100, "USDT")...))
		}
		snap, err, _, _ := snapshotOf(t, g)
		if err == nil || !strings.Contains(err.Error(), "more than 100 pages") || len(snap.Balances) != 0 {
			t.Errorf("error %v, %d balances; want the paging guard and no balance", err, len(snap.Balances))
		}
	})
}

// X1-T406 — Req: EC-209, FR-205. Spot LDUSDT, a flexible position of USDT, no alias LDUSDT: no SPOT row of LDUSDT;
// USDT of Earn counted once, in EARN_FLEXIBLE.
func TestT406_WrapperAsset(t *testing.T) {
	snap, err, _, _ := snapshotOf(t, fixture(t, "snapshot_full.json"), ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range snap.Balances {
		if b.NativeAsset == "LDUSDT" {
			t.Errorf("a row of the wrapper: %+v", b)
		}
	}
	assertRows(t, snap, AccountEarnFlexible, "USDT 12.5/0")
}

// X1-T407 — Req: EC-216. Spot LDO with its alias: kept in SPOT whether a flexible position of O exists or not.
// Without the alias and with a position of O it is a wrapper.
func TestT407_RealAssetStartingWithLD(t *testing.T) {
	f := fixture(t, "snapshot_full.json")
	withO := replaceCall(f, 3, flexibleWith(t, f.Calls[3], 2, [2]string{"USDT", "10"}, [2]string{"O", "1"}))
	for name, c := range map[string]struct {
		f       httpfixture.File
		aliases []connector.Alias
		kept    bool
	}{
		"alias, no position of O":   {f, []connector.Alias{ldoAlias}, true},
		"alias, a position of O":    {withO, []connector.Alias{ldoAlias}, true},
		"no alias, no position":     {f, nil, true},
		"no alias, a position of O": {withO, nil, false},
	} {
		snap, err, _, _ := snapshotOf(t, c.f, c.aliases...)
		if err != nil {
			t.Fatal(err)
		}
		if kept := slices.Contains(rows(snap, AccountSpot), "LDO 3.5/0"); kept != c.kept {
			t.Errorf("%s: LDO kept %v, want %v", name, kept, c.kept)
		}
	}
}

// X1-T408 — Req: UC-202 step 5; EC-208. Spot LDXYZ, no flexible position of XYZ, no alias: kept under its native
// code. The metric unmapped_assets_total is counted by the engine: internal/engine.
func TestT408_LDCodeWithoutPosition(t *testing.T) {
	f := fixture(t, "snapshot_full.json")
	f = replaceCall(f, 1, spotWith(t, f, [3]string{"LDXYZ", "2", "0"}))
	snap, err, _, _ := snapshotOf(t, f)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, snap, AccountSpot, "LDXYZ 2/0")
}

// X1-T409 — Req: EC-208; Core EC-109. A spot asset without an alias (TSTX) is kept under its native code; the
// metric is counted by the engine: internal/engine.
func TestT409_UnknownAsset(t *testing.T) {
	snap, err, _, _ := snapshotOf(t, fixture(t, "snapshot_full.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rows(snap, AccountSpot), "TSTX 1/0") {
		t.Errorf("SPOT = %v, want TSTX", rows(snap, AccountSpot))
	}
}

// X1-T410, the connector part — Req: FR-204, EC-215. Each of the four sources fails in turn, with an error answer and
// with 429: FetchSnapshot fails with that error and returns no balance. The committed files of a failing source and
// variants of snapshot_full.json.
func TestT410_OneSourceFails(t *testing.T) {
	full := fixture(t, "snapshot_full.json")
	failAt := func(i, status int, retryAfter string) httpfixture.File {
		c := full.Calls[i]
		c.HTTPStatus, c.Body, c.Headers = status, nil, nil
		if retryAfter != "" {
			c.Headers = map[string]string{"Retry-After": retryAfter}
		}
		f := replaceCall(full, i, c)
		f.Calls = f.Calls[:i+1]
		return f
	}
	cases := map[string]httpfixture.File{
		"snapshot_funding_fails.json":  fixture(t, "snapshot_funding_fails.json"),
		"snapshot_flexible_fails.json": fixture(t, "snapshot_flexible_fails.json"),
		"snapshot_locked_fails.json":   fixture(t, "snapshot_locked_fails.json"),
	}
	for i, source := range []string{"spot", "funding", "flexible", "locked"} {
		cases[source+" 500"] = failAt(1+i, 500, "")
		cases[source+" 429"] = failAt(1+i, 429, "30")
	}
	for name, f := range cases {
		snap, err, _, _ := snapshotOf(t, f, ldoAlias)
		var rl *connector.RateLimitError
		if err == nil || len(snap.Balances) != 0 || !snap.TakenAt.IsZero() {
			t.Errorf("%s: %d balances, error %v; want an error and nothing", name, len(snap.Balances), err)
		}
		if strings.HasSuffix(name, "429") && (!errors.As(err, &rl) || rl.Pause != 30*time.Second) {
			t.Errorf("%s: %v, want RateLimitError of 30 s", name, err)
		}
	}
}

// X1-T412 — Req: UC-202; X1 D-9. The time of the snapshot is the server time at the first request of the run: the
// local clock with the offset of the base URL.
func TestT412_TimeOfTheSnapshot(t *testing.T) {
	f := fixture(t, "snapshot_full.json")
	f = replaceCall(f, 0, timeCall(t0.UnixMilli()+1500)) // Binance 1500 ms ahead of the local clock t0
	snap, err, _, _ := snapshotOf(t, f, ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(1500 * time.Millisecond).UTC(); !snap.TakenAt.Equal(want) {
		t.Errorf("taken at %v, want %v", snap.TakenAt, want)
	}
}

// X1-T413 — Req: Core EC-117. A balance abc, a negative one, one with 19 decimals: the snapshot is refused as a
// whole: abc and the negative by the connector, 19 decimals by the ledger writer (ledger.ValidateSnapshot). No error
// quotes the amount.
func TestT413_MalformedAmount(t *testing.T) {
	f := fixture(t, "snapshot_full.json")
	for _, c := range []struct {
		amount   string
		byLedger bool
	}{{"abc", false}, {"-1.5", false}, {"0.1234567890123456789", true}} {
		g := replaceCall(f, 1, spotWith(t, f, [3]string{"BTC", "1", "0"}, [3]string{"USDT", c.amount, "0"}))
		snap, err, _, _ := snapshotOf(t, g, ldoAlias)
		if c.byLedger {
			if err != nil {
				t.Fatalf("%s: %v; the ledger writer decides", c.amount, err)
			}
			err = ledger.ValidateSnapshot(snap)
		} else if len(snap.Balances) != 0 {
			t.Errorf("%s: %d balances returned", c.amount, len(snap.Balances))
		}
		if err == nil || strings.Contains(err.Error(), c.amount) || strings.Contains(err.Error(), strings.TrimPrefix(c.amount, "-")) {
			t.Errorf("%s: error %v; want a refusal that does not quote the amount", c.amount, err)
		}
	}
}

// X1-T401 … X1-T404 — Req: X1 D-35. A row whose free and locked are both 0 is not returned, in every account type;
// sums of decimals are exact and normalised.
func TestT404_ZeroRowsAndSums(t *testing.T) {
	f := fixture(t, "snapshot_full.json")
	f = replaceCall(f, 3, flexibleWith(t, f.Calls[3], 3, [2]string{"USDT", "0.1"}, [2]string{"USDT", "0.2"}, [2]string{"ZERO", "0.000"}))
	snap, err, _, _ := snapshotOf(t, f, ldoAlias)
	if err != nil {
		t.Fatal(err)
	}
	assertRows(t, snap, AccountEarnFlexible, "USDT 0.3/0")
	for _, b := range snap.Balances {
		if b.Free == "0" && b.Locked == "0" {
			t.Errorf("a zero row: %+v", b)
		}
	}
	if err := ledger.ValidateSnapshot(snap); err != nil {
		t.Errorf("the ledger refuses the snapshot: %v", err)
	}
}

// X1-T107, the snapshot part — Req: SRS — Binance §3.2 Security. A failing snapshot: the error and the log hold no
// key, secret, signature, uid or amount.
func TestT107_SnapshotErrorsHoldNoSecret(t *testing.T) {
	for _, name := range []string{"snapshot_funding_fails.json", "snapshot_flexible_fails.json", "snapshot_locked_fails.json"} {
		_, err, h, srv := snapshotOf(t, fixture(t, name), ldoAlias)
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		forbidden := []string{h.s.key.APIKey.Value(), h.s.key.APISecret.Value(), "signature=", "100000001", "12.5", "129.99"}
		for _, r := range srv.Received() {
			if m := signatureParam.FindStringSubmatch(r.RawQuery); m != nil {
				forbidden = append(forbidden, m[1])
			}
		}
		for _, s := range forbidden {
			if strings.Contains(err.Error(), s) || strings.Contains(h.log.String(), s) {
				t.Errorf("%s: %q in the error or the log", name, s)
			}
		}
	}
}
