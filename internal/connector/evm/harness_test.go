package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/rpcfixture"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Fictitious addresses of the fixture runs.
var (
	fxController = common.HexToAddress("0x00000000000000000000000000000000000c0001")
	fxToken      = common.HexToAddress("0x00000000000000000000000000000000000c0002")
	fxTreasury   = common.HexToAddress("0x00000000000000000000000000000000000c0003")
	fxOther      = common.HexToAddress("0x00000000000000000000000000000000000c0004")
)

const fxTreasuryConnection = "0b0b0b0b-0000-4000-8000-000000000001"

// metricsRec records the metrics of the connector.
type metricsRec struct {
	mu       sync.Mutex
	final    map[string]uint64
	reorgs   map[string]int
	checks   map[string]bool // source/check → failed
	requests map[string]int  // source/endpoint/method/result
	fallback map[string]bool
	ranges   map[string]uint64
	lag      map[string]uint64
	unmatch  map[string]int
	skipped  map[string]int // source/reason
	complete map[string]int
}

func newMetricsRec() *metricsRec {
	return &metricsRec{final: map[string]uint64{}, reorgs: map[string]int{}, checks: map[string]bool{},
		requests: map[string]int{}, fallback: map[string]bool{}, ranges: map[string]uint64{},
		lag: map[string]uint64{}, unmatch: map[string]int{}, skipped: map[string]int{}, complete: map[string]int{}}
}

func (m *metricsRec) LogRangeBlocks(source string, n uint64) {
	m.mu.Lock()
	m.ranges[source] = n
	m.mu.Unlock()
}

func (m *metricsRec) IndexerLag(source string, n uint64) {
	m.mu.Lock()
	m.lag[source] = n
	m.mu.Unlock()
}

func (m *metricsRec) UnmatchedControllerEvent(source string) {
	m.mu.Lock()
	m.unmatch[source]++
	m.mu.Unlock()
}

func (m *metricsRec) CompletenessSkipped(source string) {
	m.mu.Lock()
	m.complete[source]++
	m.mu.Unlock()
}

func (m *metricsRec) SkippedLog(source, reason string) {
	m.mu.Lock()
	m.skipped[source+"/"+reason]++
	m.mu.Unlock()
}

func (m *metricsRec) rangeOf(source string) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ranges[source]
}

func (m *metricsRec) FinalBlock(source string, n uint64) {
	m.mu.Lock()
	m.final[source] = n
	m.mu.Unlock()
}
func (m *metricsRec) ReorgBelowFinal(source string) { m.mu.Lock(); m.reorgs[source]++; m.mu.Unlock() }
func (m *metricsRec) StartCheck(source, check string, failed bool) {
	m.mu.Lock()
	m.checks[source+"/"+check] = failed
	m.mu.Unlock()
}
func (m *metricsRec) RPCRequest(source, endpoint, method, result string) {
	m.mu.Lock()
	m.requests[source+"/"+endpoint+"/"+method+"/"+result]++
	m.mu.Unlock()
}
func (m *metricsRec) FallbackActive(source string, active bool) {
	m.mu.Lock()
	m.fallback[source] = active
	m.mu.Unlock()
}

func (m *metricsRec) check(source, check string) (failed, set bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	failed, set = m.checks[source+"/"+check]
	return failed, set
}

func (m *metricsRec) requestsOf(endpoint string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, v := range m.requests {
		if strings.Contains(k, "/"+endpoint+"/") {
			n += v
		}
	}
	return n
}

// recLimiter records reservations and pauses per budget; it never waits.
type recLimiter struct {
	mu       sync.Mutex
	reserved map[string]int
	paused   map[string]time.Duration
}

func newRecLimiter() *recLimiter {
	return &recLimiter{reserved: map[string]int{}, paused: map[string]time.Duration{}}
}

func (l *recLimiter) Reserve(_ context.Context, budget string, cost int) error {
	l.mu.Lock()
	l.reserved[budget] += cost
	l.mu.Unlock()
	return nil
}

func (l *recLimiter) Pause(budget string, d time.Duration) {
	l.mu.Lock()
	l.paused[budget] = d
	l.mu.Unlock()
}

// syncBuffer is the log of the connector, written by its runs and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// rig is a connector on given endpoints with its metrics, log and limiter.
type rig struct {
	c   *Connector
	m   *metricsRec
	log *syncBuffer
	lim *recLimiter
}

func newRig(primary, fallback string) *rig {
	r := &rig{m: newMetricsRec(), log: &syncBuffer{}, lim: newRecLimiter()}
	ep := Endpoints{Primary: vault.NewSecret(primary), Fallback: vault.NewSecret(fallback)}
	r.c = New([]uint64{31337, 84532}, map[string]Endpoints{"anvil": ep}, r.m, slog.New(slog.NewJSONHandler(r.log, nil)))
	return r
}

// fxSource is the source anvil of the fixture runs: confirmations 10, floor 0, the token as its alias row.
func fxSource(extra string) connector.Source {
	config := `{"chain_id": 31337, "finality_mode": "confirmations", "finality_confirmations": 10,
		"controller_address": "` + fxController.Hex() + `", "backfill_floor": 0`
	if extra != "" {
		config += ", " + extra
	}
	decimals := int16(6)
	return connector.Source{Code: "anvil", Kind: connector.KindEVM, Enabled: true, Config: json.RawMessage(config + "}"),
		Aliases: []connector.Alias{{NativeAsset: fxToken.Hex(), Asset: "USDC", Decimals: &decimals}}}
}

func withTreasury() string {
	return `"treasury_connection": "` + fxTreasuryConnection + `", "treasury_address": "` + fxTreasury.Hex() + `"`
}

func (r *rig) conn(id string, src connector.Source) connector.Connection {
	return connector.Connection{ID: id, Source: src, Account: "0x00000000000000000000000000000000000000aa", Limiter: r.lim}
}

func (r *rig) logs(conn connector.Connection, cursor string) (connector.Page, error) {
	return r.c.FetchPage(context.Background(), conn, FamilyLogs, connector.ModeBackfill, json.RawMessage(cursor))
}

// script builds the calls of a fixture in code.
type script struct{ calls []rpcfixture.Call }

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (s *script) result(t *testing.T, method string, params []any, result any) *script {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	s.calls = append(s.calls, rpcfixture.Call{Method: method, Params: raw(t, params), Result: raw(t, result)})
	return s
}

func (s *script) rpcError(t *testing.T, method string, params []any, code int, message string) *script {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	s.calls = append(s.calls, rpcfixture.Call{Method: method, Params: raw(t, params),
		Error: &rpcfixture.Error{Code: code, Message: message}})
	return s
}

func (s *script) status(t *testing.T, method string, params []any, status int) *script {
	t.Helper()
	if params == nil {
		params = []any{}
	}
	s.calls = append(s.calls, rpcfixture.Call{Method: method, Params: raw(t, params), HTTPStatus: status})
	return s
}

func (s *script) chainID(t *testing.T, id uint64) *script {
	return s.result(t, "eth_chainId", nil, hexutil.Uint64(id))
}

// constant is the eth_call of token() or treasury() of the controller answering address.
func (s *script) constant(t *testing.T, method string, address common.Address) *script {
	t.Helper()
	return s.result(t, "eth_call", callParams(t, method), hexutil.Bytes(common.LeftPadBytes(address.Bytes(), 32)))
}

func callParams(t *testing.T, method string) []any {
	t.Helper()
	data, err := controllerABI.Pack(method)
	if err != nil {
		t.Fatal(err)
	}
	return []any{map[string]any{"to": fxController, "data": hexutil.Bytes(data)}, "latest"}
}

// checks are the start checks of a run that pass: chain ID, token and, with a treasury, treasury.
func (s *script) checks(t *testing.T, treasury bool) *script {
	s.chainID(t, 31337).constant(t, "token", fxToken)
	if treasury {
		s.constant(t, "treasury", fxTreasury)
	}
	return s
}

// logsChecks are the checks before a logs run of a wallet in a source without a treasury connection: the start
// checks, then treasury() of the controller, read once per network by the treasury guard (S3 D-42).
func (s *script) logsChecks(t *testing.T) *script {
	return s.checks(t, false).constant(t, "treasury", fxTreasury)
}

func (s *script) head(t *testing.T, n uint64) *script {
	return s.result(t, "eth_blockNumber", nil, hexutil.Uint64(n))
}

// blockHash is a fictitious hash of a block number.
func blockHash(n uint64, fork byte) common.Hash {
	var h common.Hash
	h[0], h[31] = fork, byte(n)
	h[30] = byte(n >> 8)
	return h
}

func (s *script) header(t *testing.T, block string, n uint64, fork byte) *script {
	return s.result(t, "eth_getBlockByNumber", []any{block, false},
		map[string]any{"number": hexutil.Uint64(n), "hash": blockHash(n, fork), "timestamp": hexutil.Uint64(1_760_000_000 + n)})
}

func (s *script) noHeader(t *testing.T, block string) *script {
	return s.result(t, "eth_getBlockByNumber", []any{block, false}, nil)
}

func (s *script) file() rpcfixture.File { return rpcfixture.File{Calls: s.calls} }

func cursorJSON(next uint64, hash common.Hash) string {
	return `{"next_block": ` + jsonUint(next) + `, "last_hash": "` + hash.Hex() + `", "last_time": "2026-10-07T10:00:00Z"}`
}

func jsonUint(n uint64) string { b, _ := json.Marshal(n); return string(b) }
