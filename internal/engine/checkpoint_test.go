package engine_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// checkpointRow is a row of balance_checkpoints.
type checkpointRow struct {
	asset, hash               string
	block                     int64
	balance, ledgerTotal, gap string
	takenAt                   time.Time
}

func (h *harness) checkpointOf(t *testing.T, id uuid.UUID, native string) (checkpointRow, bool) {
	t.Helper()
	var c checkpointRow
	err := h.owner.QueryRow(ctx, `SELECT asset, block_hash, block_number, trim_scale(balance)::text,
		trim_scale(ledger_total)::text, trim_scale(gap)::text, taken_at FROM balance_checkpoints
		WHERE connection_id = $1 AND native_asset = $2`, id, native).
		Scan(&c.asset, &c.hash, &c.block, &c.balance, &c.ledgerTotal, &c.gap, &c.takenAt)
	if err != nil {
		return checkpointRow{}, false
	}
	return c, true
}

// toCheckpoint runs the engine twice: the first pass backfills the logs streams to F (INCREMENTAL, no checkpoint);
// three more blocks make F move, and the next run of the logs streams, five minutes later, ends at F with a checkpoint.
func (h *harness) toCheckpoint(t *testing.T, c *testchain.Chain) {
	t.Helper()
	e := h.engine(h.server)
	h.pass(t, e)
	c.MineBlocks(t, 3)
	h.clock.Advance(6 * time.Minute)
	h.pass(t, e)
}

// wantGap checks the checkpoint of a connection: the token, block F, the balance, the gap and the metric.
func (h *harness) wantGap(t *testing.T, c *testchain.Chain, id uuid.UUID, balance, gap string) {
	t.Helper()
	cp, ok := h.checkpointOf(t, id, c.Token.Hex())
	if !ok {
		t.Fatalf("no checkpoint of %s", id)
	}
	f := c.Head(t) - 10
	if cp.asset != "USDC" || cp.block != int64(f) || cp.balance != balance || cp.gap != gap {
		t.Errorf("checkpoint %+v, want USDC at block %d, balance %s, gap %s", cp, f, balance, gap)
	}
	if got, ok := h.rec.gap("anvil", id.String(), "USDC"); !ok || got != gap {
		t.Errorf("ledger_gap = %q (%v), want %s", got, ok, gap)
	}
	if logs := h.cursor(t, id, "logs"); logs.failures != 0 || logs.mode != "INCREMENTAL" {
		t.Errorf("logs %+v", logs)
	}
}

// S3-T502 — Req: FR-315; Core §2.4, §2.5; S3 D-5, S3 D-19. W receives 10 and sends 3: at the checkpoint the row of W
// holds block F, balance 7, ledger total 7, gap 0; ledger_gap 0.
func TestT502_GapComputedAndStored(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	c.Transfer(t, w, x, usdc(30))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)

	h.toCheckpoint(t, c)
	h.wantGap(t, c, id, "7", "0")
	cp, _ := h.checkpointOf(t, id, c.Token.Hex())
	if cp.ledgerTotal != "7" || !strings.HasPrefix(cp.hash, "0x") || len(cp.hash) != 66 || cp.takenAt.IsZero() {
		t.Errorf("checkpoint %+v", cp)
	}
}

// S3-T503 — Req: FR-315. backfill_floor set after a mint of 50: the ledger misses it; gap 50 stored and in the metric,
// where its alert condition (any non-zero) holds.
func TestT503_GapFound(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, w, usdc(500))
	floor := c.Head(t) + 1
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, `{"backfill_floor": `+itoa(int(floor))+`}`)
	id := h.walletAt(t, w)

	h.toCheckpoint(t, c)
	h.wantGap(t, c, id, "60", "50")
	if cp, _ := h.checkpointOf(t, id, c.Token.Hex()); cp.ledgerTotal != "10" {
		t.Errorf("ledger total %s, want 10", cp.ledgerTotal)
	}
}

// tenantClient creates a tenant with a service token and returns the ConnectionService of cmd/server as that tenant.
func (h *harness) tenantClient(t *testing.T, name string) (casv1.ConnectionServiceClient, context.Context) {
	t.Helper()
	if _, err := h.reg.CreateTenant(ctx, name); err != nil {
		t.Fatal(err)
	}
	tok, err := h.reg.IssueToken(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return casv1.NewConnectionServiceClient(h.apiConn(t, nil)),
		metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok.Value)
}

// S3-T505, the API and engine part — Req: §2.4; Core UC-101, UC-105 row 9; S3 D-1, S3 D-2, S3 D-22, S3 D-32. The
// treasury connection created through the API in tenant cas-platform, named by set-treasury (the registry call of
// casctl): ACTIVE, both cursors, CONNECTION_CREATED; both keys written; its first run passes the start check
// treasury. The command line is shown in cmd/casctl.
func TestT505_TreasuryConnectionCreated(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	client, callCtx := h.tenantClient(t, "cas-platform")
	resp, err := client.CreateConnection(callCtx, &casv1.CreateConnectionRequest{OwnerRef: "treasury", Source: "anvil",
		Label: "Treasury", Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: strings.ToLower(c.Treasury.Hex())}}})
	if err != nil {
		t.Fatal(err)
	}
	conn := resp.GetConnection()
	id := uuid.MustParse(conn.GetConnectionId())
	if conn.GetStatus() != casv1.ConnectionStatus_CONNECTION_STATUS_ACTIVE || conn.GetWalletAddress() != c.Treasury.Hex() ||
		conn.GetKind() != casv1.ConnectionKind_CONNECTION_KIND_EVM_WALLET {
		t.Errorf("connection %v", conn)
	}
	if n := h.count(t, `SELECT count(*) FROM sync_cursors WHERE connection_id = $1 AND stream IN ('balances', 'logs')`, id); n != 2 {
		t.Errorf("%d cursors, want balances and logs", n)
	}
	if n := h.count(t, `SELECT count(*) FROM audit_log a JOIN tenants t ON t.id = a.tenant_id
		WHERE t.name = 'cas-platform' AND a.action = 'CONNECTION_CREATED' AND a.object_id = $1`, id.String()); n != 1 {
		t.Errorf("%d audit rows CONNECTION_CREATED, want 1", n)
	}

	previous, current, err := h.reg.SetTreasury(ctx, "anvil", id)
	if err != nil {
		t.Fatal(err)
	}
	if previous != (registry.Treasury{}) || current.Connection != id.String() || current.Address != c.Treasury.Hex() {
		t.Errorf("set-treasury: previous %+v, current %+v", previous, current)
	}
	var config map[string]any
	if err := h.owner.QueryRow(ctx, `SELECT config FROM sources WHERE code = 'anvil'`).Scan(&config); err != nil {
		t.Fatal(err)
	}
	if config["treasury_connection"] != id.String() || config["treasury_address"] != c.Treasury.Hex() {
		t.Errorf("config %v", config)
	}

	h.pass(t, h.engine(h.server))
	if logs := h.cursor(t, id, "logs"); logs.failures != 0 || logs.lastSuccessAt == nil {
		t.Errorf("logs of the treasury %+v, want a success after the start check treasury", logs)
	}
}

// tenantWallet creates a wallet connection of another tenant, which needs no API token.
func (h *harness) tenantWallet(t *testing.T, tenant string, address common.Address) uuid.UUID {
	t.Helper()
	tn, err := h.reg.CreateTenant(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.reg.IssueToken(ctx, tenant)
	if err != nil {
		t.Fatal(err)
	}
	var credentialID uuid.UUID
	if err := h.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	conn, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: tn.ID, CredentialID: credentialID, OwnerRef: "owner-b", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: address.Hex()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn.ID
}

// S3-T507 — Req: §2.1.1 Roles, §2.1.2 filters. The treasury connection sees the debits and refunds of W (tenant A)
// and of V (tenant B), and its own deposit and withdrawal.
func TestT507_TreasurySeesEveryEvent(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, auth := debited(t, c)
	v := c.NewWallet(t)
	c.Mint(t, v, usdc(300))
	c.SetDailyLimit(t, v, usdc(10000))
	c.Approve(t, v, usdc(300))
	authV := [32]byte{0xa5, 0x07}
	c.Debit(t, v, usdc(100), authV)
	c.ApproveRefunds(t, usdc(1000))
	c.Refund(t, auth, [32]byte{0xa5, 0x01}, usdc(50))
	c.Refund(t, authV, [32]byte{0xa5, 0x02}, usdc(30))
	c.Mint(t, c.Treasury, usdc(70))
	c.Transfer(t, c.Treasury, common.HexToAddress("0x00000000000000000000000000000000000A5070"), usdc(40))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	treasury := h.treasuryConnection(t, c)
	h.walletAt(t, w)
	h.tenantWallet(t, "tenant-b", v)

	h.pass(t, h.engine(h.server))
	wantEntries(t, h.logEntries(t, treasury), c.Token,
		"CARD_DEBIT IN 20", "CARD_DEBIT IN 10", "CARD_REFUND OUT 5", "CARD_REFUND OUT 3", "DEPOSIT IN 7", "WITHDRAWAL OUT 4")
}

// operations makes the scenario of FR-317 on c: mint, transfer in, transfer out, debit and refund of W; final.
func operations(t *testing.T, c *testchain.Chain) common.Address {
	t.Helper()
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(1000))
	c.Transfer(t, x, w, usdc(100))
	c.Mint(t, w, usdc(500))
	c.Transfer(t, w, x, usdc(30))
	c.SetDailyLimit(t, w, usdc(10000))
	c.Approve(t, w, usdc(200))
	auth := [32]byte{0xa5, 0x08}
	c.Debit(t, w, usdc(200), auth)
	c.ApproveRefunds(t, usdc(50))
	c.Refund(t, auth, [32]byte{0xa5, 0x09}, usdc(50))
	final(t, c)
	return w
}

// S3-T508 — Req: FR-315. After the scenario both W and the treasury connection reach a checkpoint with gap 0.
func TestT508_ZeroGapWalletAndTreasury(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w := operations(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	treasury := h.treasuryConnection(t, c)
	id := h.walletAt(t, w)

	h.toCheckpoint(t, c)
	// W: 10 + 50 − 3 − 20 + 5 = 42. T: 20 − 5 = 15.
	h.wantGap(t, c, id, "42", "0")
	h.wantGap(t, c, treasury, "15", "0")
}

// S3-T509 — Req: Card Spend UC-4 rule 5; S3 D-5. The checkpoint of the treasury connection is readable with the role
// of server, at the block of its logs cursor or below.
func TestT509_TreasuryCheckpointReadableByServer(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	operations(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	treasury := h.treasuryConnection(t, c)
	h.toCheckpoint(t, c)

	var block int64
	var gap string
	if err := h.server.QueryRow(ctx, `SELECT block_number, trim_scale(gap)::text FROM balance_checkpoints
		WHERE connection_id = $1 AND native_asset = $2`, treasury, c.Token.Hex()).Scan(&block, &gap); err != nil {
		t.Fatalf("read with cas_server: %v", err)
	}
	cur, _ := h.logCursor(t, treasury)
	if cur.NextBlock == nil || block > int64(*cur.NextBlock)-1 || gap != "0" {
		t.Errorf("checkpoint at block %d, gap %s; the logs cursor is at %v", block, gap, cur.NextBlock)
	}
}

// S3-T510, on Anvil — Req: EC-319, EC-317; S3 D-39. W holds 10^20 USDC from a mint (21 integer digits; the mint log
// itself is skipped by EC-315), then receives 10. The run that reaches the checkpoint commits the transfer and the
// cursor; no balance_checkpoints row of W; the stream is not failed.
func TestT510_OversizedBalanceDoesNotStopTheImport(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, w, new(big.Int).Exp(big.NewInt(10), big.NewInt(26), nil))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)
	e := h.engine(h.server)
	h.pass(t, e) // backfill to F: INCREMENTAL, no checkpoint

	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	final(t, c)
	h.clock.Advance(6 * time.Minute)
	h.pass(t, e) // INCREMENTAL to F: the checkpoint is due and skipped

	wantEntries(t, h.logEntries(t, id), c.Token, "DEPOSIT IN 10")
	logs := h.cursor(t, id, "logs")
	cur, _ := h.logCursor(t, id)
	if logs.failures != 0 || logs.mode != "INCREMENTAL" || cur.NextBlock == nil || *cur.NextBlock != c.Head(t)-10+1 {
		t.Errorf("logs %+v, cursor %v; want a success up to F", logs, cur.NextBlock)
	}
	if n := h.count(t, `SELECT count(*) FROM balance_checkpoints WHERE connection_id = $1`, id); n != 0 {
		t.Errorf("%d checkpoint rows of W, want none", n)
	}
	if _, ok := h.rec.gap("anvil", id.String(), "USDC"); ok {
		t.Error("ledger_gap reported for W")
	}
}

// S3-T511, on Anvil through the engine — Req: §2.1.1 Roles; FR-307; S3 D-42. A connection of the treasury address is
// created through the API while the source has its endpoint and a Debited is final on chain. Before set-treasury its
// logs runs fail with the check treasury: no entry, the cursor {}, one WARN line. After set-treasury (the registry
// call of casctl) the next run of the same engine, without a restart, imports CARD_DEBIT IN.
func TestT511_TreasuryAddressGuardOnAnvil(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	debited(t, c)
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	client, callCtx := h.tenantClient(t, "cas-platform")
	resp, err := client.CreateConnection(callCtx, &casv1.CreateConnectionRequest{OwnerRef: "treasury", Source: "anvil",
		Label: "Treasury", Credential: &casv1.CreateConnectionRequest_Wallet{Wallet: &casv1.Wallet{Address: c.Treasury.Hex()}}})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(resp.GetConnection().GetConnectionId())

	e := h.engine(h.server)
	for run := 1; run <= 2; run++ {
		h.pass(t, e)
		logs := h.cursor(t, id, "logs")
		if logs.failures != run || logs.lastError == nil || !strings.Contains(*logs.lastError, "start check treasury failed") ||
			logs.cursor != "{}" {
			t.Fatalf("run %d: logs %+v; want failure %d with the check treasury, cursor {}", run, logs, run)
		}
		if n := h.count(t, `SELECT count(*) FROM ledger_entries WHERE connection_id = $1`, id); n != 0 {
			t.Fatalf("run %d: %d entries of the unnamed treasury connection", run, n)
		}
		h.clock.Advance(h.cfg.BackoffMax)
	}
	if n := strings.Count(h.log.String(), "EVM logs run refused"); n != 1 ||
		!strings.Contains(h.log.String(), "casctl source set-treasury anvil "+id.String()) {
		t.Errorf("%d WARN lines of the guard, want 1 with the hint:\n%s", n, h.log.String())
	}

	if _, _, err := h.reg.SetTreasury(ctx, "anvil", id); err != nil {
		t.Fatal(err)
	}
	h.pass(t, e)
	if logs := h.cursor(t, id, "logs"); logs.failures != 0 || logs.lastSuccessAt == nil {
		t.Errorf("logs after set-treasury %+v, want a success", logs)
	}
	wantEntries(t, h.logEntries(t, id), c.Token, "CARD_DEBIT IN 20")
}
