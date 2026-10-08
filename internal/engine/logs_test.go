package engine_test

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/google/uuid"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	casv1 "github.com/DigitLock/crypto-account-service/internal/grpc/pb/cas/v1"
	"github.com/DigitLock/crypto-account-service/internal/registry"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// usdc is n tenths of a USDC in base units: usdc(105) is 10.5 USDC.
func usdc(tenths int64) *big.Int { return big.NewInt(tenths * 100_000) }

// fromZero is the config of the log tests: the backfill starts at block 0.
const fromZero = `{"backfill_floor": 0}`

// logEntry is a stored entry of the logs stream.
type logEntry struct {
	seq                                 int64
	externalID, groupID, kind, leg, dir string
	asset, native, amount               string
	occurredAt                          time.Time
	raw                                 map[string]any
}

func (e logEntry) String() string { return e.kind + " " + e.dir + " " + e.amount }

func (h *harness) logEntries(t *testing.T, id uuid.UUID) []logEntry {
	t.Helper()
	rows, err := h.owner.Query(ctx, `SELECT seq, external_id, group_id, type, leg, direction, asset, native_asset,
		trim_scale(amount)::text, occurred_at, raw FROM ledger_entries WHERE connection_id = $1 AND stream = 'logs' ORDER BY seq`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []logEntry
	for rows.Next() {
		var e logEntry
		var raw []byte
		if err := rows.Scan(&e.seq, &e.externalID, &e.groupID, &e.kind, &e.leg, &e.dir, &e.asset, &e.native, &e.amount,
			&e.occurredAt, &raw); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &e.raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// wantEntries checks the entries of a connection as "TYPE DIRECTION amount", in seq order, all SINGLE of USDC.
func wantEntries(t *testing.T, got []logEntry, token common.Address, want ...string) {
	t.Helper()
	var kinds []string
	for _, e := range got {
		kinds = append(kinds, e.String())
		if e.leg != "SINGLE" || e.asset != "USDC" || e.native != token.Hex() {
			t.Errorf("entry %s: leg %s, asset %s, native %s; want SINGLE USDC %s", e, e.leg, e.asset, e.native, token.Hex())
		}
	}
	if strings.Join(kinds, ", ") != strings.Join(want, ", ") {
		t.Errorf("entries [%s], want [%s]", strings.Join(kinds, ", "), strings.Join(want, ", "))
	}
}

// final mines the blocks that make every operation so far final under confirmations 10.
func final(t *testing.T, c *testchain.Chain) { c.MineBlocks(t, 10) }

// treasuryConnection creates the treasury connection of c in the platform tenant cas-platform and names it in
// treasury_connection and treasury_address of anvil, as casctl source set-treasury does.
func (h *harness) treasuryConnection(t *testing.T, c *testchain.Chain) uuid.UUID {
	t.Helper()
	tn, err := h.reg.CreateTenant(ctx, "cas-platform")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := h.reg.IssueToken(ctx, "cas-platform")
	if err != nil {
		t.Fatal(err)
	}
	var credentialID uuid.UUID
	if err := h.owner.QueryRow(ctx, `SELECT id FROM api_credentials WHERE key_id = $1`, tok.KeyID).Scan(&credentialID); err != nil {
		t.Fatal(err)
	}
	conn, err := h.conns.Create(ctx, registry.CreateInput{
		TenantID: tn.ID, CredentialID: credentialID, OwnerRef: "treasury", Source: "anvil",
		Credentials: connector.Credentials{WalletAddress: c.Treasury.Hex()},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE sources SET config = config || jsonb_build_object('treasury_connection', $1::text,
		'treasury_address', $2::text) WHERE code = 'anvil'`, conn.ID.String(), c.Treasury.Hex())
	return conn.ID
}

// S3-T401 — Req: FR-308. W receives 10 USDC and sends 3: DEPOSIT IN 10 and WITHDRAWAL OUT 3, leg SINGLE.
func TestT401_TransfersInAndOut(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	c.Transfer(t, w, common.HexToAddress("0x00000000000000000000000000000000000A4011"), usdc(30))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	if cur := h.cursor(t, id, "logs"); cur.failures != 0 || cur.mode != "INCREMENTAL" {
		t.Fatalf("logs %+v, want a success up to the final block", cur)
	}
	wantEntries(t, h.logEntries(t, id), c.Token, "DEPOSIT IN 10", "WITHDRAWAL OUT 3")
}

// S3-T402 — Req: FR-308. W mints 100 and burns 1: DEPOSIT 100 from the zero address, WITHDRAWAL 1 to it.
func TestT402_MintAndBurn(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w := c.NewWallet(t)
	c.Mint(t, w, usdc(1000))
	c.Burn(t, w, usdc(10))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	es := h.logEntries(t, id)
	wantEntries(t, es, c.Token, "DEPOSIT IN 100", "WITHDRAWAL OUT 1")
	zero := common.Address{}.Hex()
	if len(es) == 2 {
		if args := es[0].raw["args"].(map[string]any); args["from"] != zero || args["to"] != w.Hex() {
			t.Errorf("mint args %v, want from the zero address to W", args)
		}
		if args := es[1].raw["args"].(map[string]any); args["from"] != w.Hex() || args["to"] != zero {
			t.Errorf("burn args %v, want from W to the zero address", args)
		}
	}
}

// S3-T403 — Req: FR-308. A transfer of W to itself and a transfer of 0 to W give no entry.
func TestT403_SelfAndZeroTransfers(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, w, usdc(50))
	c.Transfer(t, w, w, usdc(20))
	c.Transfer(t, x, w, big.NewInt(0))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	if cur := h.cursor(t, id, "logs"); cur.failures != 0 {
		t.Fatalf("logs %+v", cur)
	}
	wantEntries(t, h.logEntries(t, id), c.Token, "DEPOSIT IN 5")
}

// debited mints 50 USDC to a new wallet and debits 20 of it through the controller.
func debited(t *testing.T, c *testchain.Chain) (common.Address, [32]byte) {
	t.Helper()
	w := c.NewWallet(t)
	c.Mint(t, w, usdc(500))
	c.SetDailyLimit(t, w, usdc(10000))
	c.Approve(t, w, usdc(500))
	auth := [32]byte{0xa4, 0x04}
	c.Debit(t, w, usdc(200), auth)
	return w, auth
}

// S3-T404 — Req: FR-307. A debit of W: W one CARD_DEBIT OUT, the treasury connection one CARD_DEBIT IN; the
// Transfer of the debit gives no WITHDRAWAL of W and no DEPOSIT of T.
func TestT404_DebitPairedWithItsTransfer(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, auth := debited(t, c)
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	treasury := h.treasuryConnection(t, c)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	wallet, tre := h.logEntries(t, id), h.logEntries(t, treasury)
	wantEntries(t, wallet, c.Token, "DEPOSIT IN 50", "CARD_DEBIT OUT 20")
	wantEntries(t, tre, c.Token, "CARD_DEBIT IN 20")
	if len(wallet) == 2 && len(tre) == 1 {
		if wallet[1].externalID != tre[0].externalID || wallet[1].raw["event"] != "Debited" {
			t.Errorf("W %s and T %s: want the same Debited log", wallet[1].externalID, tre[0].externalID)
		}
		args := tre[0].raw["args"].(map[string]any)
		if args["authId"] != common.Hash(auth).Hex() || args["user"] != w.Hex() || args["amount"] != "20000000" {
			t.Errorf("args %v", args)
		}
	}
}

// S3-T405 — Req: FR-307. A refund to W: W one CARD_REFUND IN, T one CARD_REFUND OUT; the paired Transfer gives none.
func TestT405_RefundPairedWithItsTransfer(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, auth := debited(t, c)
	c.ApproveRefunds(t, usdc(500))
	refund := [32]byte{0xa4, 0x05}
	c.Refund(t, auth, refund, usdc(50))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	treasury := h.treasuryConnection(t, c)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	wantEntries(t, h.logEntries(t, id), c.Token, "DEPOSIT IN 50", "CARD_DEBIT OUT 20", "CARD_REFUND IN 5")
	tre := h.logEntries(t, treasury)
	wantEntries(t, tre, c.Token, "CARD_DEBIT IN 20", "CARD_REFUND OUT 5")
	if len(tre) == 2 {
		if args := tre[1].raw["args"].(map[string]any); args["refundId"] != common.Hash(refund).Hex() {
			t.Errorf("refund args %v", args)
		}
	}
}

// recordingProxy forwards JSON-RPC requests to target and keeps their bodies.
type recordingProxy struct {
	mu     sync.Mutex
	bodies []string
}

func newRecordingProxy(t *testing.T, target string) (*recordingProxy, string) {
	t.Helper()
	p := &recordingProxy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, string(body))
		p.mu.Unlock()
		resp, err := http.Post(target, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			http.Error(w, "proxy", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(srv.Close)
	return p, srv.URL
}

func (p *recordingProxy) all() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.bodies, "\n")
}

// S3-T408 — Req: EC-312. A second ERC-20 without an alias row moves tokens to W: no request names it, no entry.
func TestT408_UntrackedToken(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	other := c.DeployToken(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.MintOf(t, other, x, usdc(70))
	c.TransferOf(t, other, x, w, usdc(70))
	final(t, c)
	proxy, url := newRecordingProxy(t, c.RPCURL)
	h.useAnvilAt(t, c, url, fromZero)
	id := h.walletAt(t, w)

	h.pass(t, h.engine(h.server))
	if cur := h.cursor(t, id, "logs"); cur.failures != 0 || cur.mode != "INCREMENTAL" {
		t.Fatalf("logs %+v", cur)
	}
	sent := strings.ToLower(proxy.all())
	if !strings.Contains(sent, "eth_getlogs") || !strings.Contains(sent, strings.ToLower(c.Token.Hex()[2:])) {
		t.Fatal("the run sent no eth_getLogs of the tracked token")
	}
	if strings.Contains(sent, strings.ToLower(other.Hex()[2:])) {
		t.Errorf("a request names the untracked token %s", other.Hex())
	}
	wantEntries(t, h.logEntries(t, id), c.Token)
}

var externalID = regexp.MustCompile(`^0x[0-9a-f]{64}:[0-9]+$`)

// S3-T410 — Req: FR-310, §2.4; Core FR-113. The entries of T401 through ListLedgerEntries and raw in the database.
func TestT410_EntryFields(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)
	h.pass(t, h.engine(h.server))

	client, callCtx := h.dataClient(t)
	resp, err := client.ListLedgerEntries(callCtx, &casv1.ListLedgerEntriesRequest{ConnectionId: id.String()})
	if err != nil || len(resp.GetEntries()) != 1 {
		t.Fatalf("ListLedgerEntries: %v, %d entries", err, len(resp.GetEntries()))
	}
	got, stored := resp.GetEntries()[0], h.logEntries(t, id)[0]
	raw := stored.raw
	var block struct {
		Time hexutil.Uint64 `json:"timestamp"`
	}
	c.Call(t, &block, "eth_getBlockByHash", raw["blockHash"], false)
	index, err := hexutil.DecodeUint64(raw["logIndex"].(string))
	if err != nil {
		t.Fatal(err)
	}
	want := raw["transactionHash"].(string) + ":" + itoa(int(index))
	switch {
	case !externalID.MatchString(got.GetExternalId()) || got.GetExternalId() != want:
		t.Errorf("external_id %s, want %s", got.GetExternalId(), want)
	case got.GetGroupId() != "logs:"+want:
		t.Errorf("group_id %s", got.GetGroupId())
	case !got.GetOccurredAt().AsTime().Equal(time.Unix(int64(block.Time), 0)):
		t.Errorf("occurred_at %s, want the block time %d", got.GetOccurredAt().AsTime(), block.Time)
	case got.GetAsset() != "USDC" || got.GetNativeAsset() != c.Token.Hex() || got.GetAmount() != "10" ||
		got.GetType() != casv1.LedgerEntryType_LEDGER_ENTRY_TYPE_DEPOSIT ||
		got.GetDirection() != casv1.LedgerDirection_LEDGER_DIRECTION_IN || got.GetLeg() != casv1.LedgerLeg_LEDGER_LEG_SINGLE:
		t.Errorf("entry %v", got)
	}
	for _, key := range []string{"address", "blockNumber", "blockHash", "transactionHash", "logIndex", "topics", "data", "removed"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("raw has no %s: %v", key, raw)
		}
	}
	args, _ := raw["args"].(map[string]any)
	if raw["event"] != "Transfer" || args["from"] != x.Hex() || args["to"] != w.Hex() || args["value"] != "10000000" ||
		!strings.EqualFold(raw["address"].(string), c.Token.Hex()) {
		t.Errorf("raw event %v, args %v", raw["event"], args)
	}
}

// S3-T411 — Req: FR-310, EC-309; Core FR-106. The cursor reset to backfill_floor: the repeated range adds no entry,
// the duplicates are counted, seq is unchanged.
func TestT411_RepeatedRange(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	c.Transfer(t, w, x, usdc(30))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)
	e := h.engine(h.server)
	h.pass(t, e)
	before := h.logEntries(t, id)
	if len(before) != 2 {
		t.Fatalf("%d entries after the first run, want 2", len(before))
	}

	h.exec(t, `UPDATE sync_cursors SET cursor = '{}', mode = 'BACKFILL', next_run_at = $2
		WHERE connection_id = $1 AND stream = 'logs'`, id, h.clock.Now())
	skipped := h.rec.snapshot().skipped
	h.pass(t, e)
	after := h.logEntries(t, id)
	if len(after) != 2 || after[0].seq != before[0].seq || after[1].seq != before[1].seq {
		t.Errorf("entries %v after the repeat, want the same seq %d, %d", after, before[0].seq, before[1].seq)
	}
	if got := h.rec.snapshot().skipped - skipped; got != 2 {
		t.Errorf("%d duplicates skipped, want 2", got)
	}
	if cur := h.cursor(t, id, "logs"); cur.failures != 0 || cur.mode != "INCREMENTAL" {
		t.Errorf("logs %+v", cur)
	}
}

// logCursor is the logs cursor of a connection.
type logCursor struct {
	NextBlock *uint64 `json:"next_block"`
	LastHash  string  `json:"last_hash"`
	LastTime  string  `json:"last_time"`
}

func (h *harness) logCursor(t *testing.T, id uuid.UUID) (logCursor, string) {
	t.Helper()
	cur := h.cursor(t, id, "logs")
	var c logCursor
	if err := json.Unmarshal([]byte(cur.cursor), &c); err != nil {
		t.Fatal(err)
	}
	return c, cur.mode
}

// backfillChain is a chain with a deposit to W every few blocks and a head far above them.
func backfillChain(t *testing.T) (*testchain.Chain, common.Address, int) {
	t.Helper()
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(1000))
	deposits := 0
	for range 4 {
		c.Transfer(t, x, w, usdc(10))
		deposits++
		c.MineBlocks(t, 6)
	}
	c.MineBlocks(t, 20)
	return c, w, deposits
}

// S3-T414 — Req: UC-303 basic flow; Core UC-102. Pages of log_range_max blocks; BACKFILL until the first page that
// ends at F, then INCREMENTAL; at most SYNC_MAX_PAGES_PER_RUN pages per run.
func TestT414_BackfillToIncremental(t *testing.T) {
	h := setup(t)
	h.cfg.MaxPagesPerRun = 3
	c, w, deposits := backfillChain(t)
	h.useAnvilAt(t, c, c.RPCURL, `{"backfill_floor": 0, "log_range_max": 5}`)
	id := h.walletAt(t, w)
	f := c.Head(t) - 10
	e := h.engine(h.server)

	next := uint64(0)
	for pass := 1; ; pass++ {
		if pass > 20 {
			t.Fatal("the backfill did not end")
		}
		h.pass(t, e)
		cur, mode := h.logCursor(t, id)
		if cur.NextBlock == nil || *cur.NextBlock <= next {
			t.Fatalf("pass %d: cursor %+v did not move from %d", pass, cur, next)
		}
		want := min(next+15, f+1) // three pages of five blocks
		if *cur.NextBlock != want {
			t.Errorf("pass %d: next_block %d, want %d", pass, *cur.NextBlock, want)
		}
		next = *cur.NextBlock
		if next <= f {
			if mode != "BACKFILL" {
				t.Errorf("pass %d: mode %s below F, want BACKFILL", pass, mode)
			}
			continue
		}
		if mode != "INCREMENTAL" {
			t.Errorf("pass %d: mode %s at F, want INCREMENTAL", pass, mode)
		}
		break
	}
	if n := len(h.logEntries(t, id)); n != deposits {
		t.Errorf("%d entries, want %d", n, deposits)
	}
}

// S3-T415 — Req: EC-314; Core FR-108. A backfill stopped after two pages: the next engine continues from the
// committed cursor; no entry twice.
func TestT415_StopInABackfill(t *testing.T) {
	h := setup(t)
	h.cfg.MaxPagesPerRun = 2
	c, w, deposits := backfillChain(t)
	h.useAnvilAt(t, c, c.RPCURL, `{"backfill_floor": 0, "log_range_max": 5}`)
	id := h.walletAt(t, w)
	f := c.Head(t) - 10

	h.pass(t, h.engine(h.server))
	cur, mode := h.logCursor(t, id)
	if *cur.NextBlock != 10 || mode != "BACKFILL" {
		t.Fatalf("after the stop: next_block %d, mode %s; want 10, BACKFILL", *cur.NextBlock, mode)
	}
	stored := len(h.logEntries(t, id))

	// The restart: a new engine without the page limit.
	h.cfg.MaxPagesPerRun = 50
	skipped := h.rec.snapshot().skipped
	h.pass(t, h.engine(h.server))
	cur, mode = h.logCursor(t, id)
	if *cur.NextBlock != f+1 || mode != "INCREMENTAL" {
		t.Errorf("after the restart: next_block %d, mode %s; want %d, INCREMENTAL", *cur.NextBlock, mode, f+1)
	}
	es := h.logEntries(t, id)
	seen := map[string]bool{}
	for _, e := range es {
		if seen[e.externalID] {
			t.Errorf("%s stored twice", e.externalID)
		}
		seen[e.externalID] = true
	}
	if len(es) != deposits || stored == 0 || h.rec.snapshot().skipped != skipped {
		t.Errorf("%d entries (%d before the restart), %d skipped; want %d, none read twice", len(es), stored,
			h.rec.snapshot().skipped-skipped, deposits)
	}
}

// S3-T416 — Req: §2.4 Cursor formats; S3 D-7. The cursor of T401: none of last_hash and last_time before the first
// run; then the last processed block + 1, its hash and its time.
func TestT416_CursorOfTheLogsStream(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w, x := c.NewWallet(t), c.NewWallet(t)
	c.Mint(t, x, usdc(100))
	c.Transfer(t, x, w, usdc(100))
	final(t, c)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)
	if cur := h.cursor(t, id, "logs"); cur.cursor != "{}" || cur.mode != "BACKFILL" {
		t.Fatalf("before the first run: %+v, want {} in BACKFILL", cur)
	}
	f := c.Head(t) - 10

	h.pass(t, h.engine(h.server))
	var block struct {
		Hash common.Hash    `json:"hash"`
		Time hexutil.Uint64 `json:"timestamp"`
	}
	c.Call(t, &block, "eth_getBlockByNumber", hexutil.EncodeUint64(f), false)
	var generic map[string]any
	cur := h.cursor(t, id, "logs")
	if err := json.Unmarshal([]byte(cur.cursor), &generic); err != nil || len(generic) != 3 {
		t.Fatalf("cursor %s, want three members", cur.cursor)
	}
	got, _ := h.logCursor(t, id)
	wantTime := time.Unix(int64(block.Time), 0).UTC().Format(time.RFC3339)
	if *got.NextBlock != f+1 || got.LastHash != block.Hash.Hex() || got.LastTime != wantTime {
		t.Errorf("cursor %s, want next_block %d, last_hash %s, last_time %s", cur.cursor, f+1, block.Hash.Hex(), wantTime)
	}
}

// S3-T417 — Req: Core FR-122, EC-119. A wallet connection created by the code of v0.3.0 has no cursors: the start
// of the engine creates balances and logs, both due at once, and runs them.
func TestT417_CursorsOfExistingWallets(t *testing.T) {
	h := setup(t)
	c := testchain.Start(t)
	w := c.NewWallet(t)
	h.useAnvilAt(t, c, c.RPCURL, fromZero)
	id := h.walletAt(t, w)
	h.exec(t, `DELETE FROM sync_cursors WHERE connection_id = $1`, id)
	h.clock.Advance(time.Hour)

	e := h.engine(h.server)
	e.CreateMissingCursors(ctx)
	for stream, mode := range map[string]string{"balances": "INCREMENTAL", "logs": "BACKFILL"} {
		cur := h.cursor(t, id, stream)
		if cur.mode != mode || cur.cursor != "{}" || !cur.nextRunAt.Equal(h.clock.Now()) {
			t.Errorf("%s: %+v, want mode %s, cursor {}, due now", stream, cur, mode)
		}
	}
	h.pass(t, e)
	for _, stream := range []string{"balances", "logs"} {
		if cur := h.cursor(t, id, stream); cur.failures != 0 || cur.lastSuccessAt == nil {
			t.Errorf("%s: %+v, want a success", stream, cur)
		}
	}
}
