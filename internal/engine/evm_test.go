package engine_test

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Fictitious addresses of the EVM runs of the engine tests.
var (
	evmController = common.HexToAddress("0x00000000000000000000000000000000000c0001")
	evmToken      = common.HexToAddress("0x00000000000000000000000000000000000c0002")
)

// fakeNode is a JSON-RPC endpoint that answers by method, in any order, and records the methods it served.
type fakeNode struct {
	mu      sync.Mutex
	answers map[string]any // method, or eth_getBlockByNumber/<block> → result
	methods []string
}

func newFakeNode(t *testing.T, answers map[string]any) (*fakeNode, string) {
	t.Helper()
	n := &fakeNode{answers: answers}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params []any           `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "not JSON-RPC", http.StatusBadRequest)
			return
		}
		key := req.Method
		if req.Method == "eth_getBlockByNumber" && len(req.Params) > 0 {
			key += "/" + req.Params[0].(string)
		}
		n.mu.Lock()
		n.methods = append(n.methods, key)
		result, ok := n.answers[key]
		n.mu.Unlock()
		if !ok {
			t.Errorf("fake node: unexpected %s", key)
			http.Error(w, "unexpected", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(srv.Close)
	return n, srv.URL
}

// useEVM registers an EVM connector with the endpoint of anvil (none when url is empty), sets the values of the
// source anvil for the fixture addresses and its alias row; the config is restored at the end of the test.
// rpc_rate_limit 100: the limiter waits on the fake clock, and a run of both streams can send more than 5 requests.
func (h *harness) useEVM(t *testing.T, url string) {
	t.Helper()
	endpoints := map[string]evm.Endpoints{}
	if url != "" {
		endpoints["anvil"] = evm.Endpoints{Primary: vault.NewSecret(url)}
	}
	h.set.RegisterEVM(evm.New([]uint64{31337, 84532}, endpoints, nil, h.logger))
	var saved []byte
	if err := h.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.exec(t, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved) })
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('controller_address', $1::text, 'backfill_floor', 0,
		'rpc_rate_limit', 100) WHERE code = 'anvil'`, evmController.Hex())
	h.exec(t, `INSERT INTO asset_aliases (source_id, native_asset, asset, decimals)
		SELECT id, $1, 'USDC', 6 FROM sources WHERE code = 'anvil' ON CONFLICT DO NOTHING`, evmToken.Hex())
}

func (h *harness) wallet(t *testing.T) uuid.UUID {
	t.Helper()
	c, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: h.tenantID, CredentialID: h.credentialID, OwnerRef: "owner-1", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: "0x" + hex.EncodeToString(randomBytes(t, 20))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// tokenAnswer is token() of the controller: the token address, ABI-encoded.
func tokenAnswer() hexutil.Bytes { return common.LeftPadBytes(evmToken.Bytes(), 32) }

// S3-T207, the engine part — Req: FR-314; S3 D-23. A failed start check counts as any failed run: the backoff, and
// DEGRADED after the threshold.
func TestT207_StartCheckFailureCounts(t *testing.T) {
	h := setup(t)
	h.cfg.FailureThreshold = 2
	node, url := newFakeNode(t, map[string]any{"eth_chainId": hexutil.Uint64(84532)})
	h.useEVM(t, url)
	id := h.wallet(t)
	e := h.engine(h.server)

	for pass := 1; pass <= 2; pass++ {
		h.pass(t, e)
		for _, stream := range []string{"balances", "logs"} {
			c := h.cursor(t, id, stream)
			if c.failures != pass || c.lastError == nil || !strings.Contains(*c.lastError, "start check chain_id failed") {
				t.Errorf("pass %d, %s: failures %d, last_error %v", pass, stream, c.failures, c.lastError)
			}
			if wantNext := h.clock.Now().Add(30 * time.Second << (pass - 1)); !c.nextRunAt.Equal(wantNext) {
				t.Errorf("pass %d, %s: next run %s, want the backoff %s", pass, stream, c.nextRunAt, wantNext)
			}
		}
		h.clock.Advance(time.Hour)
	}
	if got := h.status(t, id); got != "DEGRADED" {
		t.Errorf("status = %s, want DEGRADED after the threshold", got)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	for _, m := range node.methods {
		if m != "eth_chainId" {
			t.Errorf("a read after the failed check: %s", m)
		}
	}
}

// S3-T204, the engine part — Req: FR-312, EC-308. A guard hit stores nothing: the cursor, the entries and the
// checkpoints stay; the stream counts a failure with REORG_BELOW_FINAL in last_error, run after run.
func TestT204_ReorgLeavesStoredDataUnchanged(t *testing.T) {
	h := setup(t)
	stored, found := common.HexToHash("0x01"), common.HexToHash("0x02")
	_, url := newFakeNode(t, map[string]any{
		"eth_chainId":                    hexutil.Uint64(31337),
		"eth_call":                       tokenAnswer(),
		"eth_blockNumber":                hexutil.Uint64(100),
		"eth_getBlockByNumber/" + "0x13": map[string]any{"number": "0x13", "hash": found, "timestamp": "0x1"},
		// The balances stream: the head block; its balanceOf is answered by the eth_call above.
		"eth_getBlockByNumber/latest": map[string]any{"number": "0x64", "hash": common.HexToHash("0x64"), "timestamp": "0x64"},
	})
	h.useEVM(t, url)
	id := h.wallet(t)
	cursor := `{"last_hash": "` + stored.Hex() + `", "last_time": "2026-10-07T10:00:00Z", "next_block": 20}`
	h.exec(t, `UPDATE sync_cursors SET cursor = $2, mode = 'INCREMENTAL' WHERE connection_id = $1 AND stream = 'logs'`, id, cursor)
	h.exec(t, `INSERT INTO ledger_entries (tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
		asset, native_asset, amount, occurred_at, raw) VALUES ($1, $2, 'logs', '0xab:1', 'SINGLE', 'logs:0xab:1', 'DEPOSIT',
		'IN', 'USDC', $3, 5, now(), '{}')`, h.tenantID, id, evmToken.Hex())
	h.exec(t, `INSERT INTO balance_checkpoints (connection_id, native_asset, asset, block_number, block_hash, taken_at,
		balance, ledger_total, gap) VALUES ($1, $2, 'USDC', 19, $3, now(), 5, 5, 0)`, id, evmToken.Hex(), stored.Hex())
	snapshot := func() string {
		var s string
		if err := h.owner.QueryRow(ctx, `SELECT
			(SELECT cursor::text || mode FROM sync_cursors WHERE connection_id = $1 AND stream = 'logs') ||
			(SELECT string_agg(seq::text || external_id || amount::text, ',') FROM ledger_entries WHERE connection_id = $1) ||
			(SELECT string_agg(native_asset || block_hash || balance::text || gap::text || checked_at::text, ',')
				FROM balance_checkpoints WHERE connection_id = $1)`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	e := h.engine(h.server)
	for pass := 1; pass <= 2; pass++ {
		h.pass(t, e)
		c := h.cursor(t, id, "logs")
		if c.failures != pass || c.lastError == nil || !strings.Contains(*c.lastError, "REORG_BELOW_FINAL") ||
			!strings.Contains(*c.lastError, stored.Hex()) || !strings.Contains(*c.lastError, found.Hex()) {
			t.Errorf("pass %d: failures %d, last_error %v", pass, c.failures, c.lastError)
		}
		if got := snapshot(); got != before {
			t.Errorf("pass %d: stored data changed:\n%s\n%s", pass, before, got)
		}
		h.clock.Advance(time.Hour)
	}
}

// S3-T215, the engine part — Req: §2.1.1, §3.1; Core FR-122; S3 D-16. Without EVM_RPC_URL_ANVIL a wallet
// connection is created without streams; with it, the next start creates both cursors, due at once.
func TestT215_SourceWithoutEndpoint(t *testing.T) {
	h := setup(t)
	h.useEVM(t, "")
	id := h.wallet(t)
	if n := h.count(t, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1`, id); n != 0 {
		t.Fatalf("%d cursors without an endpoint, want 0", n)
	}
	h.pass(t, h.engine(h.server))

	// The URL is set; the restart creates the cursors.
	h.set.RegisterEVM(evm.New([]uint64{31337, 84532}, map[string]evm.Endpoints{
		"anvil": {Primary: vault.NewSecret("http://127.0.0.1:1")},
	}, nil, h.logger))
	h.engine(h.server).CreateMissingCursors(ctx)
	for stream, wantMode := range map[string]string{"balances": "INCREMENTAL", "logs": "BACKFILL"} {
		c := h.cursor(t, id, stream)
		if c.mode != wantMode || c.cursor != "{}" || !c.nextRunAt.Equal(h.clock.Now()) {
			t.Errorf("%s: %+v, want mode %s, cursor {}, due now", stream, c, wantMode)
		}
	}
}
