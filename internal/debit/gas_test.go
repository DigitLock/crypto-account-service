package debit_test

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/config"
	"github.com/DigitLock/crypto-account-service/internal/signer"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// SRS — Card Spend §3.1 debit_gas_limit, refund_gas_limit: the gas of debit and refund on Anvil, cold and warm
// storage, against the defaults. A default must cover the measured maximum + 25 %, rounded up to a thousand. The
// measurement is eth_estimateGas, the gas limit a transaction needs; it is used here only, never in the decision
// path.
func TestGasLimitsCoverMeasured(t *testing.T) {
	c := testchain.Start(t)
	ctx := context.Background()
	client, err := ethclient.Dial(c.RPCURL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	op, err := signer.New(c.OperatorKey)
	if err != nil {
		t.Fatal(err)
	}
	abi, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	usdc := func(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1_000_000)) }
	c.ApproveRefunds(t, usdc(1_000_000))
	fund := func() common.Address {
		w := c.NewWallet(t)
		c.Mint(t, w, usdc(100))
		c.Approve(t, w, usdc(100))
		c.SetDailyLimit(t, w, usdc(100))
		return w
	}

	// measure estimates the gas of a call of the operator, then sends it and returns the estimate and the gas
	// used.
	measure := func(name string, args ...any) (estimate, used uint64) {
		data, err := abi.Pack(name, args...)
		if err != nil {
			t.Fatal(err)
		}
		controller := c.Controller
		estimate, err = client.EstimateGas(ctx, ethereum.CallMsg{From: op.Address(), To: &controller, Data: data})
		if err != nil {
			t.Fatalf("estimate %s: %v", name, err)
		}
		nonce, err := client.PendingNonceAt(ctx, op.Address())
		if err != nil {
			t.Fatal(err)
		}
		head, err := client.HeaderByNumber(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		tip := big.NewInt(1_000_000_000)
		fee := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		chainID := big.NewInt(testchain.ChainID)
		tx, err := op.SignTx(types.NewTx(&types.DynamicFeeTx{ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: fee,
			Gas: 2 * estimate, To: &controller, Value: new(big.Int), Data: data}), chainID)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.SendTransaction(ctx, tx); err != nil {
			t.Fatal(err)
		}
		r, err := waitReceipt(ctx, client, tx.Hash())
		if err != nil || r.Status != types.ReceiptStatusSuccessful {
			t.Fatalf("%s: receipt %v, %v", name, r, err)
		}
		return estimate, r.GasUsed
	}
	id := func(s string) [32]byte { return crypto.Keccak256Hash([]byte(s)) }
	validUntil := uint64(time.Now().Add(time.Hour).Unix())

	w1, w2 := fund(), fund()
	var maxDebit, maxRefund uint64
	record := func(max *uint64, what string, estimate, used uint64) {
		t.Logf("%s: eth_estimateGas %d, gas used %d", what, estimate, used)
		*max = Max(*max, estimate, used)
	}
	e, u := measure("debit", w1, usdc(10), id("a1"), validUntil)
	record(&maxDebit, "debit, cold: first debit, treasury balance and the wallet's daily spend zero", e, u)
	e, u = measure("debit", w1, usdc(10), id("a2"), validUntil)
	record(&maxDebit, "debit, warm: second debit of the wallet in the day", e, u)
	e, u = measure("debit", w2, usdc(10), id("a3"), validUntil)
	record(&maxDebit, "debit, cold spend: first debit of another wallet, treasury not empty", e, u)
	e, u = measure("debit", w2, usdc(90), id("a4"), validUntil)
	record(&maxDebit, "debit, last: balance and allowance of the wallet to zero", e, u)
	e, u = measure("refund", id("a4"), id("r1"), usdc(30))
	record(&maxRefund, "refund, cold: first refund, wallet balance zero", e, u)
	e, u = measure("refund", id("a4"), id("r2"), usdc(30))
	record(&maxRefund, "refund, warm: second refund of the debit", e, u)
	e, u = measure("refund", id("a1"), id("r3"), usdc(10))
	record(&maxRefund, "refund, full: the whole debit", e, u)

	want := func(measured uint64) uint64 { return (measured*125/100 + 999) / 1000 * 1000 }
	t.Logf("measured maximum: debit %d, refund %d; + 25 %% rounded up to a thousand: %d, %d",
		maxDebit, maxRefund, want(maxDebit), want(maxRefund))
	// The estimate moves by a few gas with the zero bytes of the calldata: the defaults must cover it, not equal it.
	if config.DefaultDebitGasLimit < want(maxDebit) || config.DefaultRefundGasLimit < want(maxRefund) {
		t.Errorf("defaults debit %d, refund %d do not cover %d and %d", config.DefaultDebitGasLimit,
			config.DefaultRefundGasLimit, want(maxDebit), want(maxRefund))
	}
}

// Max returns the largest value.
func Max(v ...uint64) uint64 {
	var m uint64
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

func waitReceipt(ctx context.Context, client *ethclient.Client, hash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		r, err := client.TransactionReceipt(ctx, hash)
		if err == nil {
			return r, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
