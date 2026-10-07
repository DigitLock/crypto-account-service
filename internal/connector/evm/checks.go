package evm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
)

// Start checks of a network, the label check of evm_start_check_failed (§2.1.1 Common rules; S3 D-23, S3 D-30).
const (
	CheckConfig   = "config"
	CheckChainID  = "chain_id"
	CheckToken    = "token"
	CheckTreasury = "treasury"
)

// StartCheckError is a failed start check of a network. It fails the run before any other read and is not an RPC
// error: the endpoint of the next run does not change.
type StartCheckError struct {
	Check  string
	Detail string
}

func (e *StartCheckError) Error() string {
	return "evm: start check " + e.Check + " failed: " + e.Detail
}

// controllerABI is the ABI of the frozen CardSpendController (contracts/abi).
var controllerABI = mustABI(bindings.CardSpendControllerMetaData.GetAbi())

func mustABI(a *abi.ABI, err error) *abi.ABI {
	if err != nil {
		panic("evm: the ABI of the controller: " + err.Error())
	}
	return a
}

// checkFailed records a failed check: evm_start_check_failed 1 and an ERROR line marked critical.
func (s *session) checkFailed(ctx context.Context, check, detail string) error {
	s.c.metrics.StartCheck(s.src.Code, check, true)
	s.c.logger.ErrorContext(ctx, "critical: EVM start check failed; the run fails",
		"source", s.src.Code, "check", check, "detail", detail)
	return &StartCheckError{Check: check, Detail: detail}
}

func (s *session) checkPassed(check string) { s.c.metrics.StartCheck(s.src.Code, check, false) }

// startChecks runs the checks that have not passed yet (S3 D-23): the chain ID of the endpoint of the run, token()
// of the controller among the tracked tokens and every tracked token with 0 to 18 decimals, treasury() of the
// controller equal to treasury_address. A check that passed is kept until the process stops. An RPC error of a
// check is an RPC error of the run, not a failed check; a controller without code is a failed check.
func (s *session) startChecks(ctx context.Context) error {
	n := s.net
	n.mu.Lock()
	chainChecked, tokenChecked, treasuryOK := n.chainChecked[s.endpoint], n.tokenChecked, n.treasuryOK
	n.mu.Unlock()

	if !chainChecked {
		var id hexutil.Uint64
		if err := s.call(ctx, &id, "eth_chainId"); err != nil {
			return err
		}
		switch {
		case uint64(id) != s.cfg.ChainID:
			return s.checkFailed(ctx, CheckChainID, fmt.Sprintf("%s serves chain ID %d, chain_id of the source is %d",
				s.variable(), uint64(id), s.cfg.ChainID))
		case !s.c.allowed[uint64(id)]:
			return s.checkFailed(ctx, CheckChainID, fmt.Sprintf("chain ID %d is not in EVM_ALLOWED_CHAIN_IDS", uint64(id)))
		}
		n.mu.Lock()
		n.chainChecked[s.endpoint] = true
		n.mu.Unlock()
		s.checkPassed(CheckChainID)
	}

	if !tokenChecked {
		// The decimals of every alias row first: a local check, no request.
		for _, a := range s.src.Aliases {
			if _, err := tokenDecimals(a); err != nil {
				return s.checkFailed(ctx, CheckToken, err.Error())
			}
		}
		token, err := s.controllerAddress(ctx, "token")
		if errors.Is(err, errNoCode) {
			return s.checkFailed(ctx, CheckToken, err.Error())
		}
		if err != nil {
			return err
		}
		if !s.tracked(token) {
			return s.checkFailed(ctx, CheckToken, fmt.Sprintf("token() of the controller %s is %s, which has no row in asset_aliases",
				s.cfg.ControllerAddress, token.Hex()))
		}
		n.mu.Lock()
		n.tokenChecked = true
		n.mu.Unlock()
		s.checkPassed(CheckToken)
	}

	if s.cfg.TreasuryConnection == "" {
		n.mu.Lock()
		warn := !n.treasuryWarned
		n.treasuryWarned = true
		n.mu.Unlock()
		if warn {
			s.c.logger.WarnContext(ctx, "EVM source without a treasury connection: the treasury check is skipped, "+
				"wallet streams run, reconciliation does not run; set it with casctl source set-treasury", "source", s.src.Code)
		}
		return nil
	}
	if !treasuryOK {
		treasury, err := s.controllerAddress(ctx, "treasury")
		if errors.Is(err, errNoCode) {
			return s.checkFailed(ctx, CheckTreasury, err.Error())
		}
		if err != nil {
			return err
		}
		if s.cfg.TreasuryAddress == "" || !strings.EqualFold(s.cfg.TreasuryAddress, treasury.Hex()) {
			address := s.cfg.TreasuryAddress
			if address == "" {
				address = "unset"
			}
			return s.checkFailed(ctx, CheckTreasury, fmt.Sprintf("treasury() of the controller is %s, treasury_address of the source is %s",
				treasury.Hex(), address))
		}
		n.mu.Lock()
		n.treasury, n.treasuryOK = treasury, true
		n.mu.Unlock()
		s.checkPassed(CheckTreasury)
	}
	return nil
}

// tracked reports whether address is a native asset of an alias row of the source, regardless of case.
func (s *session) tracked(address common.Address) bool {
	for _, a := range s.src.Aliases {
		if strings.EqualFold(a.NativeAsset, address.Hex()) {
			return true
		}
	}
	return false
}

// errNoCode marks an empty answer of an eth_call to the controller: no contract at controller_address. It is a
// failed start check, a configuration error, not an RPC error: the endpoint of the next run does not change.
var errNoCode = errors.New("evm: no contract code")

// controllerAddress calls a constant of the controller that returns an address: token() or treasury().
func (s *session) controllerAddress(ctx context.Context, method string) (common.Address, error) {
	data, err := controllerABI.Pack(method)
	if err != nil {
		return common.Address{}, fmt.Errorf("evm: pack %s(): %w", method, err)
	}
	var out *hexutil.Bytes // nil for a null result
	call := map[string]any{"to": common.HexToAddress(s.cfg.ControllerAddress), "data": hexutil.Bytes(data)}
	if err := s.call(ctx, &out, "eth_call", call, "latest"); err != nil {
		return common.Address{}, err
	}
	// "0x" or a null result: an address without code.
	if out == nil || len(*out) == 0 {
		return common.Address{}, fmt.Errorf("%w at controller_address %s: %s() answered no data",
			errNoCode, s.cfg.ControllerAddress, method)
	}
	values, err := controllerABI.Unpack(method, *out)
	if err != nil || len(values) != 1 {
		return common.Address{}, fmt.Errorf("evm: %s: %s() of the controller %s: malformed answer: no contract at the address?",
			s.variable(), method, s.cfg.ControllerAddress)
	}
	address, ok := values[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("evm: %s: %s() of the controller: malformed answer", s.variable(), method)
	}
	return address, nil
}
