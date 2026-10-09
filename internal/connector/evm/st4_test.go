package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
)

// fxAccount is the wallet of the fixture runs (rig.conn).
var fxAccount = common.HexToAddress("0x00000000000000000000000000000000000000aa")

// latest is the head block of a balances run: eth_getBlockByNumber("latest", false).
func (s *script) latest(t *testing.T, n uint64, fork byte) *script {
	return s.header(t, "latest", n, fork)
}

// balanceParams are the parameters of balanceOf(account) of token, pinned to a block by its hash (EIP-1898).
func balanceParams(t *testing.T, token, account common.Address, block common.Hash) []any {
	t.Helper()
	data, err := tokenABI.Pack("balanceOf", account)
	if err != nil {
		t.Fatal(err)
	}
	return []any{map[string]any{"to": token, "data": hexutil.Bytes(data)}, map[string]any{"blockHash": block}}
}

func (s *script) balance(t *testing.T, token common.Address, block common.Hash, units *big.Int) *script {
	t.Helper()
	return s.result(t, "eth_call", balanceParams(t, token, fxAccount, block),
		hexutil.Bytes(common.LeftPadBytes(units.Bytes(), 32)))
}

// blockTime is the time of a fixture block of header.
func blockTime(n uint64) time.Time { return time.Unix(int64(1_760_000_000+n), 0).UTC() }

func decimals(n int16) *int16 { return &n }

// withAlias adds an alias row to a source.
func withAlias(src connector.Source, native common.Address, d *int16) connector.Source {
	src.Aliases = append(append([]connector.Alias(nil), src.Aliases...), connector.Alias{NativeAsset: native.Hex(), Asset: "OTHER", Decimals: d})
	return src
}

// S3-T302 — Req: FR-304, UC-302. The request order of one run: the start checks, the head block, then balanceOf of
// each tracked token pinned by the hash of that head; the snapshot time is the time of that block. as_of of
// GetBalances is shown in internal/engine.
func TestT302_OneBlockItsTime(t *testing.T) {
	head := blockHash(42, 1)
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).
		latest(t, 42, 1).
		balance(t, fxToken, head, big.NewInt(29_866_387)).
		balance(t, fxOther, head, big.NewInt(0)).
		file())
	r := newRig(srv.URL(), "")
	snap, err := r.FetchSnapshotOf(r.conn("w", withAlias(fxSource(""), fxOther, decimals(18))))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.TakenAt.Equal(blockTime(42)) || snap.TakenAt.Location() != time.UTC {
		t.Errorf("taken at %s, want the time of block 42, %s UTC", snap.TakenAt, blockTime(42))
	}
	want := []connector.Balance{
		{AccountType: "WALLET", NativeAsset: fxToken.Hex(), Free: "29.866387", Locked: "0"},
		{AccountType: "WALLET", NativeAsset: fxOther.Hex(), Free: "0", Locked: "0"},
	}
	if !equalBalances(snap.Balances, want) {
		t.Errorf("balances %+v, want %+v", snap.Balances, want)
	}
	srv.AssertAllServed()
}

func equalBalances(got, want []connector.Balance) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// S3-T303, the connector part — Req: FR-303, EC-304; S3 D-33. A read that fails fails the run and returns no
// snapshot, not a partial one: a JSON-RPC error and an HTTP 500 of balanceOf. Both are RPC errors: the next run uses
// the fallback. That the previous snapshot stays and becomes stale is shown in internal/engine.
func TestT303_NoPartialSnapshot(t *testing.T) {
	head := blockHash(30, 1)
	cases := map[string]func(s *script) *script{
		"JSON-RPC error": func(s *script) *script {
			return s.rpcError(t, "eth_call", balanceParams(t, fxOther, fxAccount, head), -32000, "header not found")
		},
		"HTTP 500": func(s *script) *script {
			return s.status(t, "eth_call", balanceParams(t, fxOther, fxAccount, head), http.StatusInternalServerError)
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			// The first token is read, the second fails.
			primary := rpcfixture.Serve(t, fail((&script{}).checks(t, false).latest(t, 30, 1).
				balance(t, fxToken, head, big.NewInt(5_000_000))).file())
			fallback := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).file())
			r := newRig(primary.URL(), fallback.URL())
			snap, err := r.FetchSnapshotOf(r.conn("w", withAlias(fxSource(""), fxOther, decimals(6))))
			if !isRPCFailure(err) || snap.Balances != nil || !snap.TakenAt.IsZero() {
				t.Fatalf("run: %v, snapshot %+v; want an RPC error and no snapshot", err, snap)
			}
			if strings.Contains(err.Error(), primary.URL()) || !strings.Contains(err.Error(), "EVM_RPC_URL_ANVIL") {
				t.Errorf("error %q: want the variable, not the URL", err)
			}
			if !r.c.network("anvil").onFallback {
				t.Error("an RPC error of balanceOf did not send the next run to the fallback")
			}
			primary.AssertAllServed()
		})
	}

	// A token without code answers "0x" or null: the run fails without a snapshot, but it is no RPC error.
	for _, answer := range []any{hexutil.Bytes{}, nil} {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).latest(t, 30, 1).
			balance(t, fxToken, head, big.NewInt(5_000_000)).
			result(t, "eth_call", balanceParams(t, fxOther, fxAccount, head), answer).file())
		r := newRig(srv.URL(), "http://127.0.0.1:1")
		snap, err := r.FetchSnapshotOf(r.conn("w", withAlias(fxSource(""), fxOther, decimals(6))))
		if err == nil || isRPCFailure(err) || snap.Balances != nil || !strings.Contains(err.Error(), fxOther.Hex()) {
			t.Errorf("token without code, answer %v: %v, snapshot %+v; want a failure naming the token", answer, err, snap)
		}
		if r.c.network("anvil").onFallback {
			t.Errorf("answer %v: a token without code moved the source to the fallback", answer)
		}
		srv.AssertAllServed()
	}
}

// S3-T304 — Req: §2.1.1 Amounts. Base units to a plain decimal by the decimals of the alias row: integer arithmetic,
// no trailing zeros, "0" for zero. No float in the path of the balances.
func TestT304_BaseUnitsToDecimals(t *testing.T) {
	maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	pow := func(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }
	cases := []struct {
		units    *big.Int
		decimals int
		want     string
	}{
		{big.NewInt(29_866_387), 6, "29.866387"},
		{big.NewInt(0), 6, "0"},
		{big.NewInt(1_000_000), 6, "1"},
		{big.NewInt(12_500_000), 6, "12.5"},
		{big.NewInt(1), 6, "0.000001"},
		{big.NewInt(100_000_000), 6, "100"},
		{big.NewInt(0), 0, "0"},
		{big.NewInt(1), 0, "1"},
		{big.NewInt(1230), 0, "1230"},
		{big.NewInt(0), 18, "0"},
		{big.NewInt(1), 18, "0.000000000000000001"},
		{pow(18), 18, "1"},
		{new(big.Int).Add(pow(18), new(big.Int).Mul(big.NewInt(5), pow(17))), 18, "1.5"},
		{new(big.Int).Sub(pow(38), big.NewInt(1)), 18, "99999999999999999999.999999999999999999"},
		{maxUint256, 18, "115792089237316195423570985008687907853269984665640564039457.584007913129639935"},
	}
	for _, c := range cases {
		if got := FormatUnits(c.units, c.decimals); got != c.want {
			t.Errorf("FormatUnits(%s, %d) = %s, want %s", c.units, c.decimals, got, c.want)
		}
	}

	src, err := os.ReadFile("balances.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"float", "big.Float", "strconv.Parse", "math.Pow"} {
		if bytes.Contains(src, []byte(s)) {
			t.Errorf("balances.go contains %q", s)
		}
	}
}

// S3-T305, the connector part — Req: UC-302 trigger; Core FR-120. The balances stream of a wallet connection: every
// 15 min, or sync_interval.balances of sources.config; first mode INCREMENTAL with an empty cursor. That the first
// run comes at creation and TriggerSync runs it is shown in internal/engine.
func TestT305_IntervalAndTrigger(t *testing.T) {
	c := newRig("http://127.0.0.1:1", "").c
	for extra, want := range map[string]time.Duration{
		``:                                     15 * time.Minute,
		`"sync_interval": {"balances": "20m"}`: 20 * time.Minute,
		`"sync_interval": {"balances": "-5m"}`: 15 * time.Minute,
		`"sync_interval": {"balances": 600}`:   15 * time.Minute,
	} {
		streams, err := c.Streams(context.Background(), fxSource(extra), connector.AccountInfo{Identity: fxAccount.Hex()})
		if err != nil || len(streams) != 2 {
			t.Fatalf("%s: streams %+v, %v", extra, streams, err)
		}
		b := streams[0]
		if b.Name != FamilyBalances || b.Family != FamilyBalances || b.Interval != want ||
			b.FirstMode != connector.ModeIncremental || string(b.FirstCursor) != `{}` {
			t.Errorf("%s: balances stream %+v, want every %s", extra, b, want)
		}
	}
}

// orderLog records the reservations of the limiter and the requests that reach an endpoint, in order.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(e string) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *orderLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// orderLimiter is a counting limiter that records each reservation in the log.
type orderLimiter struct {
	log *orderLog
}

func (l orderLimiter) Reserve(_ context.Context, budget string, cost int) error {
	if cost != 1 {
		l.log.add("reserve " + budget + " cost " + jsonUint(uint64(cost)))
		return nil
	}
	l.log.add("reserve " + budget)
	return nil
}

func (l orderLimiter) Pause(string, time.Duration) {}

func (l orderLimiter) Observe(budget string, _ int) { l.log.add("observe " + budget) }

// countingProxy records each request as "send <endpoint> <method>" before it forwards it to target.
func countingProxy(t *testing.T, log *orderLog, endpoint, target string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("not a single JSON-RPC request: %s", body)
		}
		log.add("send " + endpoint + " " + req.Method)
		resp, err := http.Post(target, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// S3-T306 — Req: Core FR-110; §3.2. Every request of a balances run reserves cost 1 in the budget of its endpoint
// before it is sent: a run on the primary that fails, then a run on the fallback.
func TestT306_EveryRequestReservesItsCost(t *testing.T) {
	head := blockHash(12, 1)
	primary := rpcfixture.Serve(t, (&script{}).checks(t, false).latest(t, 12, 1).
		status(t, "eth_call", balanceParams(t, fxToken, fxAccount, head), http.StatusBadGateway).file())
	fallback := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).latest(t, 12, 1).
		balance(t, fxToken, head, big.NewInt(1)).file())
	log := &orderLog{}
	r := newRig(countingProxy(t, log, EndpointPrimary, primary.URL()), countingProxy(t, log, EndpointFallback, fallback.URL()))
	conn := r.conn("w", fxSource(""))
	conn.Limiter = orderLimiter{log: log}

	if _, err := r.FetchSnapshotOf(conn); !isRPCFailure(err) {
		t.Fatalf("run 1: %v, want an RPC error of the primary", err)
	}
	if _, err := r.FetchSnapshotOf(conn); err != nil {
		t.Fatalf("run 2 on the fallback: %v", err)
	}
	want := []string{
		"reserve primary", "send primary eth_chainId",
		"reserve primary", "send primary eth_call", // token()
		"reserve primary", "send primary eth_getBlockByNumber",
		"reserve primary", "send primary eth_call", // balanceOf: 502
		"reserve fallback", "send fallback eth_chainId",
		"reserve fallback", "send fallback eth_getBlockByNumber",
		"reserve fallback", "send fallback eth_call", // balanceOf
	}
	got := log.list()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	primary.AssertAllServed()
	fallback.AssertAllServed()
}

// S3-T208, configuration errors of the check token — Req: FR-314, §2.1.1 Tracked tokens; S3 D-23. A controller
// without code, and an alias row without decimals or with decimals outside 0 to 18, fail the check token: the
// metric is 1 and the endpoint of the next run does not change.
func TestT208_TokenCheckConfigurationErrors(t *testing.T) {
	t.Run("controller without code", func(t *testing.T) {
		for _, answer := range []any{hexutil.Bytes{}, nil} {
			primary := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).
				result(t, "eth_call", callParams(t, "token"), answer).
				result(t, "eth_call", callParams(t, "token"), answer).file())
			fallback := rpcfixture.Serve(t, (&script{}).file())
			r := newRig(primary.URL(), fallback.URL())
			conn := r.conn("w", fxSource(""))
			for run := 1; run <= 2; run++ {
				_, err := r.FetchSnapshotOf(conn)
				var check *StartCheckError
				if !errors.As(err, &check) || check.Check != CheckToken || !strings.Contains(err.Error(), "no contract code") ||
					!strings.Contains(err.Error(), fxController.Hex()) {
					t.Errorf("answer %v, run %d: %v, want the check token", answer, run, err)
				}
			}
			if failed, _ := r.m.check("anvil", CheckToken); !failed {
				t.Error("evm_start_check_failed{check=token} is not 1")
			}
			if r.c.network("anvil").onFallback || r.m.requestsOf(EndpointFallback) != 0 {
				t.Error("a controller without code moved the source to the fallback")
			}
			primary.AssertAllServed()
		}
	})

	for name, d := range map[string]*int16{"no decimals": nil, "decimals 19": decimals(19), "decimals -1": decimals(-1)} {
		t.Run(name, func(t *testing.T) {
			// Only the chain ID is read: the decimals are checked before token().
			primary := rpcfixture.Serve(t, (&script{}).chainID(t, 31337).file())
			fallback := rpcfixture.Serve(t, (&script{}).file())
			r := newRig(primary.URL(), fallback.URL())
			_, err := r.FetchSnapshotOf(r.conn("w", withAlias(fxSource(""), fxOther, d)))
			var check *StartCheckError
			if !errors.As(err, &check) || check.Check != CheckToken || !strings.Contains(err.Error(), fxOther.Hex()) ||
				!strings.Contains(err.Error(), "decimals") {
				t.Errorf("run: %v, want the check token naming the alias row", err)
			}
			if failed, _ := r.m.check("anvil", CheckToken); !failed {
				t.Error("evm_start_check_failed{check=token} is not 1")
			}
			if r.c.network("anvil").onFallback {
				t.Error("a failed check moved the source to the fallback")
			}
			primary.AssertAllServed()
		})
	}

	t.Run("decimals 0 and 18 pass", func(t *testing.T) {
		head := blockHash(7, 1)
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).latest(t, 7, 1).
			balance(t, fxToken, head, big.NewInt(3)).balance(t, fxOther, head, big.NewInt(3)).file())
		r := newRig(srv.URL(), "")
		src := fxSource("")
		src.Aliases[0].Decimals = decimals(0)
		snap, err := r.FetchSnapshotOf(r.conn("w", withAlias(src, fxOther, decimals(18))))
		if err != nil || len(snap.Balances) != 2 || snap.Balances[0].Free != "3" || snap.Balances[1].Free != "0.000000000000000003" {
			t.Errorf("snapshot %+v, %v", snap, err)
		}
		srv.AssertAllServed()
	})
}
