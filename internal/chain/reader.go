package chain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/DigitLock/crypto-account-service/internal/chain/bindings"
)

// PendingTag is the block tag of every read of the decision path (SRS — Card Spend §3.2 Chain access).
const PendingTag = "pending"

// ErrRead is the error of a failed read of step 9: an RPC error, a malformed answer, or no answer in time.
var ErrRead = errors.New("chain: read failed")

// State is the on-chain state of a wallet that step 9 of UC-1 checks. Amounts are base units of the token.
type State struct {
	Balance             *big.Int
	Allowance           *big.Int // of the wallet to the controller
	RemainingDailyLimit *big.Int // of the wallet in the controller
	Paused              bool
}

// ReaderConfig holds the values of SRS — Card Spend §3.1 the reader uses.
type ReaderConfig struct {
	ChainID       uint64
	Controller    common.Address
	Token         common.Address
	ReadTimeout   time.Duration // rpc_read_timeout: the whole batch of step 9
	FallbackAfter int           // rpc_fallback_after
	ProbeInterval time.Duration // tracker_interval
}

// Reader reads the state of step 9 in one JSON-RPC batch at the pending block tag. After FallbackAfter
// consecutive failures of the primary endpoint it reads from the fallback; Probe moves it back. A read is never
// retried: a failure is the caller's CHAIN_UNAVAILABLE.
type Reader struct {
	client     *Client
	cfg        ReaderConfig
	logger     *slog.Logger
	tokenABI   *abi.ABI
	controlABI *abi.ABI
	failures   atomic.Int64 // consecutive failures of the primary
}

// NewReader returns a reader on the endpoints of client.
func NewReader(client *Client, cfg ReaderConfig, logger *slog.Logger) (*Reader, error) {
	tokenABI, err := bindings.MockUSDCMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("chain: token ABI: %w", err)
	}
	controlABI, err := bindings.CardSpendControllerMetaData.GetAbi()
	if err != nil {
		return nil, fmt.Errorf("chain: controller ABI: %w", err)
	}
	return &Reader{client: client, cfg: cfg, logger: logger, tokenABI: tokenABI, controlABI: controlABI}, nil
}

// OnFallback reports whether reads go to the fallback endpoint.
func (r *Reader) OnFallback() bool {
	return r.client.Fallback != nil && r.failures.Load() >= int64(r.cfg.FallbackAfter)
}

// Endpoint returns the endpoint reads and sends go to now (SRS — Card Spend §3.2 Chain access).
func (r *Reader) Endpoint() Endpoint {
	ep, _ := r.endpoint()
	return ep
}

// Describe turns an error of a call bounded by timeout into text without a URL.
func (r *Reader) Describe(err error, timeout time.Duration) string {
	return r.client.describeWithin(err, timeout)
}

// endpoint returns the endpoint reads go to now and whether it is the primary.
func (r *Reader) endpoint() (Endpoint, bool) {
	if r.OnFallback() {
		return *r.client.Fallback, false
	}
	return r.client.Primary, true
}

type call struct {
	abi    *abi.ABI
	to     common.Address
	method string
	args   []any
}

// Read reads balanceOf(wallet) and allowance(wallet, controller) of the token, remainingDailyLimit(wallet) and
// paused() of the controller: four eth_call in one batch at the pending tag within ReadTimeout. The error wraps
// ErrRead and names the endpoint by its variable, never its URL.
func (r *Reader) Read(ctx context.Context, wallet common.Address) (State, error) {
	ep, primary := r.endpoint()
	st, err := r.read(ctx, ep, wallet)
	if !primary {
		return st, err
	}
	switch {
	case err == nil:
		r.failures.Store(0)
	case ctx.Err() == nil:
		// A read cut short by the caller's deadline is not a failure of the endpoint.
		if n := r.failures.Add(1); n == int64(r.cfg.FallbackAfter) && r.client.Fallback != nil {
			r.logger.WarnContext(ctx, "chain reads moved to the fallback endpoint",
				"primary", r.client.Primary.Name, "fallback", r.client.Fallback.Name, "failures", n)
		}
	}
	return st, err
}

func (r *Reader) read(ctx context.Context, ep Endpoint, wallet common.Address) (State, error) {
	calls := []call{
		{r.tokenABI, r.cfg.Token, "balanceOf", []any{wallet}},
		{r.tokenABI, r.cfg.Token, "allowance", []any{wallet, r.cfg.Controller}},
		{r.controlABI, r.cfg.Controller, "remainingDailyLimit", []any{wallet}},
		{r.controlABI, r.cfg.Controller, "paused", nil},
	}
	results := make([]hexutil.Bytes, len(calls))
	batch := make([]rpc.BatchElem, len(calls))
	for i, c := range calls {
		data, err := c.abi.Pack(c.method, c.args...)
		if err != nil {
			return State{}, fmt.Errorf("%w: pack %s: %v", ErrRead, c.method, err)
		}
		batch[i] = rpc.BatchElem{
			Method: "eth_call",
			Args:   []any{map[string]any{"to": c.to, "data": hexutil.Bytes(data)}, PendingTag},
			Result: &results[i],
		}
	}

	rctx, cancel := context.WithTimeout(ctx, r.cfg.ReadTimeout)
	defer cancel()
	if err := ep.Client.Client().BatchCallContext(rctx, batch); err != nil {
		return State{}, fmt.Errorf("%w: %s: %s", ErrRead, ep.Name, r.client.describeWithin(err, r.cfg.ReadTimeout))
	}

	values := make([]any, len(calls))
	for i, c := range calls {
		if batch[i].Error != nil {
			return State{}, fmt.Errorf("%w: %s: %s: %s", ErrRead, ep.Name, c.method, r.client.describeWithin(batch[i].Error, r.cfg.ReadTimeout))
		}
		out, err := c.abi.Unpack(c.method, results[i])
		if err != nil || len(out) != 1 {
			return State{}, fmt.Errorf("%w: %s: %s: malformed answer", ErrRead, ep.Name, c.method)
		}
		values[i] = out[0]
	}
	st := State{}
	var ok [4]bool
	st.Balance, ok[0] = values[0].(*big.Int)
	st.Allowance, ok[1] = values[1].(*big.Int)
	st.RemainingDailyLimit, ok[2] = values[2].(*big.Int)
	st.Paused, ok[3] = values[3].(bool)
	if ok != [4]bool{true, true, true, true} {
		return State{}, fmt.Errorf("%w: %s: malformed answer", ErrRead, ep.Name)
	}
	return st, nil
}

// Probe runs until ctx ends. While reads go to the fallback, it calls eth_chainId on the primary every
// ProbeInterval; an answer with the configured chain ID moves the reads back to the primary.
func (r *Reader) Probe(ctx context.Context) {
	if r.client.Fallback == nil {
		return
	}
	ticker := time.NewTicker(r.cfg.ProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !r.OnFallback() {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, r.cfg.ReadTimeout)
		id, err := r.client.Primary.Client.ChainID(pctx)
		cancel()
		if err == nil && id.IsUint64() && id.Uint64() == r.cfg.ChainID {
			r.failures.Store(0)
			r.logger.InfoContext(ctx, "chain reads back on the primary endpoint", "primary", r.client.Primary.Name)
		}
	}
}
