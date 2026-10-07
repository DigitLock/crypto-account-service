package evm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
)

// S3-T203 — Req: FR-306. By construction: no range of step 5 ends above F; a run with blocks beyond F sends no
// eth_getLogs and no read above F (every request is served from the fixture, in order).
func TestT203_NoLogAboveFinal(t *testing.T) {
	for _, size := range []uint64{1, 2, 3, 7, 2000} {
		for final := range uint64(40) {
			for next := range final + 1 {
				from, to := logRange(next, final, size)
				if from != next || to > final || to < from || to-from+1 > size {
					t.Fatalf("logRange(%d, %d, %d) = %d…%d", next, final, size, from, to)
				}
				if want := min(next+size-1, final); to != want {
					t.Fatalf("logRange(%d, %d, %d) ends at %d, want %d", next, final, size, to, want)
				}
			}
		}
	}

	// Head 25, confirmations 10: F = 15; the cursor at 5 with the hash of block 4.
	// The fixture holds the eth_getLogs of blocks 5 to 7 only, and the header of 7.
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 25).header(t, hexutil.EncodeUint64(4), 4, 1).
		page(t, 5, 7).file())
	r := newRig(srv.URL(), "")
	page, err := r.logs(r.conn("w", fxSource(`"log_range_max": 3`)), cursorJSON(5, blockHash(4, 1)))
	if err != nil || *cursorOf(t, page).NextBlock != 8 || !page.More {
		t.Errorf("run: %v, page %+v; want the range 5 to 7 and more", err, page)
	}
	if r.m.final["anvil"] != 15 {
		t.Errorf("evm_final_block = %d, want 15", r.m.final["anvil"])
	}
	srv.AssertAllServed()
}

// S3-T205 — Req: FR-312, EC-308. The block next_block − 1 is missing: REORG_BELOW_FINAL, as T204.
func TestT205_MissingBlockBelowCursor(t *testing.T) {
	block := hexutil.EncodeUint64(19)
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
		head(t, 40).noHeader(t, block).
		head(t, 41).noHeader(t, block).file())
	r := newRig(srv.URL(), "")
	conn := r.conn("w", fxSource(""))
	for run := 1; run <= 2; run++ {
		page, err := r.logs(conn, cursorJSON(20, blockHash(19, 1)))
		var reorg *ReorgError
		if !errors.As(err, &reorg) || reorg.Block != 19 || reorg.Found != "" || page.Cursor != nil {
			t.Fatalf("run %d: %v, page %+v; want REORG_BELOW_FINAL of block 19, no block found", run, err, page)
		}
		if !strings.Contains(err.Error(), "REORG_BELOW_FINAL") || r.m.reorgs["anvil"] != run {
			t.Errorf("run %d: error %q, evm_reorg_below_final_total %d", run, err, r.m.reorgs["anvil"])
		}
	}
	if !strings.Contains(r.log.String(), blockHash(19, 1).Hex()) || !strings.Contains(r.log.String(), `"level":"ERROR"`) {
		t.Errorf("no ERROR line with the stored hash:\n%s", r.log)
	}
	// A guard hit is not an RPC error: the endpoint does not change.
	if r.c.network("anvil").onFallback {
		t.Error("a guard hit moved the source to the fallback")
	}
	srv.AssertAllServed()
}

// S3-T206 — Req: UC-303 step 3. The first run has no hash: no header of next_block − 1 is read.
func TestT206_FirstRunWithoutGuard(t *testing.T) {
	// Head 25: F = 15 ≥ backfill_floor 0, so the run goes on to step 5. The only eth_getBlockByNumber of the
	// fixture is the header of the range end, 15.
	for _, cursor := range []string{`{}`, ``, `{"next_block": 3}`} {
		from := uint64(0)
		if cursor == `{"next_block": 3}` {
			from = 3
		}
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 25).page(t, from, 15).file())
		r := newRig(srv.URL(), "")
		page, err := r.logs(r.conn("w", fxSource("")), cursor)
		if err != nil || *cursorOf(t, page).NextBlock != 16 {
			t.Errorf("cursor %q: %v, page %+v; want blocks %d to 15", cursor, err, page, from)
		}
		srv.AssertAllServed()
	}
}

// S3-T207 — Req: FR-305, FR-314, EC-305; S3 D-23. Another chain ID fails the run before any other read; the metric
// is 1 and the log line critical. That the engine counts it is shown in internal/engine.
func TestT207_StartCheckChainID(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).chainID(t, 84532).chainID(t, 1).file())
	r := newRig(srv.URL(), "")
	conn := r.conn("w", fxSource(""))
	for run, want := range []string{"chain ID 84532", "chain ID 1"} {
		_, err := r.logs(conn, `{}`)
		var check *StartCheckError
		if !errors.As(err, &check) || check.Check != CheckChainID || !strings.Contains(err.Error(), want) {
			t.Errorf("run %d: %v, want the check chain_id with %s", run, err, want)
		}
	}
	if failed, _ := r.m.check("anvil", CheckChainID); !failed {
		t.Error("evm_start_check_failed{check=chain_id} is not 1")
	}
	if !strings.Contains(r.log.String(), `"level":"ERROR","msg":"critical: EVM start check failed`) ||
		!strings.Contains(r.log.String(), `"check":"chain_id"`) {
		t.Errorf("no critical line:\n%s", r.log)
	}
	if r.c.network("anvil").onFallback {
		t.Error("a failed check moved the source to the fallback")
	}
	srv.AssertAllServed()

	t.Run("config", func(t *testing.T) {
		r := newRig(srv.URL(), "")
		_, err := r.logs(r.conn("w", connector.Source{Code: "anvil", Kind: connector.KindEVM, Config: []byte(`{"chain_id": 31337}`)}), `{}`)
		var check *StartCheckError
		if !errors.As(err, &check) || check.Check != CheckConfig {
			t.Errorf("run: %v, want the check config", err)
		}
		if failed, _ := r.m.check("anvil", CheckConfig); !failed || r.lim.reserved[EndpointPrimary] != 0 {
			t.Errorf("config check: metric %v, %d reservations", failed, r.lim.reserved[EndpointPrimary])
		}
	})
}

// S3-T208 — Req: FR-314; S3 D-23. token() of the controller has no alias row of the source.
func TestT208_StartCheckToken(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).constant(t, "token", fxOther).file())
	r := newRig(srv.URL(), "")
	_, err := r.FetchSnapshotOf(r.conn("w", fxSource("")))
	var check *StartCheckError
	if !errors.As(err, &check) || check.Check != CheckToken || !strings.Contains(err.Error(), fxOther.Hex()) {
		t.Errorf("snapshot: %v, want the check token", err)
	}
	if failed, _ := r.m.check("anvil", CheckToken); !failed {
		t.Error("evm_start_check_failed{check=token} is not 1")
	}
	if failed, set := r.m.check("anvil", CheckChainID); failed || !set {
		t.Error("evm_start_check_failed{check=chain_id} is not 0 after it passed")
	}
	srv.AssertAllServed()
}

func (r *rig) FetchSnapshotOf(conn connector.Connection) (connector.Snapshot, error) {
	return r.c.FetchSnapshot(context.Background(), conn)
}

// S3-T209 — Req: FR-314; S3 D-23, S3 D-32. (a) treasury_address ≠ treasury(); (b) treasury_connection unset: one
// WARN line per start, the stream runs.
func TestT209_StartCheckTreasury(t *testing.T) {
	t.Run("a: other treasury", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).constant(t, "token", fxToken).
			constant(t, "treasury", fxOther).file())
		r := newRig(srv.URL(), "")
		_, err := r.logs(r.conn("w", fxSource(withTreasury())), `{}`)
		var check *StartCheckError
		if !errors.As(err, &check) || check.Check != CheckTreasury || !strings.Contains(err.Error(), fxOther.Hex()) {
			t.Errorf("run: %v, want the check treasury", err)
		}
		if failed, _ := r.m.check("anvil", CheckTreasury); !failed {
			t.Error("evm_start_check_failed{check=treasury} is not 1")
		}
		srv.AssertAllServed()
	})

	t.Run("a: treasury_address unset", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, true).file())
		r := newRig(srv.URL(), "")
		_, err := r.logs(r.conn("w", fxSource(`"treasury_connection": "`+fxTreasuryConnection+`"`)), `{}`)
		var check *StartCheckError
		if !errors.As(err, &check) || check.Check != CheckTreasury || !strings.Contains(err.Error(), "unset") {
			t.Errorf("run: %v, want the check treasury", err)
		}
	})

	t.Run("b: no treasury connection", func(t *testing.T) {
		// Head 5: no block is final; both runs succeed without reading treasury().
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 5).head(t, 6).file())
		r := newRig(srv.URL(), "")
		conn := r.conn("w", fxSource(""))
		for run := range 2 {
			page, err := r.logs(conn, `{}`)
			if err != nil || page.More || string(page.Cursor) != `{}` {
				t.Errorf("run %d: %v, page %+v", run, err, page)
			}
		}
		if n := strings.Count(r.log.String(), "without a treasury connection"); n != 1 {
			t.Errorf("%d WARN lines, want 1:\n%s", n, r.log)
		}
		if _, set := r.m.check("anvil", CheckTreasury); set {
			t.Error("the skipped treasury check set the metric")
		}
		srv.AssertAllServed()
	})
}

// S3-T210 — Req: §2.1.1; S3 D-23. The checks run before each failing run; once passed, not again.
func TestT210_ChecksRepeatedUntilPassed(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).
		chainID(t, 84532).
		chainID(t, 84532).
		checks(t, true).head(t, 5). // fixed: the checks pass, then the run
		head(t, 6).                 // the next run: no check
		file())
	r := newRig(srv.URL(), "")
	conn := r.conn("w", fxSource(withTreasury()))
	for run, wantErr := range []bool{true, true, false, false} {
		_, err := r.logs(conn, `{}`)
		if (err != nil) != wantErr {
			t.Fatalf("run %d: %v", run, err)
		}
		if failed, _ := r.m.check("anvil", CheckChainID); failed != wantErr {
			t.Errorf("run %d: evm_start_check_failed{check=chain_id} = %v", run, failed)
		}
	}
	srv.AssertAllServed()
}

// S3-T211 — Req: UC-303 step 1; FR-305. The first run on the fallback reads its chain ID first; another chain ID
// fails the run as T207.
func TestT211_ChainIDOnEndpointChange(t *testing.T) {
	primary := rpcfixture.Serve(t, (&script{}).checks(t, false).status(t, "eth_blockNumber", nil, http.StatusBadGateway).file())
	fallback := rpcfixture.Serve(t, (&script{}).chainID(t, 1).file())
	r := newRig(primary.URL(), fallback.URL())
	conn := r.conn("w", fxSource(""))
	if _, err := r.logs(conn, `{}`); !isRPCFailure(err) {
		t.Fatalf("run 1: %v, want an RPC error of the primary", err)
	}
	_, err := r.logs(conn, `{}`)
	var check *StartCheckError
	if !errors.As(err, &check) || check.Check != CheckChainID || !strings.Contains(err.Error(), "EVM_RPC_FALLBACK_URL_ANVIL") {
		t.Errorf("run 2: %v, want the chain ID check of the fallback", err)
	}
	primary.AssertAllServed()
	fallback.AssertAllServed()
}

// S3-T212 — Req: §2.1.1 Common rules, EC-313; S3 D-33. One endpoint per run; the choice is per source, across
// connections.
func TestT212_FallbackAndReturn(t *testing.T) {
	primary := rpcfixture.Serve(t, (&script{}).
		checks(t, false).status(t, "eth_blockNumber", nil, http.StatusServiceUnavailable). // run 1 (W1) fails
		head(t, 5).                                                                        // run 3 (W1)
		file())
	fallback := rpcfixture.Serve(t, (&script{}).
		chainID(t, 31337).head(t, 5). // run 2 (W2): its chain ID first, then only the fallback
		file())
	r := newRig(primary.URL(), fallback.URL())
	w1, w2 := r.conn("w1", fxSource("")), r.conn("w2", fxSource(""))

	if _, err := r.logs(w1, `{}`); !isRPCFailure(err) || !strings.Contains(err.Error(), "EVM_RPC_URL_ANVIL") ||
		strings.Contains(err.Error(), primary.URL()) {
		t.Fatalf("run 1: %v", err)
	}
	if _, err := r.logs(w2, `{}`); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if !r.m.fallback["anvil"] {
		t.Error("evm_rpc_fallback_active is not 1 after run 2")
	}
	primaryCalls := r.m.requestsOf(EndpointPrimary)
	if _, err := r.logs(w1, `{}`); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if r.m.fallback["anvil"] || r.m.requestsOf(EndpointPrimary) != primaryCalls+1 {
		t.Error("run 3 did not go back to the primary")
	}
	if r.lim.reserved[EndpointFallback] != 2 || r.lim.reserved[EndpointPrimary] != 4 {
		t.Errorf("reservations %v, want 4 on the primary and 2 on the fallback", r.lim.reserved)
	}
	primary.AssertAllServed()
	fallback.AssertAllServed()

	t.Run("no fallback configured", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).status(t, "eth_blockNumber", nil, http.StatusBadGateway).
			head(t, 5).file())
		r := newRig(srv.URL(), "")
		if _, err := r.logs(r.conn("w", fxSource("")), `{}`); !isRPCFailure(err) {
			t.Fatalf("run 1: %v", err)
		}
		if _, err := r.logs(r.conn("w", fxSource("")), `{}`); err != nil {
			t.Fatalf("run 2 on the primary: %v", err)
		}
		srv.AssertAllServed()
	})
}

// S3-T213 — Req: EC-313, §3.2 Reliability; Core EC-107; S3 D-34. A rate limit fails the run with RateLimitError
// and pauses only the budget of its endpoint: for Retry-After when given, else 60 s; the next run uses the
// fallback. A call without an answer ends after 10 s.
func TestT213_RateLimit(t *testing.T) {
	cases := []struct {
		name    string
		primary func(t *testing.T) string
		pause   time.Duration
	}{
		{"HTTP 429 without Retry-After", func(t *testing.T) string {
			return rpcfixture.Serve(t, (&script{}).status(t, "eth_chainId", nil, http.StatusTooManyRequests).file()).URL()
		}, 60 * time.Second},
		{"JSON-RPC -32005", func(t *testing.T) string {
			return rpcfixture.Serve(t, (&script{}).rpcError(t, "eth_chainId", nil, -32005, "limit exceeded").file()).URL()
		}, 60 * time.Second},
		{"HTTP 429 with Retry-After", func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}, 7 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fallback := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 5).file())
			r := newRig(c.primary(t), fallback.URL())
			conn := r.conn("w", fxSource(""))
			_, err := r.logs(conn, `{}`)
			var limit *connector.RateLimitError
			if !errors.As(err, &limit) || limit.Budget != EndpointPrimary || limit.Pause != c.pause {
				t.Fatalf("run 1: %v, want RateLimitError of the primary with pause %s", err, c.pause)
			}
			if r.lim.paused[EndpointPrimary] != c.pause || r.lim.paused[EndpointFallback] != 0 {
				t.Errorf("pauses %v, want only the primary for %s", r.lim.paused, c.pause)
			}
			if r.m.requests["anvil/primary/eth_chainId/rate_limit"] != 1 {
				t.Errorf("requests %v", r.m.requests)
			}
			if _, err := r.logs(conn, `{}`); err != nil {
				t.Errorf("run 2 on the fallback: %v", err)
			}
			fallback.AssertAllServed()
		})
	}

	t.Run("a call without an answer ends after 10 s", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		r := newRig(srv.URL, "")
		start := time.Now()
		_, err := r.logs(r.conn("w", fxSource("")), `{}`)
		elapsed := time.Since(start)
		if !isRPCFailure(err) || !strings.Contains(err.Error(), "no answer within 10s") {
			t.Errorf("run: %v, want no answer within 10 s", err)
		}
		if elapsed < CallTimeout || elapsed > CallTimeout+3*time.Second {
			t.Errorf("the call ended after %s, want %s", elapsed, CallTimeout)
		}
	})

	t.Run("Retry-After forms", func(t *testing.T) {
		now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
		for v, want := range map[string]time.Duration{
			"120": 2 * time.Minute, "Wed, 07 Oct 2026 12:00:30 GMT": 30 * time.Second,
			"": 0, "0": 0, "-5": 0, "soon": 0, "Wed, 07 Oct 2026 11:00:00 GMT": 0,
		} {
			if got, _ := parseRetryAfter(v, now); got != want {
				t.Errorf("Retry-After %q = %s, want %s", v, got, want)
			}
		}
	})
}
