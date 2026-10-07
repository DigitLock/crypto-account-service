package evm

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
	"github.com/DigitLock/crypto-account-service/internal/connector"
)

// AccountTypeWallet is the account type of every balance of a wallet (UC-302 step 3).
const AccountTypeWallet = "WALLET"

// MaxDecimals is the largest decimals of a tracked token (§2.1.1 Tracked tokens).
const MaxDecimals = 18

// tokenABI is the ABI of MockUSDC, a plain ERC-20: the connector reads balanceOf only.
var tokenABI = mustABI(bindings.MockUSDCMetaData.GetAbi())

// snapshot runs UC-302 steps 1 to 4 on the endpoint of the run; the endpoint choice and the start checks are open.
// One balance per tracked token, zero included. Any read that fails fails the snapshot as a whole (EC-304). A
// balance above the ledger amount type is returned as read: the ledger writer refuses the snapshot (EC-319).
func (s *session) snapshot(ctx context.Context) (connector.Snapshot, error) {
	account, err := CheckAddress(s.conn.Account)
	if err != nil {
		return connector.Snapshot{}, fmt.Errorf("evm: the account of connection %s is not a wallet address", s.conn.ID)
	}

	// Step 1.
	head, err := s.headerAt(ctx, "latest")
	if err != nil {
		return connector.Snapshot{}, err
	}
	if head == nil {
		return connector.Snapshot{}, fmt.Errorf("evm: %s: the endpoint has no latest block", s.variable())
	}

	// Steps 2 and 3: every read is pinned to the head block by its hash (EIP-1898).
	pin := map[string]any{"blockHash": head.Hash}
	balances := make([]connector.Balance, 0, len(s.src.Aliases))
	for _, a := range s.src.Aliases {
		decimals, err := tokenDecimals(a)
		if err != nil {
			return connector.Snapshot{}, err
		}
		units, err := s.balanceOf(ctx, a.NativeAsset, common.HexToAddress(account), pin)
		if err != nil {
			return connector.Snapshot{}, err
		}
		balances = append(balances, connector.Balance{
			AccountType: AccountTypeWallet, NativeAsset: a.NativeAsset, Free: FormatUnits(units, decimals), Locked: "0",
		})
	}

	// Step 4.
	return connector.Snapshot{TakenAt: time.Unix(int64(head.Time), 0).UTC(), Balances: balances}, nil
}

// balanceOf reads balanceOf(account) of a token at the pinned block.
func (s *session) balanceOf(ctx context.Context, token string, account common.Address, pin map[string]any) (*big.Int, error) {
	if !common.IsHexAddress(token) {
		return nil, fmt.Errorf("evm: the alias row %s of the source is not a token address", token)
	}
	data, err := tokenABI.Pack("balanceOf", account)
	if err != nil {
		return nil, fmt.Errorf("evm: pack balanceOf: %w", err)
	}
	var out *hexutil.Bytes // nil for a null result
	call := map[string]any{"to": common.HexToAddress(token), "data": hexutil.Bytes(data)}
	if err := s.call(ctx, &out, "eth_call", call, pin); err != nil {
		return nil, err
	}
	// "0x" or a null result: no contract at the address of the alias row. A failure of the run, not an RPC error.
	if out == nil {
		out = &hexutil.Bytes{}
	}
	values, err := tokenABI.Unpack("balanceOf", *out)
	if err != nil || len(values) != 1 {
		return nil, fmt.Errorf("evm: %s: balanceOf of the token %s: malformed answer: no contract at the address?",
			s.variable(), token)
	}
	units, ok := values[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("evm: %s: balanceOf of the token %s: malformed answer", s.variable(), token)
	}
	return units, nil
}

// errDecimals marks an alias row whose decimals are missing or outside 0 to MaxDecimals.
var errDecimals = errors.New("evm: decimals of a tracked token")

// tokenDecimals returns the decimals of an alias row: set, and 0 to MaxDecimals (§2.1.1 Tracked tokens).
func tokenDecimals(a connector.Alias) (int, error) {
	switch {
	case a.Decimals == nil:
		return 0, fmt.Errorf("%w: the alias row %s has no decimals", errDecimals, a.NativeAsset)
	case *a.Decimals < 0 || *a.Decimals > MaxDecimals:
		return 0, fmt.Errorf("%w: the alias row %s has decimals %d, want 0 to %d", errDecimals, a.NativeAsset,
			*a.Decimals, MaxDecimals)
	}
	return int(*a.Decimals), nil
}

// FormatUnits converts base units to a plain decimal with decimals places: integer arithmetic only, no exponent,
// no trailing zeros, "0" for zero. units must not be negative; decimals must not be negative.
func FormatUnits(units *big.Int, decimals int) string {
	digits := units.String()
	if decimals <= 0 {
		return digits
	}
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	whole, frac := digits[:len(digits)-decimals], strings.TrimRight(digits[len(digits)-decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}
