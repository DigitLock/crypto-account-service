package rpcfixture_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// record writes the committed fixtures of testdata/fixtures/evm/. Only make fixtures-record sets it: a normal
// test run records into a temporary directory and leaves the repository unchanged.
var record = flag.Bool("record", false, "write the fixtures of testdata/fixtures/evm/ from Anvil")

// fixturesDir is testdata/fixtures/evm/ of the repository, seen from this package.
var fixturesDir = filepath.Join("..", "..", "testdata", "fixtures", "evm")

// headFixture is the committed fixture of the head of a fresh Anvil.
const headFixture = "anvil_head.json"

// headCalls are the calls of headFixture.
func headCalls(ctx context.Context, c *rpc.Client) (chainID, number hexutil.Uint64, block map[string]any, err error) {
	if err = c.CallContext(ctx, &chainID, "eth_chainId"); err != nil {
		return
	}
	if err = c.CallContext(ctx, &number, "eth_blockNumber"); err != nil {
		return
	}
	err = c.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false)
	return
}

// keyProxy is a reverse proxy in front of Anvil that accepts only the path and the query that carry the fake
// key, and forwards the body to the root of Anvil.
func keyProxy(t *testing.T, anvilURL, key string) (endpoint string, seen func() int) {
	t.Helper()
	target, err := url.Parse(anvilURL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.URL.Path, r.Out.URL.RawPath, r.Out.URL.RawQuery = "/", "", ""
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/"+key || r.URL.Query().Get("apikey") != key {
			http.Error(w, "unknown key", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		n++
		mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v2/" + key + "?apikey=" + key, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// S3-T107 — Req: §2.6; S3 D-13, S3 D-28. Recorded from Anvil through a URL with a fake key generated at run
// time in its path and query; the file holds methods, params and answers only; the fake server replays it.
func TestT107_RecordFromAnvil(t *testing.T) {
	ctx := context.Background()
	anvilURL := testchain.StartAnvil(t, testchain.ChainID)
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	key := hex.EncodeToString(keyBytes)
	endpoint, seen := keyProxy(t, anvilURL, key)

	rec := &rpcfixture.Recorder{}
	client, err := rpc.DialOptions(ctx, endpoint, rpc.WithHTTPClient(&http.Client{Transport: rec}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	chainID, number, block, err := headCalls(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(chainID) != testchain.ChainID || seen() != 3 {
		t.Fatalf("chain ID %d, %d requests through the key URL; want %d and 3", chainID, seen(), testchain.ChainID)
	}

	f, err := rec.File("Head of a fresh Anvil: eth_chainId, eth_blockNumber, eth_getBlockByNumber latest without transactions")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), headFixture)
	if *record {
		path = filepath.Join(fixturesDir, headFixture)
		if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := rpcfixture.Write(path, f); err != nil {
		t.Fatal(err)
	}

	t.Run("no part of the URL in the file", func(t *testing.T) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(endpoint)
		for _, s := range []string{key, endpoint, u.Host, u.Port(), "/v2/", "apikey", "127.0.0.1", "localhost", "http"} {
			if strings.Contains(string(data), s) {
				t.Errorf("the fixture contains %q", s)
			}
		}
		var generic map[string]any
		if err := json.Unmarshal(data, &generic); err != nil {
			t.Fatal(err)
		}
		if len(generic) != 2 || generic["description"] == nil || generic["calls"] == nil {
			t.Errorf("fixture members = %v, want description and calls only", generic)
		}
		for _, c := range f.Calls {
			if c.Method == "" || c.Result == nil || c.Error != nil || c.HTTPStatus != 0 {
				t.Errorf("call %+v, want a method and a result", c)
			}
		}
		if got := []string{f.Calls[0].Method, f.Calls[1].Method, f.Calls[2].Method}; len(f.Calls) != 3 ||
			strings.Join(got, ",") != "eth_chainId,eth_blockNumber,eth_getBlockByNumber" {
			t.Errorf("calls = %v", f.Calls)
		}
	})

	t.Run("replayed", func(t *testing.T) {
		srv := rpcfixture.ServeFile(t, path)
		replay, err := rpc.Dial(srv.URL())
		if err != nil {
			t.Fatal(err)
		}
		defer replay.Close()
		gotID, gotNumber, gotBlock, err := headCalls(ctx, replay)
		if err != nil {
			t.Fatal(err)
		}
		if gotID != chainID || gotNumber != number || gotBlock["hash"] != block["hash"] || gotBlock["hash"] == nil {
			t.Errorf("replay: chain ID %d, block %d %v; want %d, %d %v", gotID, gotNumber, gotBlock["hash"],
				chainID, number, block["hash"])
		}
		srv.AssertAllServed()
	})

	t.Run("batch recorded per element", func(t *testing.T) {
		rec := &rpcfixture.Recorder{}
		client, err := rpc.DialOptions(ctx, endpoint, rpc.WithHTTPClient(&http.Client{Transport: rec}))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		var id, n hexutil.Uint64
		batch := []rpc.BatchElem{
			{Method: "eth_chainId", Result: &id},
			{Method: "eth_getBalance", Args: []any{"0x00000000000000000000000000000000000000aa", "latest"}, Result: &n},
		}
		if err := client.BatchCallContext(ctx, batch); err != nil || batch[0].Error != nil || batch[1].Error != nil {
			t.Fatalf("batch: %v %v %v", err, batch[0].Error, batch[1].Error)
		}
		f, err := rec.File("batch")
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Calls) != 2 || f.Calls[0].Method != "eth_chainId" || f.Calls[1].Method != "eth_getBalance" ||
			string(f.Calls[1].Params) != `["0x00000000000000000000000000000000000000aa","latest"]` {
			t.Fatalf("batch calls = %+v", f.Calls)
		}

		srv := rpcfixture.Serve(t, f)
		replay, err := rpc.Dial(srv.URL())
		if err != nil {
			t.Fatal(err)
		}
		defer replay.Close()
		var id2, n2 hexutil.Uint64
		again := []rpc.BatchElem{
			{Method: "eth_chainId", Result: &id2},
			{Method: "eth_getBalance", Args: []any{"0x00000000000000000000000000000000000000aa", "latest"}, Result: &n2},
		}
		if err := replay.BatchCallContext(ctx, again); err != nil || again[0].Error != nil || id2 != id || n2 != n {
			t.Errorf("batch replay: %v %v; chain ID %d, balance %d", err, again[0].Error, id2, n2)
		}
		srv.AssertAllServed()
	})
}

// S3-T107 — Req: §2.6; S3 D-28. The committed fixture replays without any network.
func TestT107_ReplayCommittedFixture(t *testing.T) {
	srv := rpcfixture.ServeFile(t, filepath.Join(fixturesDir, headFixture))
	client, err := rpc.Dial(srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	chainID, _, block, err := headCalls(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(chainID) != testchain.ChainID || block["hash"] == nil {
		t.Errorf("chain ID %d, block hash %v", chainID, block["hash"])
	}
	srv.AssertAllServed()
}

// captureTB records the errors that the server reports, so a test can check them without failing.
type captureTB struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (c *captureTB) Errorf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, fmt.Sprintf(format, args...))
}

func (c *captureTB) errors() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.errs)
}

// S3-T107 — Req: §2.6; S3 D-28. Hand-written answers, mismatches and unserved calls, without a network.
func TestT107_ReplayerRules(t *testing.T) {
	ctx := context.Background()
	call := func(method, params, result string) rpcfixture.Call {
		return rpcfixture.Call{Method: method, Params: json.RawMessage(params), Result: json.RawMessage(result)}
	}

	t.Run("error answer and http_status", func(t *testing.T) {
		srv := rpcfixture.Serve(t, rpcfixture.File{Calls: []rpcfixture.Call{
			{Method: "eth_getLogs", Params: json.RawMessage(`[{"fromBlock":"0x1","toBlock":"0x7d1"}]`),
				Error: &rpcfixture.Error{Code: -32005, Message: "query returned more than 10000 results"}},
			{Method: "eth_blockNumber", Params: json.RawMessage(`[]`), HTTPStatus: http.StatusTooManyRequests},
		}})
		client, err := rpc.Dial(srv.URL())
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		var logs []any
		err = client.CallContext(ctx, &logs, "eth_getLogs", map[string]any{"fromBlock": "0x1", "toBlock": "0x7d1"})
		var rpcErr rpc.Error
		if !errors.As(err, &rpcErr) || rpcErr.ErrorCode() != -32005 {
			t.Errorf("eth_getLogs: %v, want the JSON-RPC error -32005", err)
		}
		var n hexutil.Uint64
		err = client.CallContext(ctx, &n, "eth_blockNumber")
		var httpErr rpc.HTTPError
		if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests || len(httpErr.Body) != 0 {
			t.Errorf("eth_blockNumber: %v, want HTTP 429 with an empty body", err)
		}
		srv.AssertAllServed()
	})

	t.Run("params compared as JSON", func(t *testing.T) {
		srv := rpcfixture.Serve(t, rpcfixture.File{Calls: []rpcfixture.Call{
			call("eth_getBlockByNumber", `[ "latest" , false ]`, `{"number":"0x0"}`),
			call("eth_chainId", `null`, `"0x7a69"`),
		}})
		client, err := rpc.Dial(srv.URL())
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		var block map[string]any
		var id hexutil.Uint64
		if err := client.CallContext(ctx, &block, "eth_getBlockByNumber", "latest", false); err != nil {
			t.Fatal(err)
		}
		if err := client.CallContext(ctx, &id, "eth_chainId"); err != nil || id != 31337 {
			t.Fatalf("eth_chainId: %v %d", err, id)
		}
		srv.AssertAllServed()
	})

	for name, c := range map[string]struct {
		method string
		args   []any
	}{
		"other method":     {"eth_chainId", nil},
		"other params":     {"eth_getBlockByNumber", []any{"finalized", false}},
		"beyond the calls": {"eth_getBlockByNumber", []any{"latest", false}},
	} {
		t.Run("mismatch fails the test: "+name, func(t *testing.T) {
			calls := []rpcfixture.Call{call("eth_getBlockByNumber", `["latest",false]`, `{"number":"0x0"}`)}
			capture := &captureTB{TB: t}
			srv := rpcfixture.Serve(capture, rpcfixture.File{Calls: calls})
			client, err := rpc.Dial(srv.URL())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var out any
			if name == "beyond the calls" {
				if err := client.CallContext(ctx, &out, c.method, c.args...); err != nil {
					t.Fatal(err)
				}
			}
			if err := client.CallContext(ctx, &out, c.method, c.args...); err == nil {
				t.Error("the client got an answer to a request that does not match")
			}
			if capture.errors() != 1 {
				t.Errorf("%d errors reported, want 1", capture.errors())
			}
		})
	}

	t.Run("unserved call is reported", func(t *testing.T) {
		capture := &captureTB{TB: t}
		srv := rpcfixture.Serve(capture, rpcfixture.File{Calls: []rpcfixture.Call{call("eth_chainId", `[]`, `"0x7a69"`)}})
		srv.AssertAllServed()
		if capture.errors() != 1 || srv.Served() != 0 {
			t.Errorf("%d errors, %d served; want 1 and 0", capture.errors(), srv.Served())
		}
	})

	t.Run("a call needs exactly one answer", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		for _, body := range []string{
			`{"description":"x","calls":[{"method":"eth_chainId","params":[]}]}`,
			`{"description":"x","calls":[{"method":"eth_chainId","params":[],"result":"0x1","http_status":429}]}`,
			`{"description":"x","calls":[{"method":"eth_chainId","params":[],"result":"0x1","url":"http://x"}]}`,
		} {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := rpcfixture.Read(path); err == nil {
				t.Errorf("Read accepted %s", body)
			}
		}
	})
}
