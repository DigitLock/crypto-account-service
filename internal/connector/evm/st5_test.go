package evm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
)

// Topic 0 of the events, computed from their signatures: independent of the ABI the connector reads.
var (
	sigTransfer = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))
	sigDebited  = crypto.Keccak256Hash([]byte("Debited(bytes32,address,uint256)"))
	sigRefunded = crypto.Keccak256Hash([]byte("Refunded(bytes32,bytes32,address,uint256)"))
)

// logQuery is the parameter list of eth_getLogs for blocks from to to.
func logQuery(from, to uint64, address common.Address, topics ...any) []any {
	return []any{map[string]any{"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to),
		"address": []common.Address{address}, "topics": topics}}
}

// walletQueries are the four filters of a wallet of the fixture source for blocks from to to (§2.1.2).
func walletQueries(from, to uint64, account common.Address) [][]any {
	who := common.BytesToHash(account.Bytes())
	return [][]any{
		logQuery(from, to, fxToken, sigTransfer, who),
		logQuery(from, to, fxToken, sigTransfer, nil, who),
		logQuery(from, to, fxController, sigDebited, nil, who),
		logQuery(from, to, fxController, sigRefunded, nil, nil, who),
	}
}

// noLogs answers the four filters of fxAccount for blocks from to to with no log.
func (s *script) noLogs(t *testing.T, from, to uint64) *script {
	for _, q := range walletQueries(from, to, fxAccount) {
		s.result(t, "eth_getLogs", q, []any{})
	}
	return s
}

// page reads one logs page of fxAccount for blocks from to to without logs: the filters and the header of the end.
func (s *script) page(t *testing.T, from, to uint64) *script {
	return s.noLogs(t, from, to).header(t, hexutil.EncodeUint64(to), to, 1)
}

func cursorOf(t *testing.T, page connector.Page) cursor {
	t.Helper()
	var c cursor
	if err := json.Unmarshal(page.Cursor, &c); err != nil || c.NextBlock == nil {
		t.Fatalf("cursor %s: %v", page.Cursor, err)
	}
	return c
}

// S3-T412 — Req: FR-311, EC-306. The provider rejects 2000 blocks, then 1000, accepts 500: the range is halved and
// repeated, never skipped; 500 is kept for the source and is the size of the next run; evm_log_range_blocks 500.
// Down to one block; one block still rejected fails the run.
func TestT412_RangeHalvedOnRejection(t *testing.T) {
	tooLarge := func(s *script, from, to uint64) *script {
		return s.rpcError(t, "eth_getLogs", walletQueries(from, to, fxAccount)[0], -32602,
			"Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range")
	}
	// Head 3010, confirmations 10: F = 3000.
	s := (&script{}).checks(t, false).head(t, 3010)
	tooLarge(s, 0, 1999)
	s.status(t, "eth_getLogs", walletQueries(0, 999, fxAccount)[0], http.StatusRequestEntityTooLarge)
	s.page(t, 0, 499)
	// The next run starts with 500 blocks.
	s.head(t, 3010).header(t, hexutil.EncodeUint64(499), 499, 1).page(t, 500, 999)
	srv := rpcfixture.Serve(t, s.file())
	r := newRig(srv.URL(), "")
	conn := r.conn("w", fxSource(""))

	page, err := r.logs(conn, `{}`)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if c := cursorOf(t, page); *c.NextBlock != 500 || !page.More || page.Mode != connector.ModeBackfill {
		t.Errorf("run 1: cursor %s, more %v, mode %s; want next_block 500, more, BACKFILL", page.Cursor, page.More, page.Mode)
	}
	if got := r.m.rangeOf("anvil"); got != 500 {
		t.Errorf("evm_log_range_blocks = %d, want 500", got)
	}
	if !strings.Contains(r.log.String(), "the range is halved") {
		t.Errorf("no WARN line of the halving:\n%s", r.log)
	}
	page, err = r.logs(conn, string(page.Cursor))
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if c := cursorOf(t, page); *c.NextBlock != 1000 {
		t.Errorf("run 2: cursor %s, want next_block 1000", page.Cursor)
	}
	srv.AssertAllServed()

	t.Run("down to one block, then failure", func(t *testing.T) {
		s := (&script{}).checks(t, false).head(t, 30)
		for _, rg := range [][2]uint64{{0, 3}, {0, 1}, {0, 0}} {
			tooLarge(s, rg[0], rg[1])
		}
		srv := rpcfixture.Serve(t, s.file())
		r := newRig(srv.URL(), "")
		_, err := r.logs(r.conn("w", fxSource(`"log_range_max": 4`)), `{}`)
		var tl *tooLargeError
		if !errors.As(err, &tl) || !strings.Contains(err.Error(), "single block 0") {
			t.Errorf("run: %v, want the failure of one block", err)
		}
		if got := r.m.rangeOf("anvil"); got != 1 {
			t.Errorf("evm_log_range_blocks = %d, want 1", got)
		}
		srv.AssertAllServed()
	})

	t.Run("never above log_range_max", func(t *testing.T) {
		r := newRig("http://127.0.0.1:1", "")
		s := &session{c: r.c, src: fxSource(""), net: r.c.network("anvil"), cfg: Config{LogRangeMax: 300}}
		s.net.rangeSize = 500
		if got := s.rangeSize(); got != 300 {
			t.Errorf("range size %d, want log_range_max 300", got)
		}
	})
}

// S3-T412, the texts of EC-306 — Req: EC-306. Each provider text of a rejected range or answer is "too large",
// also with -32005; a rate limit with -32005 is not; HTTP 413 is, HTTP 429 is not.
func TestT412_TooLargeTexts(t *testing.T) {
	tooLarge := map[string]int{
		// Alchemy (docs: "deep dive into eth_getLogs")
		"Log response size exceeded. You can make eth_getLogs requests with up to a 2K block range and no limit on the response size, or you can request any block range with a cap of 10K logs in the response.": -32602,
		// Alchemy free tier (support article on the free tier error)
		"Under the Free tier plan, you can make eth_getLogs requests with up to a 10 block range.": -32600,
		// Infura (docs: eth_getLogs constraints)
		"query returned more than 10000 results": -32005,
		// QuickNode (support: the 10,000 block range limit)
		"eth_getLogs and eth_newFilter are limited to a 10,000 blocks range": -32614,
		// geth --rpc.rangelimit
		"exceed maximum block range: 10000": -32000,
		// Reth
		"query exceeds max block range 100000": -32602,
		"query exceeds max results 20000":      -32602,
		// public endpoints
		"block range is too wide (maximum 1024)": -32000,
	}
	for text, code := range tooLarge {
		if !isTooLarge(jsonError{code, text}) {
			t.Errorf("%q (%d) is not too large", text, code)
		}
	}
	for text, code := range map[string]int{"limit exceeded": -32005, "rate limit reached": -32005, "header not found": -32000} {
		if isTooLarge(jsonError{code, text}) {
			t.Errorf("%q (%d) is too large", text, code)
		}
	}
	if !isTooLarge(rpc.HTTPError{StatusCode: http.StatusRequestEntityTooLarge}) ||
		isTooLarge(rpc.HTTPError{StatusCode: http.StatusTooManyRequests}) || isTooLarge(errors.New("block range")) {
		t.Error("HTTP statuses or a plain error misread")
	}

	t.Run("-32005 too large is not a rate limit", func(t *testing.T) {
		srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 30).
			rpcError(t, "eth_getLogs", walletQueries(0, 19, fxAccount)[0], -32005, "query returned more than 10000 results").
			page(t, 0, 9).file())
		r := newRig(srv.URL(), "")
		if _, err := r.logs(r.conn("w", fxSource(`"log_range_max": 20`)), `{}`); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(r.lim.paused) != 0 {
			t.Errorf("budget paused %v", r.lim.paused)
		}
		srv.AssertAllServed()
	})
}

// jsonError is a JSON-RPC error as go-ethereum returns it.
type jsonError struct {
	code int
	msg  string
}

func (e jsonError) Error() string  { return e.msg }
func (e jsonError) ErrorCode() int { return e.code }

// S3-T413 — Req: EC-307. A lagging node: no logs, and the header of the range end is missing. The run fails, not
// "no logs"; no cursor is returned, so the engine does not move it.
func TestT413_LaggingNode(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 30).noLogs(t, 0, 20).
		noHeader(t, hexutil.EncodeUint64(20)).file())
	r := newRig(srv.URL(), "")
	page, err := r.logs(r.conn("w", fxSource("")), `{}`)
	if err == nil || !strings.Contains(err.Error(), "EC-307") || page.Cursor != nil || isRPCFailure(err) {
		t.Errorf("run: %v, page %+v; want the failure of a lagging node and no cursor", err, page)
	}
	srv.AssertAllServed()
}

// S3-T416, the connector part — Req: §2.4 Cursor formats; S3 D-7. The cursor of a page: the next block, the hash
// and the time of the range end; the first run starts from {} at backfill_floor.
func TestT416_CursorFormat(t *testing.T) {
	srv := rpcfixture.Serve(t, (&script{}).checks(t, false).head(t, 30).page(t, 7, 20).file())
	r := newRig(srv.URL(), "")
	page, err := r.logs(r.conn("w", fxSource(`"backfill_floor": 7`)), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(page.Cursor, &generic); err != nil || len(generic) != 3 {
		t.Fatalf("cursor %s, want three members", page.Cursor)
	}
	want := `{"next_block":21,"last_hash":"` + blockHash(20, 1).Hex() + `","last_time":"` +
		blockTime(20).Format("2006-01-02T15:04:05Z") + `"}`
	if string(page.Cursor) != want || page.Mode != connector.ModeIncremental || page.More {
		t.Errorf("page cursor %s, mode %s, more %v; want %s, INCREMENTAL, no more", page.Cursor, page.Mode, page.More, want)
	}
	srv.AssertAllServed()
}
