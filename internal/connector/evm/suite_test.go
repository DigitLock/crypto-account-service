package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/connectortest"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// record writes the scenario fixtures of testdata/fixtures/evm/. Only make fixtures-record sets it: a normal test
// run records into a temporary directory and leaves the repository unchanged.
var record = flag.Bool("record", false, "write the scenario fixtures of testdata/fixtures/evm/ from Anvil")

// fixturesDir is testdata/fixtures/evm/ of the repository, seen from this package.
var fixturesDir = filepath.Join("..", "..", "..", "testdata", "fixtures", "evm")

// Scenarios of the committed fixtures: the wallet W and the treasury connection T of one chain.
const (
	scenarioWallet   = "wallet"
	scenarioTreasury = "treasury"
	// pagedRangeMax is log_range_max of the paged recordings: several pages over the scenario.
	pagedRangeMax = 8
	// The connection IDs of the scenarios; the treasury connection is the one of treasury_connection.
	walletConnection = "0b0b0b0b-0000-4000-8000-0000000000a1"
)

// scenarioFile is the fixture of a scenario and a case of the suite.
func scenarioFile(dir, scenario string, c connectortest.Case) string {
	if c == connectortest.RateLimited {
		return filepath.Join(dir, "error_rate_limit.json")
	}
	return filepath.Join(dir, "scenario_"+scenario+"_"+string(c)+".json")
}

// scenarioConn is the source anvil and the connection of a fixture context: the same values give the same
// requests, at the recording and at the replay. The tracked token is the context value asset: a key named token
// with an address reads as an API key to gitleaks.
func scenarioConn(t *testing.T, ctx map[string]string) connector.Connection {
	t.Helper()
	for _, k := range []string{"account", "asset", "backfill_floor", "connection_id", "controller", "log_range_max",
		"treasury", "treasury_connection"} {
		if ctx[k] == "" {
			t.Fatalf("the fixture context has no %s", k)
		}
	}
	config, err := json.Marshal(map[string]any{
		"chain_id": 31337, "finality_mode": "confirmations", "finality_confirmations": 10,
		"controller_address": ctx["controller"], "backfill_floor": json.Number(ctx["backfill_floor"]),
		"log_range_max": json.Number(ctx["log_range_max"]), "treasury_connection": ctx["treasury_connection"],
		"treasury_address": ctx["treasury"],
	})
	if err != nil {
		t.Fatal(err)
	}
	decimals := int16(testchain.TokenDecimals)
	src := connector.Source{Code: "anvil", Kind: connector.KindEVM, Enabled: true, Config: config,
		Aliases: []connector.Alias{{NativeAsset: ctx["asset"], Asset: "USDC", Decimals: &decimals}}}
	return connector.Connection{ID: ctx["connection_id"], Source: src, Account: ctx["account"]}
}

// replay is the harness of the suite on the fixtures of dir: each case is served from its file, with no network.
func replay(dir, scenario string) connectortest.Harness {
	return func(t *testing.T, c connectortest.Case) connectortest.Setup {
		t.Helper()
		f, err := rpcfixture.Read(scenarioFile(dir, scenario, c))
		if err != nil {
			t.Fatal(err)
		}
		srv := rpcfixture.Serve(t, f)
		conn := New([]uint64{31337}, map[string]Endpoints{"anvil": {Primary: vault.NewSecret(srv.URL())}}, nil, nil)
		return connectortest.Setup{
			Connector: conn, Conn: scenarioConn(t, f.Context), Stream: FamilyLogs,
			Mode: connector.ModeBackfill, Cursor: json.RawMessage(`{}`),
			Served: func() (int, int) { return srv.Served(), len(f.Calls) },
		}
	}
}

// S3-T420 — Req: FR-316, ADR-2; S3 D-14. The EVM connector passes the shared connector test suite on the committed
// fixtures, with no network access: the wallet and the treasury scenarios, and the rate limit written by hand.
func TestT420_SharedConnectorSuite(t *testing.T) {
	for _, scenario := range []string{scenarioWallet, scenarioTreasury} {
		t.Run(scenario, func(t *testing.T) { connectortest.Run(t, replay(fixturesDir, scenario)) })
	}
}

// S3-T420, the content of the committed fixtures — Req: FR-317, §2.6. The scenarios give the entries of the
// operations: mint, transfer in, transfer out, debit and refund; no URL and no key in any file.
func TestT420_CommittedFixtures(t *testing.T) {
	want := map[string]string{
		scenarioWallet:   "DEPOSIT IN 10, DEPOSIT IN 50, WITHDRAWAL OUT 3, CARD_DEBIT OUT 20, CARD_REFUND IN 5",
		scenarioTreasury: "CARD_DEBIT IN 20, CARD_REFUND OUT 5",
	}
	for scenario, kinds := range want {
		s := replay(fixturesDir, scenario)(t, connectortest.Whole)
		s.Conn.Limiter = connectortest.NewLimiter()
		page, err := s.Connector.FetchPage(context.Background(), s.Conn, s.Stream, s.Mode, s.Cursor)
		if err != nil {
			t.Fatalf("%s: %v", scenario, err)
		}
		if got := entryKinds(page); got != kinds {
			t.Errorf("%s: entries [%s], want [%s]", scenario, got, kinds)
		}
	}

	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.json"))
	if err != nil || len(files) < 7 {
		t.Fatalf("%d fixture files: %v", len(files), err)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"http://", "https://", "127.0.0.1", "localhost", "apikey", "/v2/"} {
			if strings.Contains(strings.ToLower(string(data)), s) {
				t.Errorf("%s contains %q", filepath.Base(path), s)
			}
		}
		if _, err := rpcfixture.Read(path); err != nil {
			t.Error(err)
		}
	}
}

// S3-T420, the provider errors written by hand — Req: EC-306. The committed answer of a range too large halves the
// range, as T412 shows on fixtures built in code.
func TestT420_ProviderErrorFixtures(t *testing.T) {
	f, err := rpcfixture.Read(filepath.Join(fixturesDir, "error_range_too_large.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := rpcfixture.Serve(t, f)
	r := newRig(srv.URL(), "")
	page, err := r.c.FetchPage(context.Background(), scenarioConnWith(t, f.Context, r.lim), FamilyLogs,
		connector.ModeBackfill, json.RawMessage(`{}`))
	if err != nil || *cursorOf(t, page).NextBlock != 10 || r.m.rangeOf("anvil") != 10 {
		t.Errorf("run: %v, page %+v, range %d; want blocks 0 to 9", err, page, r.m.rangeOf("anvil"))
	}
	srv.AssertAllServed()
}

func scenarioConnWith(t *testing.T, ctx map[string]string, lim connector.Limiter) connector.Connection {
	conn := scenarioConn(t, ctx)
	conn.Limiter = lim
	return conn
}

// recordingProxy is an endpoint in front of Anvil whose calls the recorder keeps: methods, params and answers only.
func recordingProxy(t *testing.T, anvilURL string, rec *rpcfixture.Recorder) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, anvilURL, bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := rec.RoundTrip(req)
		if err != nil {
			http.Error(w, "anvil", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// S3-T420, the recording — Req: §2.6; S3 D-13, S3 D-28. On Anvil: mint, transfer in, transfer out, debit and
// refund of W, made final. The logs streams of W and of the treasury connection are read through a recording
// endpoint, page by page and in one page; the recordings pass the suite. make fixtures-record writes them to
// testdata/fixtures/evm/; a normal run writes them to a temporary directory.
func TestT420_RecordScenarios(t *testing.T) {
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	usdc := func(n int64) *big.Int { return big.NewInt(n * 1_000_000) }
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(10))
	c.Mint(t, w, usdc(50))
	c.Transfer(t, w, common.HexToAddress("0x00000000000000000000000000000000000A4200"), usdc(3))
	c.SetDailyLimit(t, w, usdc(1000))
	c.Approve(t, w, usdc(20))
	auth := [32]byte{0x42, 0x01}
	c.Debit(t, w, usdc(20), auth)
	c.ApproveRefunds(t, usdc(5))
	c.Refund(t, auth, [32]byte{0x42, 0x02}, usdc(5))
	c.MineBlocks(t, 10)

	dir := t.TempDir()
	if *record {
		dir = fixturesDir
	}
	base := map[string]string{
		"backfill_floor": "0", "controller": c.Controller.Hex(), "asset": c.Token.Hex(), "treasury": c.Treasury.Hex(),
		"treasury_connection": fxTreasuryConnection,
	}
	accounts := map[string][2]string{
		scenarioWallet:   {w.Hex(), walletConnection},
		scenarioTreasury: {c.Treasury.Hex(), fxTreasuryConnection},
	}
	for scenario, acc := range accounts {
		for _, cs := range []connectortest.Case{connectortest.Paged, connectortest.Whole} {
			ctx := map[string]string{"account": acc[0], "connection_id": acc[1], "log_range_max": "2000"}
			for k, v := range base {
				ctx[k] = v
			}
			if cs == connectortest.Paged {
				ctx["log_range_max"] = strconv.Itoa(pagedRangeMax)
			}
			rec := &rpcfixture.Recorder{}
			conn := New([]uint64{31337}, map[string]Endpoints{
				"anvil": {Primary: vault.NewSecret(recordingProxy(t, c.RPCURL, rec))},
			}, nil, nil)
			s := connectortest.Setup{Connector: conn, Conn: scenarioConn(t, ctx), Stream: FamilyLogs,
				Mode: connector.ModeBackfill, Cursor: json.RawMessage(`{}`), Served: func() (int, int) { return 0, 0 }}
			pages := 0
			mode, cursor := s.Mode, s.Cursor
			s.Conn.Limiter = connectortest.NewLimiter()
			for more := true; more; pages++ {
				page, err := conn.FetchPage(context.Background(), s.Conn, s.Stream, mode, cursor)
				if err != nil {
					t.Fatalf("%s %s, page %d: %v", scenario, cs, pages+1, err)
				}
				mode, cursor, more = page.Mode, page.Cursor, page.More
			}
			if (cs == connectortest.Paged) != (pages > 1) {
				t.Fatalf("%s %s: %d pages", scenario, cs, pages)
			}
			f, err := rec.File("Recorded from Anvil by TestT420_RecordScenarios: the logs stream of the " + scenario +
				" scenario (mint, transfer in, transfer out, debit, refund), " + string(cs) + ", log_range_max " +
				ctx["log_range_max"] + ".")
			if err != nil {
				t.Fatal(err)
			}
			f.Context = ctx
			if err := rpcfixture.Write(scenarioFile(dir, scenario, cs), f); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The recordings pass the suite; the hand-written rate limit is copied beside them.
	if !*record {
		data, err := os.ReadFile(filepath.Join(fixturesDir, "error_rate_limit.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "error_rate_limit.json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for scenario := range accounts {
		t.Run(scenario, func(t *testing.T) { connectortest.Run(t, replay(dir, scenario)) })
	}
}
