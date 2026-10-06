package testchain

import (
	"context"
	"crypto/rand"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
)

// Transactions of the tests: no key is used. A cardholder wallet is a random address that Anvil impersonates; it
// mints its own tokens (mint is open to anyone) and approves the controller. setDailyLimit, pause and unpause are
// sent by the unlocked ADMIN account, as the Deployment Guide does with cast.

// NewWallet returns a random cardholder address, impersonated by Anvil and funded with gas.
func (c *Chain) NewWallet(t testing.TB) common.Address {
	t.Helper()
	var wallet common.Address
	if _, err := rand.Read(wallet[:]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := dial(t, c.RPCURL)
	if err := client.CallContext(ctx, nil, "anvil_impersonateAccount", wallet); err != nil {
		t.Fatalf("testchain: impersonate the wallet: %v", err)
	}
	if err := client.CallContext(ctx, nil, "anvil_setBalance", wallet, (*hexutil.Big)(operatorBalance)); err != nil {
		t.Fatalf("testchain: fund the wallet: %v", err)
	}
	return wallet
}

// Mint mints amount base units of the token to a wallet of NewWallet, sent by the wallet itself.
func (c *Chain) Mint(t testing.TB, wallet common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, wallet, c.Token, tokenABI(t), "mint", wallet, amount)
}

// Approve sets the allowance of the wallet to the controller.
func (c *Chain) Approve(t testing.TB, wallet common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, wallet, c.Token, tokenABI(t), "approve", c.Controller, amount)
}

// SetDailyLimit sets the wallet daily limit of the controller, as ADMIN.
func (c *Chain) SetDailyLimit(t testing.TB, wallet common.Address, limit *big.Int) {
	t.Helper()
	c.send(t, c.Admin, c.Controller, controllerABI(t), "setDailyLimit", wallet, limit)
}

// Pause pauses the controller, as ADMIN.
func (c *Chain) Pause(t testing.TB) {
	t.Helper()
	c.send(t, c.Admin, c.Controller, controllerABI(t), "pause")
}

// Unpause unpauses the controller, as ADMIN.
func (c *Chain) Unpause(t testing.TB) {
	t.Helper()
	c.send(t, c.Admin, c.Controller, controllerABI(t), "unpause")
}

// TxCount returns the transaction count of the address at the pending tag.
func (c *Chain) TxCount(t testing.TB, addr common.Address) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n hexutil.Uint64
	if err := dial(t, c.RPCURL).CallContext(ctx, &n, "eth_getTransactionCount", addr, "pending"); err != nil {
		t.Fatalf("testchain: transaction count: %v", err)
	}
	return uint64(n)
}

// send sends a call from an unlocked or impersonated account and requires a successful receipt.
func (c *Chain) send(t testing.TB, from, to common.Address, a *abi.ABI, method string, args ...any) {
	t.Helper()
	data, err := a.Pack(method, args...)
	if err != nil {
		t.Fatalf("testchain: pack %s: %v", method, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := dial(t, c.RPCURL)
	var hash common.Hash
	if err := client.CallContext(ctx, &hash, "eth_sendTransaction", map[string]any{
		"from": from, "to": to, "data": hexutil.Bytes(data),
	}); err != nil {
		t.Fatalf("testchain: %s: %v", method, err)
	}
	for {
		var receipt *struct {
			Status hexutil.Uint64 `json:"status"`
		}
		if err := client.CallContext(ctx, &receipt, "eth_getTransactionReceipt", hash); err != nil {
			t.Fatalf("testchain: receipt of %s: %v", method, err)
		}
		if receipt != nil {
			if receipt.Status != 1 {
				t.Fatalf("testchain: %s reverted", method)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("testchain: %s not mined within 10 s", method)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func tokenABI(t testing.TB) *abi.ABI {
	t.Helper()
	a, err := bindings.MockUSDCMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func controllerABI(t testing.TB) *abi.ABI {
	t.Helper()
	a, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		t.Fatal(err)
	}
	return a
}
