package engine_test

import (
	"bytes"
	"encoding/json"
	"math/big"
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
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

// Selectors of the calls the connector makes: token() of the controller, balanceOf(address) of a token.
var (
	selectorToken     = []byte{0xfc, 0x0c, 0x54, 0x6a}
	selectorBalanceOf = []byte{0x70, 0xa0, 0x82, 0x31}
)

// Failure modes of balanceOf at balanceNode.
const (
	failNone = ""
	failRPC  = "JSON-RPC error"
	failHTTP = "HTTP 500"
)

// balanceNode is a JSON-RPC endpoint for the balances stream of evmToken: the chain ID, token() of the controller, a
// head block, balanceOf by wallet pinned to the hash of that head, and a head of 5 for the logs stream, where no
// block is final.
type balanceNode struct {
	t        *testing.T
	mu       sync.Mutex
	head     uint64
	hash     common.Hash
	time     time.Time
	balances map[common.Address]*big.Int
	fail     string
	heads    int // reads of the head block
}

// useBalanceNode registers the EVM connector on a new balanceNode whose head block has time at.
func (h *harness) useBalanceNode(t *testing.T, at time.Time) *balanceNode {
	t.Helper()
	node, url := newBalanceNode(t, at)
	h.useEVM(t, url)
	return node
}

func newBalanceNode(t *testing.T, at time.Time) (*balanceNode, string) {
	t.Helper()
	n := &balanceNode{t: t, head: 42, hash: common.HexToHash("0x4242"), time: at, balances: map[common.Address]*big.Int{}}
	srv := httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(srv.Close)
	return n, srv.URL
}

func (n *balanceNode) set(fn func(n *balanceNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

func (n *balanceNode) headReads() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.heads
}

func (n *balanceNode) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "not JSON-RPC", http.StatusBadRequest)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	answer := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
	switch req.Method {
	case "eth_chainId":
		answer(hexutil.Uint64(31337))
	case "eth_blockNumber":
		answer(hexutil.Uint64(5))
	case "eth_getBlockByNumber":
		n.heads++
		answer(map[string]any{"number": hexutil.Uint64(n.head), "hash": n.hash, "timestamp": hexutil.Uint64(n.time.Unix())})
	case "eth_call":
		var call struct {
			Data hexutil.Bytes `json:"data"`
		}
		if len(req.Params) != 2 || json.Unmarshal(req.Params[0], &call) != nil || len(call.Data) < 4 {
			n.t.Errorf("balance node: malformed eth_call %s", req.Params)
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		switch {
		case bytes.Equal(call.Data[:4], selectorToken):
			answer(tokenAnswer())
		case bytes.Equal(call.Data[:4], selectorBalanceOf) && len(call.Data) == 36:
			var pin map[string]common.Hash
			if json.Unmarshal(req.Params[1], &pin) != nil || len(pin) != 1 || pin["blockHash"] != n.hash {
				n.t.Errorf("balance node: balanceOf at %s, want the hash of the head %s", req.Params[1], n.hash.Hex())
			}
			switch n.fail {
			case failRPC:
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
					"error": map[string]any{"code": -32000, "message": "header not found"}})
			case failHTTP:
				http.Error(w, "internal error", http.StatusInternalServerError)
			default:
				units := n.balances[common.BytesToAddress(call.Data[4:])]
				if units == nil {
					units = new(big.Int)
				}
				answer(hexutil.Bytes(common.LeftPadBytes(units.Bytes(), 32)))
			}
		default:
			n.t.Errorf("balance node: unexpected eth_call %s", call.Data)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	default:
		n.t.Errorf("balance node: unexpected %s", req.Method)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}
}

// walletAt creates a wallet connection of owner-1 on anvil for address.
func (h *harness) walletAt(t *testing.T, address common.Address) uuid.UUID {
	t.Helper()
	c, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: h.tenantID, CredentialID: h.credentialID, OwnerRef: "owner-1", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: address.Hex()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// useAnvil registers an EVM connector on the local chain c and sets the source anvil to its deployment: the
// controller and the token as the alias row. backfill_floor lies far above the head, so the logs stream reads no
// range and succeeds.
func (h *harness) useAnvil(t *testing.T, c *testchain.Chain) {
	t.Helper()
	h.useAnvilAt(t, c, c.RPCURL, `{"backfill_floor": 1000000000}`)
}

// useAnvilAt registers an EVM connector on url, an endpoint of the local chain c, and sets the source anvil to the
// deployment of c and the values of config, a JSON object. rpc_rate_limit is 1000000 unless config sets it: the
// limiter waits on the fake clock, and the runs of a test send more requests than 5 per second. The config is
// restored at the end of the test.
func (h *harness) useAnvilAt(t *testing.T, c *testchain.Chain, url, config string) {
	t.Helper()
	h.set.RegisterEVM(evm.New([]uint64{31337, 84532}, map[string]evm.Endpoints{
		"anvil": {Primary: vault.NewSecret(url)},
	}, nil, h.logger))
	var saved []byte
	if err := h.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.exec(t, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved) })
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('controller_address', $1::text,
		'rpc_rate_limit', 1000000) || $2::jsonb WHERE code = 'anvil'`, c.Controller.Hex(), config)
	h.exec(t, `INSERT INTO asset_aliases (source_id, native_asset, asset, decimals)
		SELECT id, $1, 'USDC', $2 FROM sources WHERE code = 'anvil'`, c.Token.Hex(), testchain.TokenDecimals)
}

// balancesOf is GetBalances of one connection: its freshness and its balances.
func (h *harness) balancesOf(t *testing.T, id uuid.UUID) (*casv1.ConnectionSnapshot, []*casv1.Balance) {
	t.Helper()
	client, callCtx := h.dataClient(t)
	resp, err := client.GetBalances(callCtx, &casv1.GetBalancesRequest{
		Selector: &casv1.GetBalancesRequest_ConnectionId{ConnectionId: id.String()},
	})
	if err != nil || len(resp.GetConnections()) != 1 {
		t.Fatalf("GetBalances: %v, %d connections", err, len(resp.GetConnections()))
	}
	return resp.GetConnections()[0], resp.GetBalances()
}

// wantBalances checks the balances of GetBalances: one USDC row of the token per connection.
func wantBalances(t *testing.T, got []*casv1.Balance, token common.Address, free ...string) {
	t.Helper()
	if len(got) != len(free) {
		t.Fatalf("%d balances, want %d", len(got), len(free))
	}
	for i, b := range got {
		if b.GetAccountType() != casv1.AccountType_ACCOUNT_TYPE_WALLET || b.GetAsset() != "USDC" ||
			b.GetNativeAsset() != token.Hex() || b.GetFree() != free[i] || b.GetLocked() != "0" {
			t.Errorf("balance %d: %v, want WALLET USDC %s free %s locked 0", i, b, token.Hex(), free[i])
		}
	}
}

func (h *harness) snapshots(t *testing.T, id uuid.UUID) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM balance_snapshots WHERE connection_id = $1`, id)
}

// headTime is the time of the latest block of the local chain.
func headTime(t *testing.T, c *testchain.Chain) time.Time {
	t.Helper()
	var head struct {
		Time hexutil.Uint64 `json:"timestamp"`
	}
	c.Call(t, &head, "eth_getBlockByNumber", "latest", false)
	return time.Unix(int64(head.Time), 0).UTC()
}

// S3-T301 — Req: FR-303. On Anvil: W holds 12.5 USDC, a second wallet 0. One run of the engine: GetBalances shows one
// row per tracked token for each, the zero included, WALLET, all in free, locked 0, as of the head block.
func TestT301_SnapshotOfEveryTrackedToken(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	h.useAnvil(t, c)
	w := c.NewWallet(t)
	c.Mint(t, w, big.NewInt(12_500_000))
	idW := h.walletAt(t, w)
	idZero := h.walletAt(t, common.HexToAddress("0x00000000000000000000000000000000000Ab301"))

	h.pass(t, h.engine(h.server))
	at := headTime(t, c)
	for id, free := range map[uuid.UUID]string{idW: "12.5", idZero: "0"} {
		if cur := h.cursor(t, id, "balances"); cur.failures != 0 || cur.lastSuccessAt == nil {
			t.Fatalf("balances of %s: %+v, want a success", free, cur)
		}
		conn, balances := h.balancesOf(t, id)
		if conn.GetStale() || conn.GetAsOf() == nil || !conn.GetAsOf().AsTime().Equal(at) {
			t.Errorf("connection of %s: %v, want as of the head block %s, not stale", free, conn, at)
		}
		wantBalances(t, balances, c.Token, free)
	}
}

// S3-T302, the engine part — Req: FR-304. as_of of GetBalances is the time of the block the balances were read at,
// not the time of the run.
func TestT302_AsOfIsTheBlockTime(t *testing.T) {
	h := setup(t)
	blockAt := testNow.Add(-97 * time.Second)
	node := h.useBalanceNode(t, blockAt)
	w := common.HexToAddress("0x00000000000000000000000000000000000Ab302")
	node.set(func(n *balanceNode) { n.balances[w] = big.NewInt(29_866_387) })
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	conn, balances := h.balancesOf(t, id)
	if conn.GetAsOf() == nil || !conn.GetAsOf().AsTime().Equal(blockAt) || conn.GetStale() {
		t.Errorf("connection %v, want as_of %s", conn, blockAt)
	}
	wantBalances(t, balances, evmToken, "29.866387")
}

// S3-T303, the engine part — Req: FR-303, EC-304; Core FR-109. A snapshot exists; balanceOf fails with a JSON-RPC
// error, then with HTTP 500: each run fails, no snapshot is written, GetBalances returns the previous one, stale once
// the last success is older than stale_after (2 × 15 min).
func TestT303_PreviousSnapshotStays(t *testing.T) {
	h := setup(t)
	node := h.useBalanceNode(t, testNow.Add(-time.Minute))
	w := common.HexToAddress("0x00000000000000000000000000000000000Ab303")
	node.set(func(n *balanceNode) { n.balances[w] = big.NewInt(7_000_000) })
	id := h.walletAt(t, w)
	e := h.engine(h.server)
	h.pass(t, e)
	if n := h.snapshots(t, id); n != 1 {
		t.Fatalf("%d snapshots after the first run, want 1", n)
	}
	firstAsOf := testNow.Add(-time.Minute)

	node.set(func(n *balanceNode) { n.balances[w], n.time = big.NewInt(9_000_000), testNow.Add(14*time.Minute) })
	for i, mode := range []string{failRPC, failHTTP} {
		node.set(func(n *balanceNode) { n.fail = mode })
		h.clock.Advance(15 * time.Minute)
		h.pass(t, e)
		cur := h.cursor(t, id, "balances")
		if cur.failures != i+1 || cur.lastError == nil || !strings.Contains(*cur.lastError, "EVM_RPC_URL_ANVIL: eth_call") {
			t.Errorf("%s: balances %+v, want failure %d of eth_call", mode, cur, i+1)
		}
		if n := h.snapshots(t, id); n != 1 {
			t.Errorf("%s: %d snapshots, want the previous one only", mode, n)
		}
		conn, balances := h.balancesOf(t, id)
		// The last success was 15 min ago, then 30 min: not older than stale_after yet.
		if conn.GetStale() || !conn.GetAsOf().AsTime().Equal(firstAsOf) {
			t.Errorf("%s: connection %v, want the previous snapshot, not stale", mode, conn)
		}
		wantBalances(t, balances, evmToken, "7")
	}

	h.clock.Advance(time.Second)
	conn, balances := h.balancesOf(t, id)
	if !conn.GetStale() || !conn.GetAsOf().AsTime().Equal(firstAsOf) {
		t.Errorf("after stale_after: connection %v, want the previous snapshot, stale", conn)
	}
	wantBalances(t, balances, evmToken, "7")
}

// S3-T305, the engine part — Req: UC-302 trigger; Core FR-120. A wallet connection is due at its creation; the run
// sets the next one 15 min later, or sync_interval.balances of sources.config; TriggerSync makes it due and runs it.
func TestT305_FirstRunAtCreationAndTriggerSync(t *testing.T) {
	h := setup(t)
	node := h.useBalanceNode(t, testNow)
	created := h.clock.Now()
	id := h.walletAt(t, common.HexToAddress("0x00000000000000000000000000000000000Ab305"))
	if cur := h.cursor(t, id, "balances"); cur.mode != "INCREMENTAL" || !cur.nextRunAt.Equal(created) {
		t.Fatalf("balances at creation: %+v, want INCREMENTAL, due at %s", cur, created)
	}

	e := h.engine(h.server)
	h.pass(t, e)
	if cur := h.cursor(t, id, "balances"); node.headReads() != 1 || h.snapshots(t, id) != 1 ||
		!cur.nextRunAt.Equal(created.Add(15*time.Minute)) {
		t.Fatalf("first run: %d head reads, %d snapshots, next %s; want 1, 1, %s", node.headReads(), h.snapshots(t, id),
			cur.nextRunAt, created.Add(15*time.Minute))
	}

	// An override of the interval, then TriggerSync before the next run is due.
	h.exec(t, `UPDATE sources SET config = config || '{"sync_interval": {"balances": "20m"}}' WHERE code = 'anvil'`)
	h.clock.Advance(2 * time.Minute)
	api, callCtx := h.apiClient(t)
	if _, err := api.TriggerSync(callCtx, &casv1.TriggerSyncRequest{ConnectionId: id.String()}); err != nil {
		t.Fatal(err)
	}
	if cur := h.cursor(t, id, "balances"); !cur.nextRunAt.Equal(h.clock.Now()) {
		t.Errorf("after TriggerSync: next %s, want now %s", cur.nextRunAt, h.clock.Now())
	}
	h.pass(t, e)
	if cur := h.cursor(t, id, "balances"); node.headReads() != 2 || h.snapshots(t, id) != 2 ||
		!cur.nextRunAt.Equal(h.clock.Now().Add(20*time.Minute)) {
		t.Errorf("triggered run: %d head reads, %d snapshots, next %s; want 2, 2, %s", node.headReads(),
			h.snapshots(t, id), cur.nextRunAt, h.clock.Now().Add(20*time.Minute))
	}
}

// S3-T307 — Req: EC-319; Core EC-117; S3 D-37. On Anvil W is minted a balance with more than 20 integer digits: the
// connector returns it as read, the ledger writer refuses the snapshot as a whole, the run fails, the previous
// snapshot is returned and becomes stale.
func TestT307_OversizedBalance(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	h.useAnvil(t, c)
	w := c.NewWallet(t)
	c.Mint(t, w, big.NewInt(12_500_000))
	id := h.walletAt(t, w)
	e := h.engine(h.server)
	h.pass(t, e)
	firstAsOf := headTime(t, c)
	if n := h.snapshots(t, id); n != 1 {
		t.Fatalf("%d snapshots after the first run, want 1", n)
	}

	// 10^27 base units more: 10^21 USDC, 22 integer digits.
	c.Mint(t, w, new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil))
	h.clock.Advance(15 * time.Minute)
	h.pass(t, e)
	cur := h.cursor(t, id, "balances")
	if cur.failures != 1 || cur.lastError == nil || !strings.Contains(*cur.lastError, "20 integer digits") {
		t.Errorf("balances %+v, want a failure refused by the ledger writer", cur)
	}
	if n := h.snapshots(t, id); n != 1 {
		t.Errorf("%d snapshots, want the previous one only", n)
	}
	conn, balances := h.balancesOf(t, id)
	if conn.GetStale() || !conn.GetAsOf().AsTime().Equal(firstAsOf) {
		t.Errorf("connection %v, want the previous snapshot, not stale yet", conn)
	}
	wantBalances(t, balances, c.Token, "12.5")

	h.clock.Advance(15*time.Minute + time.Second)
	conn, balances = h.balancesOf(t, id)
	if !conn.GetStale() || !conn.GetAsOf().AsTime().Equal(firstAsOf) {
		t.Errorf("after stale_after: connection %v, want the previous snapshot, stale", conn)
	}
	wantBalances(t, balances, c.Token, "12.5")
}
