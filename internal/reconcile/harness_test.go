package reconcile_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/connector/evm"
	"github.com/DigitLock/crypto-account-service/internal/engine"
	"github.com/DigitLock/crypto-account-service/internal/limiter"
	"github.com/DigitLock/crypto-account-service/internal/reconcile"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
	"github.com/DigitLock/crypto-account-service/internal/testdb"
	"github.com/DigitLock/crypto-account-service/internal/vault"
)

var ctx = context.Background()

// clock is the clock of the engine: it moves when the test moves it, and a wait moves it to the time waited for.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Wait(_ context.Context, until time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.After(c.now) {
		c.now = until
	}
	return nil
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// reporter is the Reporter of the engine: nothing is reported in these tests.
type reporter struct{}

func (reporter) RunFinished(string, string, bool)         {}
func (reporter) EntriesInserted(string, int)              {}
func (reporter) DuplicatesSkipped(string, int)            {}
func (reporter) UnmappedAsset(string, string)             {}
func (reporter) BudgetWaited(string, time.Duration)       {}
func (reporter) RateLimited(string)                       {}
func (reporter) Connections(map[string]int)               {}
func (reporter) Staleness(map[string]time.Duration)       {}
func (reporter) LedgerGap(string, string, string, string) {}

// harness is the common precondition of phase 6 (docs/test-plan-s3.md §2): the test database, Anvil with MockUSDC
// and the controller, the source anvil set to that deployment, the platform tenant cas-platform with the treasury
// connection T named by set-treasury, and an engine on the pool of cas_server that syncs T. Rows of
// authorizations, returns and operator_txs are written by the tests with the owner role.
type harness struct {
	owner, server *pgxpool.Pool
	reg           *registry.Registry
	c             *testchain.Chain
	clock         *clock
	engine        *engine.Engine
	treasury      uuid.UUID
	sourceID      int16
	tenants       map[string]uuid.UUID
	nonce         int64
	base          time.Time
}

func setup(t *testing.T) *harness {
	t.Helper()
	h := &harness{owner: testdb.Open(t), clock: &clock{now: time.Now().UTC()}, tenants: map[string]uuid.UUID{}}
	testdb.Clean(t)
	h.server = testdb.OpenServer(t)
	h.c = testchain.Start(t)
	h.reg = registry.New(h.owner)
	h.base = time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	set := connector.NewSet()
	set.RegisterEVM(evm.New([]uint64{31337, 84532}, map[string]evm.Endpoints{
		"anvil": {Primary: vault.NewSecret(h.c.RPCURL)},
	}, nil, logger))
	var saved []byte
	if err := h.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.exec(t, `UPDATE sources SET config = $1 WHERE code = 'anvil'`, saved) })
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('controller_address', $1::text,
		'backfill_floor', 0, 'rpc_rate_limit', 1000000) WHERE code = 'anvil'`, h.c.Controller.Hex())
	h.exec(t, `INSERT INTO asset_aliases (source_id, native_asset, asset, decimals)
		SELECT id, $1, 'USDC', $2 FROM sources WHERE code = 'anvil'`, h.c.Token.Hex(), testchain.TokenDecimals)
	if err := h.owner.QueryRow(ctx, `SELECT id FROM sources WHERE code = 'anvil'`).Scan(&h.sourceID); err != nil {
		t.Fatal(err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(key, 1)
	if err != nil {
		t.Fatal(err)
	}
	limiters := limiter.NewSet(h.clock, nil)
	conns := registry.NewConnections(h.server, v, set, limiters, h.clock.Now, 50*time.Millisecond, time.Minute)

	platform := h.tenant(t, "cas-platform")
	tok, err := h.reg.IssueToken(ctx, "cas-platform")
	if err != nil {
		t.Fatal(err)
	}
	var credentialID uuid.UUID
	if err := h.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	conn, err := conns.Create(ctx, registry.CreateInput{
		TenantID: platform, CredentialID: credentialID, OwnerRef: "treasury", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: h.c.Treasury.Hex()},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.treasury = conn.ID
	if _, _, err := h.reg.SetTreasury(ctx, "anvil", conn.ID); err != nil {
		t.Fatal(err)
	}
	h.c.ApproveRefunds(t, units(1_000_000))

	h.engine = engine.New(engine.Config{
		MaxPagesPerRun: 50, FailureThreshold: 5, BackoffInitial: 30 * time.Second, BackoffMax: time.Hour,
		Workers: 2, Tick: time.Second, LockRetry: 10 * time.Second, KeyCheckInterval: 24 * time.Hour,
	}, engine.Deps{
		DB: h.server, Vault: v, Connectors: set, Limiters: limiters, Locker: engine.PGLocker{Pool: h.server},
		Clock: h.clock, Reporter: reporter{}, Logger: logger,
	})
	return h
}

func (h *harness) exec(t *testing.T, stmt string, args ...any) {
	t.Helper()
	if _, err := h.owner.Exec(ctx, stmt, args...); err != nil {
		t.Fatal(err)
	}
}

// units is n whole USDC in base units.
func units(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)) }

// tenant returns the ID of a tenant, created on first use.
func (h *harness) tenant(t *testing.T, name string) uuid.UUID {
	t.Helper()
	if id, ok := h.tenants[name]; ok {
		return id
	}
	tn, err := h.reg.CreateTenant(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	h.tenants[name] = tn.ID
	return tn.ID
}

// chainID is keccak256 of the 16 bytes of the tenant UUID and the UTF-8 bytes of the processor's ID: authId and
// refundId of SRS — Card Spend §2.1.5 (D-16).
func (h *harness) chainID(t *testing.T, tenant, id string) [32]byte {
	t.Helper()
	tid := h.tenant(t, tenant)
	return crypto.Keccak256Hash(tid[:], []byte(id))
}

func hexOf(b [32]byte) string { return hexutil.Encode(b[:]) }

// at is the time of the n-th authorization of a test: an hour ago plus n seconds, before any block of Anvil.
func (h *harness) at(n int) time.Time { return h.base.Add(time.Duration(n) * time.Second) }

// authorization writes an authorization of tenant on the chain of anvil, as card-auth stores it, and returns its row ID.
func (h *harness) authorization(t *testing.T, tenant, authID, status string, amount *big.Int, receivedAt, validUntil time.Time) uuid.UUID {
	t.Helper()
	chainAuthID := h.chainID(t, tenant, authID)
	var id uuid.UUID
	if err := h.owner.QueryRow(ctx, `INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, token, token_amount,
		wallet_address, chain_id, status, received_at, valid_until)
		VALUES ($1, $2, $3, 'USDC', $4::numeric, $5, $6, $7, $8, $9) RETURNING id`,
		h.tenants[tenant], authID, chainAuthID[:], amount.String(), common.Address{0xa6}.Bytes(), testchain.ChainID,
		status, receivedAt, validUntil).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// returnRow writes a REVERSAL of tenant for the authorization row and returns its row ID.
func (h *harness) returnRow(t *testing.T, tenant string, authRow uuid.UUID, returnID, status string, amount *big.Int) uuid.UUID {
	t.Helper()
	return h.returnRowOfType(t, tenant, authRow, returnID, "REVERSAL", status, amount)
}

// returnRowOfType writes a return of a type of SRS — Card Spend §2.4 and returns its row ID.
func (h *harness) returnRowOfType(t *testing.T, tenant string, authRow uuid.UUID, returnID, kind, status string, amount *big.Int) uuid.UUID {
	t.Helper()
	chainRefundID := h.chainID(t, tenant, returnID)
	var id uuid.UUID
	if err := h.owner.QueryRow(ctx, `INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type,
		token_amount, status) VALUES ($1, $2, $3, $4, $5, $6::numeric, $7) RETURNING id`,
		h.tenants[tenant], authRow, returnID, chainRefundID[:], kind, amount.String(), status).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// operatorTx writes a transaction of card-auth: purpose DEBIT for an authorization row, REFUND for a return row.
// block 0 is no block.
func (h *harness) operatorTx(t *testing.T, purpose string, row uuid.UUID, hash common.Hash, status string, block uint64) {
	t.Helper()
	h.nonce++
	var authRow, returnRow *uuid.UUID
	if purpose == "DEBIT" {
		authRow = &row
	} else {
		returnRow = &row
	}
	var blockNumber *int64
	if block > 0 {
		b := int64(block)
		blockNumber = &b
	}
	h.exec(t, `INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, return_row_id,
		tx_hash, status, block_number) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		testchain.ChainID, h.c.Operator.Bytes(), h.nonce, purpose, authRow, returnRow, hash.Bytes(), status, blockNumber)
}

// wallet is a cardholder wallet with 1000 USDC, a daily limit and an allowance to the controller.
func (h *harness) wallet(t *testing.T) common.Address {
	t.Helper()
	w := h.c.NewWallet(t)
	h.c.Mint(t, w, units(1000))
	h.c.SetDailyLimit(t, w, units(1000))
	h.c.Approve(t, w, units(1000))
	return w
}

// debit sends a debit of the operator on Anvil and returns its hash and block.
func (h *harness) debit(t *testing.T, w common.Address, amount *big.Int, authID [32]byte) (common.Hash, uint64) {
	t.Helper()
	hash := h.c.Debit(t, w, amount, authID)
	return hash, h.blockOf(t, hash)
}

// refund sends a refund of the operator on Anvil and returns its hash and block.
func (h *harness) refund(t *testing.T, authID, refundID [32]byte, amount *big.Int) (common.Hash, uint64) {
	t.Helper()
	hash := h.c.Refund(t, authID, refundID, amount)
	return hash, h.blockOf(t, hash)
}

func (h *harness) blockOf(t *testing.T, hash common.Hash) uint64 {
	t.Helper()
	var receipt struct {
		BlockNumber hexutil.Uint64 `json:"blockNumber"`
	}
	h.c.Call(t, &receipt, "eth_getTransactionReceipt", hash)
	return uint64(receipt.BlockNumber)
}

// headTime is the time of the latest block.
func (h *harness) headTime(t *testing.T) time.Time {
	t.Helper()
	var block struct {
		Timestamp hexutil.Uint64 `json:"timestamp"`
	}
	h.c.Call(t, &block, "eth_getBlockByNumber", "latest", false)
	return time.Unix(int64(block.Timestamp), 0).UTC()
}

// sync makes every operation so far final and syncs T to a checkpoint: the first pass reads up to the final block,
// three more blocks move it, and the next run of the logs stream, an hour later on the clock of the engine, ends at
// the final block with a checkpoint.
func (h *harness) sync(t *testing.T) {
	t.Helper()
	h.c.MineBlocks(t, 10)
	h.pass(t)
	h.c.MineBlocks(t, 3)
	h.clock.Advance(time.Hour)
	h.pass(t)
	var failures int
	if err := h.owner.QueryRow(ctx, `SELECT consecutive_failures FROM sync_cursors WHERE connection_id = $1 AND stream = 'logs'`,
		h.treasury).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("logs of T: %d failures, %v", failures, err)
	}
}

func (h *harness) pass(t *testing.T) {
	t.Helper()
	if err := h.engine.RunPass(ctx); err != nil {
		t.Fatal(err)
	}
}

// reconcile reconciles anvil with the pool of cas_server, the role of the worker of server.
func (h *harness) reconcile(t *testing.T) []reconcile.Run {
	t.Helper()
	runs, err := reconcile.Source(ctx, h.server, "anvil")
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// runOf returns the run of a tenant.
func runOf(t *testing.T, runs []reconcile.Run, tenant string) reconcile.Run {
	t.Helper()
	for _, r := range runs {
		if r.Tenant == tenant {
			return r
		}
	}
	t.Fatalf("no run of %s in %d runs", tenant, len(runs))
	return reconcile.Run{}
}

// cursor returns next_block − 1 and last_time of the logs cursor of T.
func (h *harness) cursor(t *testing.T) (int64, time.Time) {
	t.Helper()
	var toBlock int64
	var lastTime string
	if err := h.owner.QueryRow(ctx, `SELECT (cursor ->> 'next_block')::bigint - 1, cursor ->> 'last_time'
		FROM sync_cursors WHERE connection_id = $1 AND stream = 'logs'`, h.treasury).Scan(&toBlock, &lastTime); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, lastTime)
	if err != nil {
		t.Fatal(err)
	}
	return toBlock, at.UTC()
}

// randomHash is a transaction hash of no transaction, generated at run time.
func randomHash(t *testing.T) common.Hash {
	t.Helper()
	var h common.Hash
	if _, err := rand.Read(h[:]); err != nil {
		t.Fatal(err)
	}
	return h
}
