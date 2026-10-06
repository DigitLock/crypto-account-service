package processorapi_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/crs/crstest"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// Phase 4 of docs/test-plan-s2.md, the part of st5b: UC-1 steps 10 to 13 on Anvil with the Debit step of
// card-auth. Every transaction goes to the local chain 31337 of testchain.

var debitedTopic = crypto.Keccak256Hash([]byte("Debited(bytes32,address,uint256)"))

// debitedFor returns the number of Debited events of the authorizations of tenant A with these auth_ids.
func (e *env) debitedFor(t *testing.T, authIDs ...string) int {
	t.Helper()
	n := 0
	for _, l := range e.debitedLogs(t) {
		for _, id := range authIDs {
			if l == decision.ChainAuthID(e.tenantA, id) {
				n++
			}
		}
	}
	return n
}

// debitedLogs returns the authIds of the Debited events of the controller.
func (e *env) debitedLogs(t *testing.T) []common.Hash {
	t.Helper()
	var logs []struct {
		Topics []common.Hash `json:"topics"`
	}
	e.chain.Call(t, &logs, "eth_getLogs", map[string]any{
		"address": e.chain.Controller, "topics": []any{debitedTopic}, "fromBlock": "0x0", "toBlock": "latest",
	})
	out := make([]common.Hash, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.Topics[1])
	}
	return out
}

// opTx is an operator_txs row.
type opTx struct {
	Nonce       int64
	Status      string
	TxHash      []byte
	BlockNumber *int64
	BlockHash   []byte
	Purpose     string
}

func (e *env) opTxs(t *testing.T, authID string) []opTx {
	t.Helper()
	rows, err := e.owner.Query(ctx, `SELECT t.nonce, t.status, t.tx_hash, t.block_number, t.block_hash, t.purpose
		FROM operator_txs t JOIN authorizations a ON a.id = t.authorization_id
		WHERE a.tenant_id = $1 AND a.auth_id = $2 ORDER BY t.nonce`, e.tenantA, authID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[opTx])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) balance(t *testing.T, addr common.Address) *big.Int {
	t.Helper()
	var out hexutil.Bytes
	e.chain.Call(t, &out, "eth_call", map[string]any{
		"to": e.chain.Token, "data": hexutil.Bytes(testchain.TokenCalldata(t, "balanceOf", addr)),
	}, "latest")
	return new(big.Int).SetBytes(out)
}

func approvedWith(t *testing.T, r response, tokenAmount string) {
	t.Helper()
	if r.status != http.StatusOK || r.str("decision") != decision.Approved || r.str("status") != decision.StatusApproved ||
		r.str("token") != "USDC" || r.str("token_amount") != tokenAmount || len(r.str("tx_hash")) != 66 {
		t.Errorf("answer %d %s, want 200 APPROVED with USDC %s and a tx_hash", r.status, r.raw, tokenAmount)
	}
}

// S2-T403 — Req: UC-1, FR-2, FR-11, UC-3 row 10
func TestT403_ApproveUSD(t *testing.T) {
	c := testchain.Start(t)

	t.Run("sealed receipt", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true})
		before := e.balance(t, e.wallet)
		r := e.authorize(t, authReq("auth-403", "card_A", "25.40", "USD"))
		approvedWith(t, r, "25400000")
		if _, ok := r.body["quote"]; ok {
			t.Errorf("a USD approval has a quote: %s", r.raw)
		}
		want := decision.ChainAuthID(e.tenantA, "auth-403")
		if logs := e.debitedLogs(t); len(logs) != 1 || logs[0] != want {
			t.Errorf("Debited events %v, want one for %s", logs, want.Hex())
		}
		if spent := new(big.Int).Sub(before, e.balance(t, e.wallet)); spent.String() != "25400000" {
			t.Errorf("the wallet paid %s", spent)
		}
		row := e.row(t, e.tenantA, "auth-403")
		if row.Status != decision.StatusApproved || row.DecidedAt == nil ||
			strings.Join(row.Events, " ") != ">RECEIVED: RECEIVED>DEBIT_SUBMITTED: DEBIT_SUBMITTED>APPROVED:" {
			t.Errorf("row %s, decided_at %v, history %v", row.Status, row.DecidedAt, row.Events)
		}
		if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = 'auth-403' AND debited_amount = 25400000`); n != 1 {
			t.Error("debited_amount is not the token amount")
		}
		txs := e.opTxs(t, "auth-403")
		if len(txs) != 1 || txs[0].Status != "INCLUDED" || txs[0].Purpose != "DEBIT" || "0x"+hex.EncodeToString(txs[0].TxHash) != r.str("tx_hash") ||
			txs[0].BlockNumber == nil || len(txs[0].BlockHash) != 32 {
			t.Errorf("operator_txs %+v, want one INCLUDED DEBIT with the hash of the answer and its block", txs)
		}
		if got := e.decisions(t, decision.Approved, ""); got != 1 {
			t.Errorf("auth_decisions_total{APPROVED} = %v", got)
		}
		if got := e.signals(t, "polling"); got != 1 {
			t.Errorf("inclusion_signals_total{polling} = %v, want 1", got)
		}
		get := e.get(t, "auth-403")
		if get.str("tx_hash") != r.str("tx_hash") || get.str("debited_amount") != "25400000" || get.str("status") != "APPROVED" {
			t.Errorf("status query %s", get.raw)
		}
	})

	t.Run("preconfirmed receipt: zero block hash counts, no block stored", func(t *testing.T) {
		p := newRPCProxy(t, c.RPCURL)
		p.zeroBlockHash = true
		e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
		approvedWith(t, e.authorize(t, authReq("auth-403-pre", "card_A", "1", "USD")), "1000000")
		if txs := e.opTxs(t, "auth-403-pre"); len(txs) != 1 || txs[0].Status != "INCLUDED" || txs[0].BlockNumber != nil || txs[0].BlockHash != nil {
			t.Errorf("operator_txs %+v, want INCLUDED without block number and hash", txs)
		}
	})
}

// S2-T404 — Req: UC-1 step 7, FR-7. The response part.
func TestT404_ApproveEUR(t *testing.T) {
	e := newEnv(t, options{realDebit: true})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})
	r := e.authorize(t, authReq("auth-404", "card_A", "25.40", "EUR"))
	approvedWith(t, r, "29866387")
	if r.str("quote", "rate") != "1.1642" || r.str("quote", "buffer_bps") != "100" {
		t.Errorf("quote of %s, want rate 1.1642 and buffer_bps 100", r.raw)
	}
	if spent := e.count(t, `SELECT count(*) FROM authorizations WHERE debited_amount = 29866387`); spent != 1 {
		t.Error("debited_amount is not 29866387")
	}
}

// S2-T413 — Req: FR-3, EC-1
func TestT413_SameBody(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	first := e.authorize(t, authReq("auth-413", "card_A", "25.40", "USD"))
	approvedWith(t, first, "25400000")
	for range 2 {
		if r := e.authorize(t, `{"currency":"USD","amount":"25.4","card_ref":"card_A","auth_id":"auth-413"}`); !bytes.Equal(r.raw, first.raw) {
			t.Errorf("answer %s, want the stored decision %s", r.raw, first.raw)
		}
	}
	if n := len(e.debitedLogs(t)); n != 1 {
		t.Errorf("%d Debited events", n)
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations`); n != 1 {
		t.Errorf("%d rows", n)
	}
	if got := e.decisions(t, decision.Approved, ""); got != 1 || e.observations(t) != 1 {
		t.Errorf("auth_decisions_total %v, observations %d; want 1 and 1", got, e.observations(t))
	}
}

// S2-T415 — Req: §3.2 Parallel work, FR-3
func TestT415_SameAuthIDAtOnce(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	e.chain.SetIntervalMining(t, 1)
	body := mustJSON(t, authReq("auth-415", "card_A", "5", "USD"))
	answers := make([]response, 20)
	var wg sync.WaitGroup
	for i := range answers {
		wg.Add(1)
		go func() { defer wg.Done(); answers[i] = e.authorize(t, body) }()
	}
	wg.Wait()
	for _, a := range answers {
		if !bytes.Equal(a.raw, answers[0].raw) || a.status == http.StatusConflict {
			t.Errorf("answer %d %s, want the one decision %s", a.status, a.raw, answers[0].raw)
		}
	}
	approvedWith(t, answers[0], "5000000")
	if t.Failed() {
		t.Logf("log:\n%s", e.logs.String())
	}
	if n := len(e.debitedLogs(t)); n != 1 {
		t.Errorf("%d Debited events, want 1", n)
	}
	if got := e.decisions(t, decision.Approved, ""); got != 1 {
		t.Errorf("auth_decisions_total = %v", got)
	}
}

// eventIDs returns the id of the event to_status of an authorization.
func (e *env) eventID(t *testing.T, authID, toStatus string) int64 {
	t.Helper()
	var id int64
	if err := e.owner.QueryRow(ctx, `SELECT ev.id FROM authorization_events ev JOIN authorizations a ON a.id = ev.authorization_id
		WHERE a.auth_id = $1 AND ev.to_status = $2`, authID, toStatus).Scan(&id); err != nil {
		t.Fatalf("event %s of %s: %v", toStatus, authID, err)
	}
	return id
}

// S2-T416 — Req: FR-10, EC-13
func TestT416_PerCardLock(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true, deadline: 4 * time.Second, debitValidity: 5 * time.Second})
	walletB := e.chain.NewWallet(t)
	e.chain.Mint(t, walletB, usdc(t, "100"))
	e.chain.Approve(t, walletB, usdc(t, "100"))
	e.chain.SetDailyLimit(t, walletB, usdc(t, "50"))
	e.addCard(t, e.tenantA, "card_B", walletB, usdc(t, "200"))
	e.chain.SetAutomine(t, false)
	e.chain.SetIntervalMining(t, 1)

	var wg sync.WaitGroup
	answers := map[string]response{}
	var mu sync.Mutex
	for _, r := range []struct{ id, card string }{{"auth-416-a1", "card_A"}, {"auth-416-a2", "card_A"}, {"auth-416-b", "card_B"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := e.authorize(t, authReq(r.id, r.card, "2", "USD"))
			mu.Lock()
			answers[r.id] = a
			mu.Unlock()
		}()
	}
	wg.Wait()
	for id, a := range answers {
		if a.str("decision") != decision.Approved {
			t.Errorf("%s: %s, want APPROVED", id, a.raw)
		}
	}
	// card_A: the second is submitted only after the first is approved. card_B runs beside the first of card_A.
	type span struct{ submitted, decided int64 }
	spans := map[string]span{}
	for _, id := range []string{"auth-416-a1", "auth-416-a2", "auth-416-b"} {
		spans[id] = span{e.eventID(t, id, decision.StatusDebitSubmitted), e.eventID(t, id, decision.StatusApproved)}
	}
	first, second := spans["auth-416-a1"], spans["auth-416-a2"]
	if first.submitted > second.submitted {
		first, second = second, first
	}
	if second.submitted < first.decided {
		t.Errorf("card_A: the second was submitted (event %d) before the first was decided (event %d)", second.submitted, first.decided)
	}
	if b := spans["auth-416-b"]; b.submitted > first.decided {
		t.Errorf("card_B was submitted (event %d) after the first of card_A was decided (event %d): not in parallel", b.submitted, first.decided)
	}
}

// slowDB delays every statement whose SQL contains match.
type slowDB struct {
	*pgxpool.Pool
	match string
	delay time.Duration
}

func (d slowDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return slowTx{Tx: tx, d: d}, nil
}

type slowTx struct {
	pgx.Tx
	d slowDB
}

func (tx slowTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, tx.d.match) {
		time.Sleep(tx.d.delay)
	}
	return tx.Tx.Exec(ctx, sql, args...)
}

// S2-T417 — Req: UC-1 steps 5 and 9a, FR-1, D-20. Two authorizations of one card at once, mining stopped: the
// first holds the lock of the card through step 13 and times out with its debit sent; the second gets the lock,
// if at all, with less than min_send_window left and is declined before step 10: no nonce, nothing sent.
func TestT417_LockNotAcquired(t *testing.T) {
	c := testchain.Start(t)

	t.Run("less than min_send_window left", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true})
		e.chain.SetAutomine(t, false)
		defer e.chain.SetAutomine(t, true)
		before := e.chain.TxCount(t, e.chain.Operator)
		start := make(chan struct{})
		answers := make([]response, 2)
		took := make([]time.Duration, 2)
		var wg sync.WaitGroup
		for i := range answers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				t0 := time.Now()
				answers[i] = e.authorize(t, authReq("auth-417-"+string(rune('a'+i)), "card_A", "5", "USD"))
				took[i] = time.Since(t0)
			}()
		}
		close(start)
		wg.Wait()

		var timedOut, declined int
		for i, a := range answers {
			id := "auth-417-" + string(rune('a'+i))
			if took[i] > 2550*time.Millisecond {
				t.Errorf("%s answered after %s, the deadline is 2.5 s", id, took[i])
			}
			switch e.row(t, e.tenantA, id).Status {
			case decision.StatusTimedOut:
				timedOut++
				declinedWith(t, a, decision.StatusTimedOut, decision.ReasonTimeout)
			case decision.StatusDeclined:
				declined++
				declinedWith(t, a, decision.StatusDeclined, decision.ReasonTimeout)
				if txs := e.opTxs(t, id); len(txs) != 0 {
					t.Errorf("%s reserved a nonce: %+v", id, txs)
				}
			}
		}
		if timedOut != 1 || declined != 1 {
			t.Errorf("%d TIMED_OUT with a debit, %d declined before step 10; want 1 and 1", timedOut, declined)
		}
		if n := e.chain.TxCount(t, e.chain.Operator); n != before+1 {
			t.Errorf("the operator sent %d transactions, want 1", n-before)
		}
		if next := e.count(t, `SELECT next_nonce FROM operator_accounts`); uint64(next) != before+1 {
			t.Errorf("next_nonce %d, want %d: one nonce reserved", next, before+1)
		}
	})

	t.Run("enough time left: the debit goes ahead", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true})
		e.chain.SetAutomine(t, false)
		before := e.chain.TxCount(t, e.chain.Operator)
		first := make(chan response, 1)
		go func() { first <- e.authorize(t, authReq("auth-417-c", "card_A", "5", "USD")) }()
		waitFor(t, func() bool { return e.chain.TxCount(t, e.chain.Operator) == before+1 })
		second := make(chan response, 1)
		go func() { second <- e.authorize(t, authReq("auth-417-d", "card_A", "5", "USD")) }()
		time.Sleep(700 * time.Millisecond)
		// The first is approved about 0.8 s in; the second gets the lock with about 1.6 s left.
		e.chain.Mine(t)
		e.chain.SetAutomine(t, true)
		approvedWith(t, <-first, "5000000")
		approvedWith(t, <-second, "5000000")
		if n := e.debitedFor(t, "auth-417-c", "auth-417-d"); n != 2 {
			t.Errorf("%d Debited events, want 2", n)
		}
	})
}

// S2-T418 — Req: FR-5, ADR-10. The failure between step 10 and the send: the fee read fails. The authorization
// was DEBIT_SUBMITTED with its PLANNED slot before anything was sent; at the failure the answer is the deadline's,
// TIMED_OUT, because the tracker may still send the slot (UC-3 row 9, st6: T516, T518).
func TestT418_IntentBeforeSend(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
	p.mu.Lock()
	p.failMethod = "eth_maxPriorityFeePerGas"
	p.mu.Unlock()
	before := c.TxCount(t, c.Operator)

	declinedWith(t, e.authorize(t, authReq("auth-418", "card_A", "5", "USD")), decision.StatusTimedOut, decision.ReasonTimeout)
	txs := e.opTxs(t, "auth-418")
	if len(txs) != 1 || txs[0].Status != "PLANNED" || txs[0].TxHash != nil || txs[0].Nonce != int64(before) {
		t.Errorf("operator_txs %+v, want one PLANNED slot with nonce %d and no hash", txs, before)
	}
	row := e.row(t, e.tenantA, "auth-418")
	if strings.Join(row.Events, " ") != ">RECEIVED: RECEIVED>DEBIT_SUBMITTED: DEBIT_SUBMITTED>TIMED_OUT:TIMEOUT" || row.Status != decision.StatusTimedOut {
		t.Errorf("history %v", row.Events)
	}
	if n := c.TxCount(t, c.Operator); n != before || len(e.debitedLogs(t)) != 0 {
		t.Errorf("something was sent: nonce %d → %d", before, n)
	}
	if !strings.Contains(e.logs.String(), "debit not sent") {
		t.Error("the failure is not logged")
	}
}

// S2-T419 — Req: §3.2, ADR-10
func TestT419_NonceAllocation(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	n := int64(e.chain.TxCount(t, e.chain.Operator))
	ids := []string{"auth-419-1", "auth-419-2", "auth-419-x", "auth-419-3", "auth-419-4", "auth-419-5"}
	for _, id := range ids {
		amount := "1"
		if strings.HasSuffix(id, "x") {
			amount = "100.01" // INSUFFICIENT_FUNDS: no nonce
		}
		e.authorize(t, authReq(id, "card_A", amount, "USD"))
	}
	var nonces []int64
	for _, id := range ids {
		for _, tx := range e.opTxs(t, id) {
			nonces = append(nonces, tx.Nonce)
		}
	}
	if len(nonces) != 5 || nonces[0] != n || nonces[4] != n+4 || !sort.SliceIsSorted(nonces, func(i, j int) bool { return nonces[i] < nonces[j] }) ||
		nonces[1] != n+1 || nonces[2] != n+2 || nonces[3] != n+3 {
		t.Errorf("nonces %v, want %d … %d without a gap", nonces, n, n+4)
	}
	if next := e.count(t, `SELECT next_nonce FROM operator_accounts`); int64(next) != n+5 {
		t.Errorf("next_nonce %d, want %d", next, n+5)
	}

	t.Run("the operator row is locked during the reservation", func(t *testing.T) {
		tx, err := e.owner.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT next_nonce FROM operator_accounts FOR UPDATE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan response, 1)
		go func() { done <- e.authorize(t, authReq("auth-419-6", "card_A", "1", "USD")) }()
		time.Sleep(400 * time.Millisecond)
		if got := e.count(t, `SELECT count(*) FROM operator_txs WHERE nonce = $1`, n+5); got != 0 {
			t.Error("a nonce was reserved while the row was locked")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		approvedWith(t, <-done, "1000000")
		if got := e.opTxs(t, "auth-419-6"); len(got) != 1 || got[0].Nonce != n+5 {
			t.Errorf("operator_txs %+v, want nonce %d", got, n+5)
		}
	})
}

// S2-T420 — Req: FR-6, ADR-12
func TestT420_ValidUntil(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	received := time.Now().Add(30 * time.Second).Truncate(time.Second).Add(700 * time.Millisecond).UTC()
	e.clock.Set(received)
	e.chain.Call(t, nil, "evm_setNextBlockTimestamp", received.Unix()+1)
	r := e.authorize(t, authReq("auth-420", "card_A", "1", "USD"))
	approvedWith(t, r, "1000000")

	var tx struct {
		Input hexutil.Bytes `json:"input"`
	}
	e.chain.Call(t, &tx, "eth_getTransactionByHash", r.str("tx_hash"))
	abi, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	args, err := abi.Methods["debit"].Inputs.Unpack(tx.Input[4:])
	if err != nil {
		t.Fatal(err)
	}
	want := received.Unix() + 4 // 12:00:00.700 + 4 s, floored to 12:00:04
	if got := args[3].(uint64); int64(got) != want || args[0].(common.Address) != e.wallet ||
		args[1].(*big.Int).String() != "1000000" || common.Hash(args[2].([32]byte)) != decision.ChainAuthID(e.tenantA, "auth-420") {
		t.Errorf("calldata %v, want validUntil %d", args, want)
	}
	var stored time.Time
	if err := e.owner.QueryRow(ctx, `SELECT valid_until FROM authorizations WHERE auth_id = 'auth-420'`).Scan(&stored); err != nil || stored.Unix() != want {
		t.Errorf("valid_until %v, want %d", stored, want)
	}
}

// S2-T421 — Req: ADR-10, §3.2 fees
func TestT421_Fees(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
	base := big.NewInt(3_000_000_000)
	c.Call(t, nil, "anvil_setNextBlockBaseFeePerGas", (*hexutil.Big)(base))
	var tip hexutil.Big
	c.Call(t, &tip, "eth_maxPriorityFeePerGas")
	p.take()

	r := e.authorize(t, authReq("auth-421", "card_A", "1", "USD"))
	approvedWith(t, r, "1000000")
	var tx struct {
		Type    hexutil.Uint64 `json:"type"`
		Tip     hexutil.Big    `json:"maxPriorityFeePerGas"`
		FeeCap  hexutil.Big    `json:"maxFeePerGas"`
		Gas     hexutil.Uint64 `json:"gas"`
		ChainID hexutil.Big    `json:"chainId"`
	}
	c.Call(t, &tx, "eth_getTransactionByHash", r.str("tx_hash"))
	wantFeeCap := new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), (*big.Int)(&tip))
	if tx.Type != 2 || (*big.Int)(&tx.Tip).Cmp((*big.Int)(&tip)) != 0 || (*big.Int)(&tx.FeeCap).Cmp(wantFeeCap) != 0 ||
		uint64(tx.Gas) != config.DefaultDebitGasLimit || (*big.Int)(&tx.ChainID).Int64() != testchain.ChainID {
		t.Errorf("transaction type %d, tip %s, fee cap %s, gas %d, chain %s; want 2, %s, %s, %d, 31337", tx.Type,
			(*big.Int)(&tx.Tip), (*big.Int)(&tx.FeeCap), tx.Gas, (*big.Int)(&tx.ChainID), (*big.Int)(&tip), wantFeeCap,
			config.DefaultDebitGasLimit)
	}
	for _, req := range p.take() {
		if strings.Contains(string(req.body), "eth_estimateGas") {
			t.Error("eth_estimateGas was called")
		}
	}
}

// S2-T422 — Req: EC-4, FR-8
func TestT422_RevertedDebit(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	before := e.chain.TxCount(t, e.chain.Operator)
	done := make(chan response, 1)
	go func() { done <- e.authorize(t, authReq("auth-422", "card_A", "5", "USD")) }()
	waitFor(t, func() bool { return e.chain.TxCount(t, e.chain.Operator) == before+1 })
	// The allowance is revoked in the same block, ahead of the debit by its higher tip.
	e.chain.SendAsync(t, e.wallet, e.chain.Token, testchain.TokenCalldata(t, "approve", e.chain.Controller, big.NewInt(0)),
		big.NewInt(100_000_000_000))
	e.chain.Mine(t)

	declinedWith(t, <-done, decision.StatusDeclined, decision.ReasonDebitReverted)
	txs := e.opTxs(t, "auth-422")
	if len(txs) != 1 || txs[0].Status != "REVERTED" || txs[0].BlockNumber == nil {
		t.Errorf("operator_txs %+v, want REVERTED with its block", txs)
	}
	if n := e.count(t, `SELECT count(*) FROM authorizations WHERE auth_id = 'auth-422' AND debited_amount = 0`); n != 1 {
		t.Error("debited_amount is not 0")
	}
	if len(e.debitedLogs(t)) != 0 {
		t.Error("a Debited event exists")
	}
}

// S2-T423 — Req: EC-5, FR-1
func TestT423_Deadline(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	start := time.Now()
	r := e.authorize(t, authReq("auth-423", "card_A", "5", "USD"))
	took := time.Since(start)
	declinedWith(t, r, decision.StatusTimedOut, decision.ReasonTimeout)
	if took < 2400*time.Millisecond || took > 2550*time.Millisecond {
		t.Errorf("answered after %s, want the deadline 2.5 s ± 50 ms", took)
	}
	row := e.row(t, e.tenantA, "auth-423")
	if row.Status != decision.StatusTimedOut || row.DeclineReason != decision.ReasonTimeout || row.DecidedAt == nil {
		t.Errorf("row %+v", row)
	}
	if txs := e.opTxs(t, "auth-423"); len(txs) != 1 || txs[0].Status != "SENT" || txs[0].TxHash == nil {
		t.Errorf("operator_txs %+v, want SENT with its hash, for the tracker", txs)
	}
}

// S2-T423 — Req: FR-1, D-21. The outcome is stored even when the answer has already gone out at the deadline: a
// final write slowed by 300 ms.
func TestT423_OutcomeStoredAfterTheAnswer(t *testing.T) {
	c := testchain.Start(t)
	slow := func(p *pgxpool.Pool) decision.DB {
		return slowDB{Pool: p, match: "FinishSubmittedAuthorization", delay: 300 * time.Millisecond}
	}

	t.Run("deadline: the answer on time, the row TIMED_OUT", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, debitDB: slow})
		e.chain.SetAutomine(t, false)
		defer e.chain.SetAutomine(t, true)
		start := time.Now()
		r := e.authorize(t, authReq("auth-423-slow", "card_A", "5", "USD"))
		if took := time.Since(start); took > 2550*time.Millisecond {
			t.Errorf("answered after %s, the deadline is 2.5 s", took)
		}
		declinedWith(t, r, decision.StatusTimedOut, decision.ReasonTimeout)
		waitFor(t, func() bool { return e.row(t, e.tenantA, "auth-423-slow").Status == decision.StatusTimedOut })
		if got := e.row(t, e.tenantA, "auth-423-slow"); strings.Join(got.Events, " ") !=
			">RECEIVED: RECEIVED>DEBIT_SUBMITTED: DEBIT_SUBMITTED>TIMED_OUT:TIMEOUT" {
			t.Errorf("history %v", got.Events)
		}
	})

	t.Run("signal near the deadline: answered TIMEOUT, so stored TIMED_OUT, never APPROVED", func(t *testing.T) {
		e := newEnv(t, options{chain: c, realDebit: true, noCRS: true, debitDB: slow})
		e.chain.SetAutomine(t, false)
		defer e.chain.SetAutomine(t, true)
		before := e.chain.TxCount(t, e.chain.Operator)
		start := time.Now()
		done := make(chan response, 1)
		go func() { done <- e.authorize(t, authReq("auth-423-late", "card_A", "5", "USD")) }()
		waitFor(t, func() bool { return e.chain.TxCount(t, e.chain.Operator) == before+1 })
		time.Sleep(time.Until(start.Add(2200 * time.Millisecond)))
		e.chain.Mine(t) // the signal comes about 2.25 s in; its write takes 300 ms, past the deadline
		r := <-done
		if took := time.Since(start); took > 2550*time.Millisecond {
			t.Errorf("answered after %s, the deadline is 2.5 s", took)
		}
		declinedWith(t, r, decision.StatusTimedOut, decision.ReasonTimeout)
		waitFor(t, func() bool { return e.row(t, e.tenantA, "auth-423-late").Status != decision.StatusDebitSubmitted })
		time.Sleep(500 * time.Millisecond)
		if got := e.row(t, e.tenantA, "auth-423-late"); got.Status != decision.StatusTimedOut {
			t.Errorf("row %s, want TIMED_OUT: the processor was told TIMEOUT", got.Status)
		}
		if !strings.Contains(e.logs.String(), `"msg":"outcome answered at the deadline before it was stored","tenant_id"`) ||
			!strings.Contains(e.logs.String(), `"status":"APPROVED"`) {
			t.Errorf("the APPROVED write was not overtaken by the deadline:\n%s", e.logs.String())
		}
		if txs := e.opTxs(t, "auth-423-late"); len(txs) != 1 || txs[0].Status != "SENT" {
			t.Errorf("operator_txs %+v, want SENT for the tracker", txs)
		}
		if n := e.debitedFor(t, "auth-423-late"); n != 1 {
			t.Errorf("%d Debited events: the late debit is on chain for the tracker (UC-3 row 4)", n)
		}
	})
}

// S2-T424 — Req: EC-15
func TestT424_SendResultUnknown(t *testing.T) {
	c := testchain.Start(t)
	p := newRPCProxy(t, c.RPCURL)
	e := newEnv(t, options{chain: c, rpcURL: p.srv.URL, realDebit: true, noCRS: true})
	p.mu.Lock()
	p.hangMethod, p.hangFor = "eth_sendRawTransaction", time.Second
	p.mu.Unlock()
	approvedWith(t, e.authorize(t, authReq("auth-424", "card_A", "5", "USD")), "5000000")
	if !strings.Contains(e.logs.String(), "debit send failed; treated as sent") {
		t.Error("the send failure is not logged")
	}
	if logs := e.logs.String(); strings.Contains(logs, p.srv.URL) || strings.Contains(logs, strings.TrimPrefix(p.srv.URL, "http://")) {
		t.Error("the log contains the RPC URL")
	}
}

// S2-T425 — Req: §2.1.1. The failure at step 10.
func TestT425_InternalErrorAtStep10(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true, debitDB: func(p *pgxpool.Pool) decision.DB {
		return failingDB{Pool: p, match: "InsertPlannedDebit"}
	}})
	declinedWith(t, e.authorize(t, authReq("auth-425", "card_A", "5", "USD")), decision.StatusDeclined, decision.ReasonInternalError)
	if txs := e.opTxs(t, "auth-425"); len(txs) != 0 {
		t.Errorf("operator_txs %+v", txs)
	}
	if n := e.count(t, `SELECT next_nonce FROM operator_accounts`); n != 0 {
		t.Errorf("next_nonce %d: the reservation was not rolled back", n)
	}
	e.noTransaction(t)
}

// S2-T319 — Req: EC-115, UC-103
func TestT319_CardChangedInFlight(t *testing.T) {
	e := newEnv(t, options{realDebit: true, noCRS: true})
	e.chain.SetAutomine(t, false)
	before := e.chain.TxCount(t, e.chain.Operator)
	done := make(chan response, 1)
	go func() { done <- e.authorize(t, authReq("auth-319-a", "card_A", "5", "USD")) }()
	waitFor(t, func() bool { return e.chain.TxCount(t, e.chain.Operator) == before+1 })
	// UpdateCard writes the cards row: the limit below the in-flight amount, and the freeze.
	e.exec(t, `UPDATE cards SET daily_limit = 1000000, status = 'FROZEN', updated_at = now() WHERE id = $1`, e.cardA)
	e.chain.Mine(t)
	approvedWith(t, <-done, "5000000")
	e.chain.SetAutomine(t, true)
	declinedWith(t, e.authorize(t, authReq("auth-319-b", "card_A", "1", "USD")), decision.StatusDeclined, decision.ReasonCardFrozen)
}

// S2-T426 — Req: §2.5.1, §3.2 Performance. A p95 above 2 s is P2 locally: logged, not failed.
func TestT426_LatencyAndMetrics(t *testing.T) {
	e := newEnv(t, options{realDebit: true})
	e.crs.Set("EUR", crstest.Answer{RateDecimal: "1.1642000000"})
	e.crs.Set("CHF", crstest.Answer{RateDecimal: "1.2500000000", Outdated: true})
	walletB := e.chain.NewWallet(t)
	e.chain.Mint(t, walletB, usdc(t, "100"))
	e.chain.Approve(t, walletB, usdc(t, "1"))
	e.chain.SetDailyLimit(t, walletB, usdc(t, "50"))
	e.addCard(t, e.tenantA, "card_B", walletB, usdc(t, "200"))
	e.addCard(t, e.tenantA, "card_F", walletB, usdc(t, "200"))
	e.exec(t, `UPDATE cards SET status = 'FROZEN' WHERE card_ref = 'card_F'`)
	e.addCard(t, e.tenantA, "card_L", walletB, usdc(t, "1"))

	var approvals []time.Duration
	n := 0
	send := func(card, amount, currency string) response {
		n++
		start := time.Now()
		r := e.authorize(t, authReq("auth-426-"+string(rune('a'+n)), card, amount, currency))
		if r.str("decision") == decision.Approved {
			approvals = append(approvals, time.Since(start))
		}
		return r
	}
	for range 10 {
		approvedWith(t, send("card_A", "1", "USD"), "1000000")
	}
	approvedWith(t, send("card_A", "1", "EUR"), "1175842")
	for _, c := range []struct{ card, amount, currency, reason string }{
		{"card_none", "1", "USD", decision.ReasonCardNotFound},
		{"card_F", "1", "USD", decision.ReasonCardFrozen},
		{"card_A", "1", "JPY", decision.ReasonCurrencyNotSupported},
		{"card_A", "1", "CHF", decision.ReasonRateUnavailable},
		{"card_L", "2", "USD", decision.ReasonLimitExceeded},
		{"card_A", "100.01", "USD", decision.ReasonInsufficientFunds},
		{"card_B", "2", "USD", decision.ReasonInsufficientAllowance},
	} {
		declinedWith(t, send(c.card, c.amount, c.currency), decision.StatusDeclined, c.reason)
	}
	e.chain.Pause(t)
	declinedWith(t, send("card_A", "1", "USD"), decision.StatusDeclined, decision.ReasonProgramPaused)
	e.chain.Unpause(t)
	e.chain.SetAutomine(t, false)
	declinedWith(t, send("card_A", "1", "USD"), decision.StatusTimedOut, decision.ReasonTimeout)
	e.chain.SetAutomine(t, true)
	approvedWith(t, send("card_A", "1", "USD"), "1000000")

	if n != 21 || e.observations(t) != 21 {
		t.Errorf("%d requests, %d observations of auth_decision_seconds", n, e.observations(t))
	}
	for _, c := range []struct {
		decision, reason string
		want             float64
	}{
		{decision.Approved, "", 12}, {decision.Declined, decision.ReasonCardNotFound, 1}, {decision.Declined, decision.ReasonTimeout, 1},
		{decision.Declined, decision.ReasonProgramPaused, 1}, {decision.Declined, decision.ReasonInsufficientAllowance, 1},
	} {
		if got := e.decisions(t, c.decision, c.reason); got != c.want {
			t.Errorf("auth_decisions_total{%s,%s} = %v, want %v", c.decision, c.reason, got, c.want)
		}
	}
	sort.Slice(approvals, func(i, j int) bool { return approvals[i] < approvals[j] })
	p95 := approvals[(len(approvals)*95+99)/100-1]
	t.Logf("S2-T426: %d approvals on Anvil, p95 %s, max %s", len(approvals), p95, approvals[len(approvals)-1])
	if p95 > 2*time.Second {
		t.Logf("P2: p95 %s is above 2 s", p95)
	}
}
