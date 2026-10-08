package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/connector"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// anvilRig is a connector on a local chain with the contracts deployed; the source anvil holds the addresses of
// that deployment and the treasury connection.
type anvilRig struct {
	*rig
	chain *testchain.Chain
	rpc   *rpc.Client
}

func newAnvilRig(t *testing.T) *anvilRig {
	t.Helper()
	c := testchain.Start(t)
	client, err := rpc.Dial(c.RPCURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return &anvilRig{rig: newRig(c.RPCURL, ""), chain: c, rpc: client}
}

func (a *anvilRig) source(finality string) connector.Source {
	config := fmt.Sprintf(`{"chain_id": 31337, %s, "controller_address": %q, "backfill_floor": 0,
		"treasury_connection": %q, "treasury_address": %q}`, finality, a.chain.Controller.Hex(), fxTreasuryConnection,
		a.chain.Treasury.Hex())
	decimals := int16(testchain.TokenDecimals)
	return connector.Source{Code: "anvil", Kind: connector.KindEVM, Enabled: true, Config: json.RawMessage(config),
		Aliases: []connector.Alias{{NativeAsset: a.chain.Token.Hex(), Asset: "USDC", Decimals: &decimals}}}
}

func (a *anvilRig) call(t *testing.T, result any, method string, args ...any) {
	t.Helper()
	if err := a.rpc.CallContext(context.Background(), result, method, args...); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

func (a *anvilRig) head(t *testing.T) uint64 {
	t.Helper()
	var n hexutil.Uint64
	a.call(t, &n, "eth_blockNumber")
	return uint64(n)
}

func (a *anvilRig) mine(t *testing.T, blocks uint64) {
	t.Helper()
	a.call(t, nil, "anvil_mine", hexutil.Uint64(blocks))
}

func (a *anvilRig) header(t *testing.T, block string) *header {
	t.Helper()
	var h *header
	a.call(t, &h, "eth_getBlockByNumber", block, false)
	return h
}

// S3-T201 — Req: §2.1.1 Finality rule. Mode tag with finalized: the number of the header of finalized.
func TestT201_FinalBlockTag(t *testing.T) {
	a := newAnvilRig(t)
	a.mine(t, 100)
	want := a.header(t, "finalized")
	if want == nil {
		t.Fatal("anvil has no finalized block")
	}
	page, err := a.logs(a.conn("w", a.source(`"finality_mode": "tag", "finality_tag": "finalized"`)), `{}`)
	if err != nil || *cursorOf(t, page).NextBlock != uint64(want.Number)+1 {
		t.Errorf("run: %v, page %+v; want the range up to %d", err, page, uint64(want.Number))
	}
	if got := a.m.final["anvil"]; got != uint64(want.Number) {
		t.Errorf("evm_final_block = %d, want %d", got, uint64(want.Number))
	}
	if failed, set := a.m.check("anvil", CheckTreasury); failed || !set {
		t.Error("the treasury check did not pass on the deployment")
	}
}

// S3-T202 — Req: §2.1.1 Finality rule. Mode confirmations: below 10 blocks no block is final and the run reads
// nothing without failing; above, the final block is head − 10.
func TestT202_FinalBlockConfirmations(t *testing.T) {
	a := newAnvilRig(t)
	src := a.source(`"finality_mode": "confirmations", "finality_confirmations": 10`)
	conn := a.conn("w", src)
	if head := a.head(t); head >= 10 {
		t.Fatalf("the deployment left %d blocks; the test needs fewer than 10", head)
	}
	page, err := a.logs(conn, `{}`)
	if err != nil || page.More || len(page.Entries) != 0 || string(page.Cursor) != `{}` || page.Mode != connector.ModeBackfill {
		t.Fatalf("run below 10 blocks: %v, page %+v; want a successful run without entries and the same cursor", err, page)
	}

	a.mine(t, 20)
	final := a.head(t) - 10
	page, err = a.logs(conn, `{}`)
	if err != nil || *cursorOf(t, page).NextBlock != final+1 || page.Mode != connector.ModeIncremental {
		t.Errorf("run above: %v, page %+v; want the range 0 to %d", err, page, final)
	}
	if a.m.final["anvil"] != final {
		t.Errorf("evm_final_block = %d, want %d", a.m.final["anvil"], final)
	}
}

// S3-T204 — Req: FR-312, EC-308. The hash of block next_block − 1 changed (evm_revert and other blocks): each run
// fails with REORG_BELOW_FINAL, the metric grows by 1 per hit, the log line holds both hashes. That the cursor and
// the stored data stay is shown in internal/engine.
func TestT204_ChangedHashBelowCursor(t *testing.T) {
	a := newAnvilRig(t)
	var snapshot hexutil.Big
	a.call(t, &snapshot, "evm_snapshot")
	a.mine(t, 30)
	stored := a.header(t, hexutil.EncodeUint64(20))
	a.call(t, nil, "evm_revert", &snapshot)
	a.call(t, nil, "evm_increaseTime", hexutil.Uint64(100))
	a.mine(t, 30)
	found := a.header(t, hexutil.EncodeUint64(20))
	if found == nil || found.Hash == stored.Hash {
		t.Fatal("the reorg left block 20 unchanged")
	}

	conn := a.conn("w", a.source(`"finality_mode": "confirmations", "finality_confirmations": 10`))
	cursor := cursorJSON(21, stored.Hash)
	for run := 1; run <= 2; run++ {
		page, err := a.logs(conn, cursor)
		var reorg *ReorgError
		if !errors.As(err, &reorg) || reorg.Block != 20 || reorg.Found != found.Hash.Hex() || page.Cursor != nil {
			t.Fatalf("run %d: %v; want REORG_BELOW_FINAL of block 20", run, err)
		}
		if a.m.reorgs["anvil"] != run {
			t.Errorf("run %d: evm_reorg_below_final_total = %d", run, a.m.reorgs["anvil"])
		}
	}
	log := a.log.String()
	if !strings.Contains(log, stored.Hash.Hex()) || !strings.Contains(log, found.Hash.Hex()) ||
		!strings.Contains(log, "critical: REORG_BELOW_FINAL") {
		t.Errorf("no log line with both hashes:\n%s", log)
	}
}
