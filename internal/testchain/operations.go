package testchain

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
)

// Token movements of the tests (SRS — EVM Connector §2.6 Local chain): sent by impersonated wallets without a key.
// Debit and refund are sent by the operator with its generated key through the go-ethereum bindings, as the
// contract demands OPERATOR_ROLE; internal/signer is not used.

// Transfer sends amount base units of the token from a wallet of NewWallet to another address.
func (c *Chain) Transfer(t testing.TB, from, to common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, from, c.Token, tokenABI(t), "transfer", to, amount)
}

// Burn burns amount base units of the token of a wallet of NewWallet.
func (c *Chain) Burn(t testing.TB, wallet common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, wallet, c.Token, tokenABI(t), "burn", amount)
}

// MineBlocks mines n empty blocks.
func (c *Chain) MineBlocks(t testing.TB, n uint64) {
	t.Helper()
	c.Call(t, nil, "anvil_mine", hexutil.Uint64(n))
}

// Head returns the number of the latest block.
func (c *Chain) Head(t testing.TB) uint64 {
	t.Helper()
	var n hexutil.Uint64
	c.Call(t, &n, "eth_blockNumber")
	return uint64(n)
}

// Debit pulls amount from a wallet to the treasury through the controller, as the operator, valid without end.
// The wallet must have the daily limit and the allowance (SetDailyLimit, Approve).
func (c *Chain) Debit(t testing.TB, wallet common.Address, amount *big.Int, authID [32]byte) common.Hash {
	t.Helper()
	ctrl, client, opts := c.operator(t)
	tx, err := ctrl.Debit(opts, wallet, amount, authID, 1<<63)
	if err != nil {
		t.Fatalf("testchain: debit: %v", err)
	}
	c.waitMined(t, client, tx.Hash(), "debit")
	return tx.Hash()
}

// Refund returns amount of the debit authID from the treasury to its wallet, as the operator. The treasury must
// have the allowance (ApproveRefunds).
func (c *Chain) Refund(t testing.TB, authID, refundID [32]byte, amount *big.Int) common.Hash {
	t.Helper()
	ctrl, client, opts := c.operator(t)
	tx, err := ctrl.Refund(opts, authID, refundID, amount)
	if err != nil {
		t.Fatalf("testchain: refund: %v", err)
	}
	c.waitMined(t, client, tx.Hash(), "refund")
	return tx.Hash()
}

// operator returns the controller bound to an ethclient of the chain and the transact options of the operator.
func (c *Chain) operator(t testing.TB) (*bindings.CardSpendController, *ethclient.Client, *bind.TransactOpts) {
	t.Helper()
	client, err := ethclient.Dial(c.RPCURL)
	if err != nil {
		t.Fatalf("testchain: dial anvil: %v", err)
	}
	t.Cleanup(client.Close)
	key, err := crypto.ToECDSA(c.OperatorKey.Value())
	if err != nil {
		t.Fatal("testchain: the operator key is malformed")
	}
	opts, err := bind.NewKeyedTransactorWithChainID(key, big.NewInt(ChainID))
	if err != nil {
		t.Fatalf("testchain: transactor: %v", err)
	}
	ctrl, err := bindings.NewCardSpendController(c.Controller, client)
	if err != nil {
		t.Fatalf("testchain: bind the controller: %v", err)
	}
	return ctrl, client, opts
}

func (c *Chain) waitMined(t testing.TB, client *ethclient.Client, hash common.Hash, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := bind.WaitMinedHash(ctx, client, hash)
	if err != nil {
		t.Fatalf("testchain: %s not mined: %v", what, err)
	}
	if receipt.Status != 1 {
		t.Fatalf("testchain: %s reverted", what)
	}
}

// DeployToken deploys another MockUSDC from the build of contracts/ (forge build ran in Start), sent by the
// unlocked DEPLOYER, and returns its address: a token without an alias row.
func (c *Chain) DeployToken(t testing.TB) common.Address {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ContractsDir(t), "out", "MockUSDC.sol", "MockUSDC.json"))
	if err != nil {
		t.Fatalf("testchain: the build of MockUSDC: %v", err)
	}
	var artifact struct {
		Bytecode struct {
			Object hexutil.Bytes `json:"object"`
		} `json:"bytecode"`
	}
	if err := json.Unmarshal(data, &artifact); err != nil || len(artifact.Bytecode.Object) == 0 {
		t.Fatalf("testchain: no bytecode in the build of MockUSDC: %v", err)
	}
	var hash common.Hash
	c.Call(t, &hash, "eth_sendTransaction", map[string]any{"from": c.Deployer, "data": artifact.Bytecode.Object})
	client, err := ethclient.Dial(c.RPCURL)
	if err != nil {
		t.Fatalf("testchain: dial anvil: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, err := bind.WaitMinedHash(ctx, client, hash)
	if err != nil || receipt.Status != 1 || receipt.ContractAddress == (common.Address{}) {
		t.Fatalf("testchain: deploy a second token: %v", err)
	}
	return receipt.ContractAddress
}

// TransferOf sends amount base units of token from a wallet of NewWallet to another address: for a token other
// than the deployed one.
func (c *Chain) TransferOf(t testing.TB, token, from, to common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, from, token, tokenABI(t), "transfer", to, amount)
}

// MintOf mints amount base units of token to a wallet of NewWallet.
func (c *Chain) MintOf(t testing.TB, token, wallet common.Address, amount *big.Int) {
	t.Helper()
	c.send(t, wallet, token, tokenABI(t), "mint", wallet, amount)
}
