package evm

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/ledger"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
)

// incremental reads one logs page of a stream already in mode INCREMENTAL.
func (r *rig) incremental(conn connector.Connection, cursor string) (connector.Page, error) {
	return r.c.FetchPage(context.Background(), conn, FamilyLogs, connector.ModeIncremental, json.RawMessage(cursor))
}

// clockAt sets the clock of the connector and returns a function that moves it.
func (r *rig) clockAt(t time.Time) func(time.Duration) {
	now := t
	r.c.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

// S3-T501 — Req: UC-304 steps 1–2; Core Connector contract; S3 D-3. A checkpoint only with a page of a stream already
// INCREMENTAL that ends at F, once per completeness_interval: block number, hash, time, one balance per tracked
// token at F, pinned by the hash of the range-end header. A BACKFILL page at F, a page below F and a second page
// inside the interval carry none.
func TestT501_WhenACheckpointIsReturned(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).logsChecks(t).
		// 1. W, INCREMENTAL, ends at F = 30: checkpoint.
		head(t, 40).header(t, hexutil.EncodeUint64(20), 20, 1).page(t, 21, 30).
		balance(t, fxToken, blockHash(30, 1), big.NewInt(12_500_000)).
		// 2. W again inside the interval, F = 35: none.
		head(t, 45).header(t, hexutil.EncodeUint64(30), 30, 1).page(t, 31, 35).
		// 3. B, BACKFILL from the floor, ends at F = 35: none.
		head(t, 45).page(t, 0, 35).
		// 4. C, INCREMENTAL, log_range_max 5: blocks 21 to 25, below F = 35: none.
		head(t, 45).header(t, hexutil.EncodeUint64(20), 20, 1).page(t, 21, 25).
		// 5. W after the interval, F = 40: checkpoint.
		head(t, 50).header(t, hexutil.EncodeUint64(35), 35, 1).page(t, 36, 40).
		balance(t, fxToken, blockHash(40, 1), big.NewInt(0)).
		file())
	r := newRig(srv.URL(), "")
	advance := r.clockAt(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	src := fxSource(`"completeness_interval": "1h"`)

	page, err := r.incremental(r.conn("w", src), cursorJSON(21, blockHash(20, 1)))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	cp := page.Checkpoint
	if cp == nil || cp.BlockNumber == nil || *cp.BlockNumber != 30 || cp.BlockHash != blockHash(30, 1).Hex() ||
		!cp.TakenAt.Equal(blockTime(30)) || len(cp.Balances) != 1 ||
		cp.Balances[0] != (connector.Balance{AccountType: "WALLET", NativeAsset: fxToken.Hex(), Free: "12.5", Locked: "0"}) {
		t.Fatalf("run 1: checkpoint %+v, want block 30 with 12.5", cp)
	}
	if err := ledger.ValidatePage(page); err != nil {
		t.Errorf("run 1: the ledger writer refuses the page: %v", err)
	}

	advance(30 * time.Minute)
	if page, err := r.incremental(r.conn("w", src), cursorJSON(31, blockHash(30, 1))); err != nil || page.Checkpoint != nil {
		t.Errorf("run 2 inside the interval: %v, checkpoint %+v; want none", err, page.Checkpoint)
	}
	if page, err := r.logs(r.conn("b", src), `{}`); err != nil || page.Checkpoint != nil || page.Mode != connector.ModeIncremental {
		t.Errorf("run 3 BACKFILL at F: %v, checkpoint %+v; want none", err, page.Checkpoint)
	}
	srcC := fxSource(`"completeness_interval": "1h", "log_range_max": 5`)
	if page, err := r.incremental(r.conn("c", srcC), cursorJSON(21, blockHash(20, 1))); err != nil || page.Checkpoint != nil || !page.More {
		t.Errorf("run 4 below F: %v, checkpoint %+v; want none", err, page.Checkpoint)
	}

	advance(30 * time.Minute)
	page, err = r.incremental(r.conn("w", src), cursorJSON(36, blockHash(35, 1)))
	if err != nil || page.Checkpoint == nil || *page.Checkpoint.BlockNumber != 40 || page.Checkpoint.Balances[0].Free != "0" {
		t.Errorf("run 5 after the interval: %v, checkpoint %+v; want block 40 with 0", err, page.Checkpoint)
	}
	if r.lim.reserved[EndpointPrimary] != srv.Served() {
		t.Errorf("%d reservations for %d requests", r.lim.reserved[EndpointPrimary], srv.Served())
	}
	srv.AssertAllServed()
}

// S3-T504 — Req: EC-317. The node no longer serves the state of F: the page comes with its entries and cursor, no
// checkpoint; evm_completeness_skipped_total +1 and a WARN line with the error, without the URL. The endpoint does
// not change. A rate limit of the same read fails the run.
func TestT504_StateNotServed(t *testing.T) {
	deposit := transferLog(fxOther, fxAccount, usdcUnits(10), 25, 1, 0)
	at30 := balanceParams(t, fxToken, fxAccount, blockHash(30, 1))
	srv := rpcfixture.Serve(t, (&script{}).logsChecks(t).
		head(t, 40).header(t, hexutil.EncodeUint64(20), 20, 1).
		logs(t, 21, 30, nil, []any{deposit}, nil).header(t, hexutil.EncodeUint64(30), 30, 1).blockByHash(t, 25).
		rpcError(t, "eth_call", at30, -32000, "missing trie node 6a1f… (path ) state 0x… is not available").
		file())
	r := newRig(srv.URL(), "http://127.0.0.1:1")
	r.clockAt(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	page, err := r.incremental(r.conn("w", fxSource("")), cursorJSON(21, blockHash(20, 1)))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if page.Checkpoint != nil || entryKinds(page) != "DEPOSIT IN 10" || *cursorOf(t, page).NextBlock != 31 {
		t.Errorf("page %+v; want the deposit, the cursor past 30 and no checkpoint", page)
	}
	if err := ledger.ValidatePage(page); err != nil {
		t.Errorf("the ledger writer refuses the page: %v", err)
	}
	if r.m.complete["anvil"] != 1 {
		t.Errorf("evm_completeness_skipped_total = %d, want 1", r.m.complete["anvil"])
	}
	log := r.log.String()
	if !strings.Contains(log, "completeness check skipped") || !strings.Contains(log, "missing trie node") ||
		strings.Contains(log, srv.URL()) || !strings.Contains(log, `"level":"WARN"`) {
		t.Errorf("no WARN line with the error and without the URL:\n%s", log)
	}
	if r.c.network("anvil").onFallback {
		t.Error("a skipped check moved the source to the fallback")
	}
	srv.AssertAllServed()

	t.Run("rate limit fails the run", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).logsChecks(t).
			head(t, 40).header(t, hexutil.EncodeUint64(20), 20, 1).page(t, 21, 30).
			status(t, "eth_call", at30, http.StatusTooManyRequests).file())
		r := newRig(srv.URL(), "")
		_, err := r.incremental(r.conn("w", fxSource("")), cursorJSON(21, blockHash(20, 1)))
		var limit *connector.RateLimitError
		if !errors.As(err, &limit) || r.m.complete["anvil"] != 0 {
			t.Errorf("run: %v, skipped %d; want a RateLimitError and no skip", err, r.m.complete["anvil"])
		}
		srv.AssertAllServed()
	})
}

// S3-T510 — Req: EC-319, EC-317; S3 D-39. balanceOf at F answers 10^20 USDC, 21 integer digits: the page comes with
// its entry and cursor and no checkpoint, the ledger writer accepts it; the counter grows by 1, a WARN line names
// the connection and the block, without the URL. The next page after completeness_interval, with a balance in range,
// carries a checkpoint.
func TestT510_OversizedCheckpointBalance(t *testing.T) {
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(26), nil) // 10^26 base units, 6 decimals
	deposit := transferLog(fxOther, fxAccount, usdcUnits(10), 25, 1, 0)
	srv := rpcfixture.Serve(t, (&script{}).logsChecks(t).
		head(t, 40).header(t, hexutil.EncodeUint64(20), 20, 1).
		logs(t, 21, 30, nil, []any{deposit}, nil).header(t, hexutil.EncodeUint64(30), 30, 1).blockByHash(t, 25).
		balance(t, fxToken, blockHash(30, 1), huge).
		head(t, 50).header(t, hexutil.EncodeUint64(30), 30, 1).page(t, 31, 40).
		balance(t, fxToken, blockHash(40, 1), usdcUnits(10)).
		file())
	r := newRig(srv.URL(), "")
	advance := r.clockAt(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))

	page, err := r.incremental(r.conn("w", fxSource("")), cursorJSON(21, blockHash(20, 1)))
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if page.Checkpoint != nil || entryKinds(page) != "DEPOSIT IN 10" || *cursorOf(t, page).NextBlock != 31 {
		t.Errorf("run 1: page %+v; want the deposit, the cursor past 30 and no checkpoint", page)
	}
	if err := ledger.ValidatePage(page); err != nil {
		t.Errorf("run 1: the ledger writer refuses the page: %v", err)
	}
	if r.m.complete["anvil"] != 1 {
		t.Errorf("evm_completeness_skipped_total = %d, want 1", r.m.complete["anvil"])
	}
	log := r.log.String()
	if strings.Count(log, "more than 20 integer digits (EC-319)") != 1 || !strings.Contains(log, `"connection_id":"w"`) ||
		!strings.Contains(log, `"block":30`) || strings.Contains(log, srv.URL()) {
		t.Errorf("no single WARN line with the connection and the block, without the URL:\n%s", log)
	}

	advance(time.Hour)
	page, err = r.incremental(r.conn("w", fxSource("")), cursorJSON(31, blockHash(30, 1)))
	if err != nil || page.Checkpoint == nil || *page.Checkpoint.BlockNumber != 40 || page.Checkpoint.Balances[0].Free != "10" {
		t.Errorf("run 2 after the interval: %v, checkpoint %+v; want block 40 with 10", err, page.Checkpoint)
	}
	srv.AssertAllServed()
}

// fxTreasuryConn is the ID of a wallet connection of the treasury address in the fixture runs of S3-T511.
const fxTreasuryConn = "0b0b0b0b-0000-4000-8000-000000000002"

// treasuryWallet is a connection of the treasury address fxTreasury on src.
func (r *rig) treasuryWallet(src connector.Source) connector.Connection {
	return connector.Connection{ID: fxTreasuryConn, Source: src, Account: fxTreasury.Hex(), Limiter: r.lim}
}

// refused checks a logs run refused by the treasury guard: the check treasury, no page.
func refused(t *testing.T, run string, page connector.Page, err error) {
	t.Helper()
	var check *StartCheckError
	if !errors.As(err, &check) || check.Check != CheckTreasury || !strings.Contains(err.Error(), fxTreasuryConn) ||
		page.Cursor != nil || len(page.Entries) != 0 {
		t.Errorf("%s: %v, page %+v; want the check treasury and no page", run, err, page)
	}
}

// S3-T511 — Req: §2.1.1 Roles; FR-307; S3 D-42, S3 D-23. A logs run of a connection whose address is treasury() of
// the controller, while it is not the treasury_connection of the source, fails as a failed check treasury before
// any log is read: no eth_getLogs, no page, evm_start_check_failed{check=treasury} 1, the endpoint kept, one WARN
// line per connection without URL. Named, its next run reads as the treasury connection, without a restart.
func TestT511_TreasuryAddressGuard(t *testing.T) {
	t.Run("treasury_connection unset, then named", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).
			logsChecks(t).                       // run 1: the checks, then treasury() for the guard; run 2 reads nothing
			constant(t, "treasury", fxTreasury). // run 3, named: the treasury check, not passed before
			head(t, 25).
			result(t, "eth_getLogs", logQuery(0, 15, fxToken, sigTransfer, addressTopic(fxTreasury)), []any{}).
			result(t, "eth_getLogs", logQuery(0, 15, fxToken, sigTransfer, nil, addressTopic(fxTreasury)),
				[]any{transferLog(fxAccount, fxTreasury, usdcUnits(20), 5, 1, 0)}).
			result(t, "eth_getLogs", logQuery(0, 15, fxController, []any{sigDebited, sigRefunded}),
				[]any{debitedLog(1, fxAccount, usdcUnits(20), 5, 1, 1)}).
			header(t, hexutil.EncodeUint64(15), 15, 1).
			blockByHash(t, 5).
			file())
		r := newRig(srv.URL(), "")
		for _, run := range []string{"run 1", "run 2"} {
			page, err := r.logs(r.treasuryWallet(fxSource("")), `{}`)
			refused(t, run, page, err)
			if failed, _ := r.m.check("anvil", CheckTreasury); !failed {
				t.Errorf("%s: evm_start_check_failed{check=treasury} is not 1", run)
			}
		}
		if r.c.network("anvil").onFallback {
			t.Error("a refusal of the guard moved the source to the fallback")
		}
		log := r.log.String()
		if n := strings.Count(log, "EVM logs run refused"); n != 1 || !strings.Contains(log, `"level":"WARN"`) ||
			!strings.Contains(log, "casctl source set-treasury anvil "+fxTreasuryConn) ||
			!strings.Contains(log, `"treasury_connection":"unset"`) || strings.Contains(log, srv.URL()) {
			t.Errorf("%d WARN lines of the guard, want 1 with the hint and without URL:\n%s", n, log)
		}

		named := fxSource(`"treasury_connection": "` + fxTreasuryConn + `", "treasury_address": "` + fxTreasury.Hex() + `"`)
		page, err := r.logs(r.treasuryWallet(named), `{}`)
		if err != nil || entryKinds(page) != "CARD_DEBIT IN 20" || *cursorOf(t, page).NextBlock != 16 {
			t.Errorf("run 3, named: %v, entries %q; want CARD_DEBIT IN 20 and the cursor at 16", err, entryKinds(page))
		}
		if failed, set := r.m.check("anvil", CheckTreasury); failed || !set {
			t.Errorf("evm_start_check_failed{check=treasury} = %v (set %v), want 0 once named", failed, set)
		}
		srv.AssertAllServed()
	})

	t.Run("treasury_connection names another connection", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, true).file()) // the treasury check reads treasury(); the guard reuses it
		r := newRig(srv.URL(), "")
		page, err := r.logs(r.treasuryWallet(fxSource(withTreasury())), `{}`)
		refused(t, "second watcher", page, err)
		if failed, _ := r.m.check("anvil", CheckTreasury); !failed {
			t.Error("evm_start_check_failed{check=treasury} is not 1")
		}
		if log := r.log.String(); strings.Count(log, "EVM logs run refused") != 1 ||
			!strings.Contains(log, `"treasury_connection":"`+fxTreasuryConnection+`"`) {
			t.Errorf("WARN line of the guard:\n%s", log)
		}
		srv.AssertAllServed()
	})

	t.Run("balances not affected", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).latest(t, 30, 1).
			result(t, "eth_call", balanceParams(t, fxToken, fxTreasury, blockHash(30, 1)),
				hexutil.Bytes(common.LeftPadBytes(usdcUnits(5).Bytes(), 32))).file())
		r := newRig(srv.URL(), "")
		if _, err := r.FetchSnapshotOf(r.treasuryWallet(fxSource(""))); err != nil {
			t.Errorf("balances of the unnamed treasury connection: %v", err)
		}
		srv.AssertAllServed()
	})
}
