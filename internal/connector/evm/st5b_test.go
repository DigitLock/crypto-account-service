package evm

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/ledger"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
)

// fxLog is a log of the fixture runs in block n of fork 1.
func fxLog(address common.Address, block uint64, tx byte, index uint64, amount *big.Int, topics ...common.Hash) map[string]any {
	return map[string]any{
		"address": address, "topics": topics, "data": hexutil.Bytes(common.LeftPadBytes(amount.Bytes(), 32)),
		"blockNumber": hexutil.Uint64(block), "blockHash": blockHash(block, 1), "transactionHash": common.Hash{0: tx, 31: tx},
		"transactionIndex": "0x0", "logIndex": hexutil.Uint64(index), "removed": false,
	}
}

func addressTopic(a common.Address) common.Hash { return common.BytesToHash(a.Bytes()) }

func transferLog(from, to common.Address, amount *big.Int, block uint64, tx byte, index uint64) map[string]any {
	return fxLog(fxToken, block, tx, index, amount, sigTransfer, addressTopic(from), addressTopic(to))
}

func debitedLog(auth byte, user common.Address, amount *big.Int, block uint64, tx byte, index uint64) map[string]any {
	return fxLog(fxController, block, tx, index, amount, sigDebited, common.Hash{31: auth}, addressTopic(user))
}

// usdcUnits is n USDC in base units of 6 decimals.
func usdcUnits(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)) }

// oversized is 10^27 base units: 10^21 USDC, 22 integer digits.
var oversized = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil)

// logs answers the four filters of fxAccount for blocks from to to: transfers out, transfers in, Debited.
func (s *script) logs(t *testing.T, from, to uint64, out, in, debited []any) *script {
	qs := walletQueries(from, to, fxAccount)
	for i, result := range [][]any{out, in, debited, {}} {
		if result == nil {
			result = []any{}
		}
		s.result(t, "eth_getLogs", qs[i], result)
	}
	return s
}

// blockByHash answers the time read of block n of fork 1.
func (s *script) blockByHash(t *testing.T, n uint64) *script {
	return s.result(t, "eth_getBlockByHash", []any{blockHash(n, 1), false},
		map[string]any{"number": hexutil.Uint64(n), "hash": blockHash(n, 1), "timestamp": hexutil.Uint64(1_760_000_000 + n)})
}

// entryKinds are the entries of a page as "TYPE DIRECTION amount".
func entryKinds(page connector.Page) string {
	var out []string
	for _, e := range page.Entries {
		out = append(out, e.Type+" "+e.Direction+" "+e.Amount)
	}
	return strings.Join(out, ", ")
}

func externalIDOf(tx byte, index uint64) string {
	return common.Hash{0: tx, 31: tx}.Hex() + ":" + jsonUint(index)
}

// S3-T406 — Req: EC-310. Two Debited and two Transfer in one transaction: each event takes the nearest unpaired
// Transfer of the same token, parties and amount; two CARD_DEBIT entries, no WITHDRAWAL.
func TestT406_SeveralEventsInOneTransaction(t *testing.T) {
	cases := map[string]struct {
		out, debited []any
		pairs        map[uint64]uint64 // log index of a Debited → log index of its Transfer
		want         string
	}{
		"different amounts, interleaved": {
			out:     []any{transferLog(fxAccount, fxTreasury, usdcUnits(10), 5, 1, 1), transferLog(fxAccount, fxTreasury, usdcUnits(20), 5, 1, 3)},
			debited: []any{debitedLog(1, fxAccount, usdcUnits(10), 5, 1, 0), debitedLog(2, fxAccount, usdcUnits(20), 5, 1, 2)},
			pairs:   map[uint64]uint64{0: 1, 2: 3},
			want:    "CARD_DEBIT OUT 10, CARD_DEBIT OUT 20",
		},
		"equal amounts, transfers first": {
			out:     []any{transferLog(fxAccount, fxTreasury, usdcUnits(10), 5, 1, 0), transferLog(fxAccount, fxTreasury, usdcUnits(10), 5, 1, 1)},
			debited: []any{debitedLog(1, fxAccount, usdcUnits(10), 5, 1, 2), debitedLog(2, fxAccount, usdcUnits(10), 5, 1, 3)},
			pairs:   map[uint64]uint64{2: 1, 3: 0},
			want:    "CARD_DEBIT OUT 10, CARD_DEBIT OUT 10",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := rpcfixture.Serve(t, (&script{}).checks(t, true).head(t, 30).logs(t, 0, 20, c.out, nil, c.debited).
				header(t, hexutil.EncodeUint64(20), 20, 1).blockByHash(t, 5).file())
			r := newRig(srv.URL(), "")
			page, err := r.logs(r.conn("w", fxSource(withTreasury())), `{}`)
			if err != nil {
				t.Fatal(err)
			}
			if got := entryKinds(page); got != c.want {
				t.Errorf("entries [%s], want [%s]", got, c.want)
			}
			if r.m.unmatch["anvil"] != 0 {
				t.Errorf("%d unmatched events, want 0", r.m.unmatch["anvil"])
			}
			srv.AssertAllServed()

			// Which Transfer each event took.
			var events []*event
			for _, l := range append(append([]any{}, c.out...), c.debited...) {
				var cl chainLog
				if err := json.Unmarshal(raw(t, l), &cl); err != nil {
					t.Fatal(err)
				}
				e, err := decode(cl)
				if err != nil {
					t.Fatal(err)
				}
				events = append(events, e)
			}
			pair(events, fxToken, fxTreasury)
			for _, e := range events {
				if e.name != eventDebited {
					continue
				}
				if e.match == nil || uint64(e.match.log.Index) != c.pairs[uint64(e.log.Index)] {
					t.Errorf("Debited %d paired with %v, want Transfer %d", e.log.Index, e.match, c.pairs[uint64(e.log.Index)])
				}
			}
		})
	}
}

// S3-T407 — Req: EC-311. A Debited without a matching Transfer still gives its entry; the counter grows by 1 and a
// WARN line names the external_id.
func TestT407_EventWithoutItsTransfer(t *testing.T) {
	// The only Transfer of the transaction goes elsewhere: it is a WITHDRAWAL, not the pair.
	other := common.HexToAddress("0x00000000000000000000000000000000000A4070")
	srv := rpcfixture.Serve(t, (&script{}).checks(t, true).head(t, 30).
		logs(t, 0, 20, []any{transferLog(fxAccount, other, usdcUnits(10), 5, 1, 1)}, nil,
			[]any{debitedLog(1, fxAccount, usdcUnits(10), 5, 1, 0)}).
		header(t, hexutil.EncodeUint64(20), 20, 1).blockByHash(t, 5).file())
	r := newRig(srv.URL(), "")
	page, err := r.logs(r.conn("w", fxSource(withTreasury())), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryKinds(page); got != "CARD_DEBIT OUT 10, WITHDRAWAL OUT 10" {
		t.Errorf("entries [%s]", got)
	}
	if r.m.unmatch["anvil"] != 1 {
		t.Errorf("evm_unmatched_controller_events_total = %d, want 1", r.m.unmatch["anvil"])
	}
	log := r.log.String()
	if strings.Count(log, "controller event without its Transfer") != 1 || !strings.Contains(log, externalIDOf(1, 0)) ||
		!strings.Contains(log, `"level":"WARN"`) {
		t.Errorf("no single WARN line with the external_id:\n%s", log)
	}
	srv.AssertAllServed()
}

// S3-T409 — Req: FR-309, EC-315. A mint above 20 integer digits and a debit of the same size with its Transfer are
// not imported; the deposit of the range is; the counter grows once per skipped log, a WARN line names each; the
// cursor moves past the range and the ledger writer accepts the page.
func TestT409_AmountTooLarge(t *testing.T) {
	zero := common.Address{}
	srv := rpcfixture.Serve(t, (&script{}).checks(t, true).head(t, 30).
		logs(t, 0, 20,
			[]any{transferLog(fxAccount, fxTreasury, oversized, 7, 3, 1)},
			[]any{transferLog(zero, fxAccount, oversized, 5, 1, 0), transferLog(fxOther, fxAccount, usdcUnits(10), 6, 2, 0)},
			[]any{debitedLog(1, fxAccount, oversized, 7, 3, 0)}).
		header(t, hexutil.EncodeUint64(20), 20, 1).blockByHash(t, 5).blockByHash(t, 6).blockByHash(t, 7).file())
	r := newRig(srv.URL(), "")
	page, err := r.logs(r.conn("w", fxSource(withTreasury())), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := entryKinds(page); got != "DEPOSIT IN 10" {
		t.Errorf("entries [%s], want the deposit only", got)
	}
	if c := cursorOf(t, page); *c.NextBlock != 21 {
		t.Errorf("cursor %s, want past the range", page.Cursor)
	}
	if err := ledger.ValidatePage(page); err != nil {
		t.Errorf("the ledger writer refuses the page: %v", err)
	}
	if got := r.m.skipped["anvil/"+SkipAmountTooLarge]; got != 2 {
		t.Errorf("evm_skipped_logs_total{reason=amount_too_large} = %d, want 2: the mint and the debit", got)
	}
	log := r.log.String()
	for _, id := range []string{externalIDOf(1, 0), externalIDOf(3, 0)} {
		if !strings.Contains(log, `"external_id":"`+id+`"`) {
			t.Errorf("no WARN line names %s:\n%s", id, log)
		}
	}
	if strings.Contains(log, externalIDOf(3, 1)) || strings.Count(log, "log not imported") != 2 {
		t.Errorf("the paired Transfer was counted, or the WARN lines are not two:\n%s", log)
	}
	srv.AssertAllServed()
}

// promText is the text exposition of reg, as /metrics serves it.
func promText(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(reg, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// S3-T418 — Req: §2.5.1. The metrics of the import on the registry of server (NewPromMetrics, as cmd/server
// registers them): values after the runs of two connections of the source.
func TestT418_MetricsOfTheImport(t *testing.T) {
	head20 := blockHash(20, 1)
	srv := rpcfixture.Serve(t, (&script{}).
		// Run 1, W: F = 20; an unmatched Debited, an oversized mint, a deposit.
		checks(t, true).head(t, 30).
		logs(t, 0, 20, nil,
			[]any{transferLog(common.Address{}, fxAccount, oversized, 6, 2, 0), transferLog(fxOther, fxAccount, usdcUnits(10), 7, 3, 0)},
			[]any{debitedLog(1, fxAccount, usdcUnits(5), 5, 1, 0)}).
		header(t, hexutil.EncodeUint64(20), 20, 1).blockByHash(t, 5).blockByHash(t, 6).blockByHash(t, 7).
		// Run 2, W2 from block 21: F = 25.
		head(t, 35).header(t, hexutil.EncodeUint64(20), 20, 1).page(t, 21, 25).
		// Run 3, W2 after 3 × 5 min: nothing new; W is dropped.
		head(t, 35).header(t, hexutil.EncodeUint64(25), 25, 1).
		file())
	reg := prometheus.NewRegistry()
	r := newRig(srv.URL(), "")
	r.c = New([]uint64{31337}, r.c.endpoints, NewPromMetrics(reg), nil)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r.c.now = func() time.Time { return now }
	src := fxSource(withTreasury())

	if _, err := r.logs(r.conn("w", src), `{}`); err != nil {
		t.Fatalf("run 1: %v", err)
	}
	text := promText(t, reg)
	for _, line := range []string{
		`evm_final_block{source="anvil"} 20`,
		`evm_log_range_blocks{source="anvil"} 2000`,
		`evm_rpc_requests_total{endpoint="primary",method="eth_getLogs",result="success",source="anvil"} 4`,
		`evm_rpc_requests_total{endpoint="primary",method="eth_getBlockByHash",result="success",source="anvil"} 3`,
		`evm_indexer_lag_blocks{source="anvil"} 0`,
		`evm_unmatched_controller_events_total{source="anvil"} 1`,
		`evm_skipped_logs_total{reason="amount_too_large",source="anvil"} 1`,
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("run 1: no %s", line)
		}
	}

	if _, err := r.logs(r.conn("w2", src), cursorJSON(21, head20)); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if text := promText(t, reg); !strings.Contains(text, `evm_indexer_lag_blocks{source="anvil"} 5`+"\n") ||
		!strings.Contains(text, `evm_final_block{source="anvil"} 25`+"\n") {
		t.Errorf("run 2: lag of W at 20 below F 25 is not 5:\n%s", grepLines(text, "evm_"))
	}

	now = now.Add(15*time.Minute + time.Second)
	if _, err := r.logs(r.conn("w2", src), cursorJSON(26, blockHash(25, 1))); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if text := promText(t, reg); !strings.Contains(text, `evm_indexer_lag_blocks{source="anvil"} 0`+"\n") {
		t.Errorf("run 3: W not dropped after 3 × sync_interval.logs:\n%s", grepLines(text, "evm_indexer"))
	}
	srv.AssertAllServed()

	t.Run("BACKFILL and failed runs", func(t *testing.T) {
		r := newRig("http://127.0.0.1:1", "")
		s := &session{c: r.c, conn: connector.Connection{ID: "b"}, src: src, net: r.c.network("anvil"),
			cfg: Config{LogsInterval: 5 * time.Minute}, final: 100, finalOK: true}
		next := func(n uint64, mode connector.Mode) connector.Page {
			return connector.Page{Cursor: json.RawMessage(`{"next_block": ` + jsonUint(n) + `}`), Mode: mode}
		}
		s.indexerLag(next(91, connector.ModeIncremental), nil)
		if r.m.lag["anvil"] != 10 {
			t.Errorf("lag %d, want 10", r.m.lag["anvil"])
		}
		s.indexerLag(connector.Page{}, context.DeadlineExceeded) // a failed run keeps the value
		if r.m.lag["anvil"] != 10 {
			t.Errorf("after a failed run: lag %d, want 10", r.m.lag["anvil"])
		}
		s.indexerLag(next(50, connector.ModeBackfill), nil) // a reset to BACKFILL removes the connection
		if r.m.lag["anvil"] != 0 {
			t.Errorf("after BACKFILL: lag %d, want 0", r.m.lag["anvil"])
		}
	})
}

func grepLines(text, prefix string) string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
