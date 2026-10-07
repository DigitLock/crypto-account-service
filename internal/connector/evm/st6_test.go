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
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
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
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
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
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
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
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
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
