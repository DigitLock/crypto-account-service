// Package operator is the operator queue of card-auth (SRS — Card Spend §2.4 operator_txs, §3.2, ADR-10): one nonce
// sequence for debits and refunds, reserved in the database under the row lock of operator_accounts, the intent
// stored before anything is sent (FR-5), the hash stored before eth_sendRawTransaction.
//
// It holds the operator signer: only card-auth uses it; packages of server must not import it (ADR-3). Fees are
// *big.Int, never floats.
package operator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/decision"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/signer"
)

// Purposes of operator_txs (SRS — Card Spend §2.4).
const (
	PurposeDebit   = "DEBIT"
	PurposeRefund  = "REFUND"
	PurposeRelease = "RELEASE"
)

// DB is what the queue needs from the database, role cas_card_auth. *pgxpool.Pool satisfies it.
type DB interface {
	repository.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Slot is a reserved nonce and its operator_txs row.
type Slot struct {
	ID    uuid.UUID
	Nonce uint64
}

// Queue reserves nonces and sends operator transactions.
type Queue struct {
	db          DB
	reader      *chain.Reader
	signer      signer.Signer
	chainID     uint64
	callTimeout time.Duration // one fee read or send: rpc_read_timeout
	logger      *slog.Logger
	now         func() time.Time

	mu   sync.Mutex
	fees map[uuid.UUID]Fees // slot → the fees of its last send by this process
}

// New returns the queue of the operator of signer on chainID.
func New(db DB, reader *chain.Reader, s signer.Signer, chainID uint64, callTimeout time.Duration, logger *slog.Logger, now func() time.Time) *Queue {
	return &Queue{db: db, reader: reader, signer: s, chainID: chainID, callTimeout: callTimeout, logger: logger, now: now,
		fees: map[uuid.UUID]Fees{}}
}

// ChainID is the chain of the queue.
func (qu *Queue) ChainID() uint64 { return qu.chainID }

// Address is the operator's address.
func (qu *Queue) Address() common.Address { return qu.signer.Address() }

// Reader is the chain reader of the queue, for receipts and calls on the current endpoint.
func (qu *Queue) Reader() *chain.Reader { return qu.reader }

// Reserve takes the next nonce of the operator inside the caller's transaction q: the operator_accounts row is
// locked with SELECT … FOR UPDATE until the caller commits; the slot is written PLANNED. Exactly one of
// authorizationID (DEBIT) and returnRowID (REFUND) is set. No chain call.
func (qu *Queue) Reserve(ctx context.Context, q *repository.Queries, purpose string, authorizationID, returnRowID *uuid.UUID) (Slot, error) {
	chainID := int64(qu.chainID)
	operator := qu.signer.Address().Bytes()
	next, err := q.LockOperatorAccount(ctx, repository.LockOperatorAccountParams{ChainID: chainID, Address: operator})
	if err != nil {
		return Slot{}, fmt.Errorf("lock the operator account: %w", err)
	}
	if err := q.SetOperatorNextNonce(ctx, repository.SetOperatorNextNonceParams{
		ChainID: chainID, Address: operator, NextNonce: next + 1,
	}); err != nil {
		return Slot{}, err
	}
	id, err := q.InsertPlannedOperatorTx(ctx, repository.InsertPlannedOperatorTxParams{
		ChainID: chainID, OperatorAddress: operator, Nonce: next, Purpose: purpose,
		AuthorizationID: authorizationID, ReturnRowID: returnRowID, CreatedAt: qu.now(),
	})
	if err != nil {
		return Slot{}, err
	}
	return Slot{ID: id, Nonce: uint64(next)}, nil
}

// Call is one operator transaction to send.
type Call struct {
	Purpose string
	To      common.Address
	Data    []byte
	Gas     uint64
}

// pendingBlock is the part of the pending block the fee needs.
type pendingBlock struct {
	BaseFee *hexutil.Big `json:"baseFeePerGas"`
}

// Send signs c in the slot, stores the hash as SENT, then calls eth_sendRawTransaction. previous is the current hash
// of a slot that is sent again: it moves to replaced_hashes. An error is returned only before the hash is stored:
// nothing was sent. Any error of the send itself counts as sent (EC-15): the transaction may be on the network.
//
// Fees (§3.2, ADR-10): the tip of eth_maxPriorityFeePerGas; maxFeePerGas = 2 × base fee of the pending block + the
// tip; the gas limit is fixed, no eth_estimateGas.
func (qu *Queue) Send(ctx context.Context, slot Slot, c Call, previous *common.Hash) (common.Hash, error) {
	return qu.SendWithFloor(ctx, slot, c, previous, nil)
}

// Fees are the two fee fields of an EIP-1559 transaction, in wei.
type Fees struct {
	Tip, Cap *big.Int
}

// Bumped returns both fees raised by percent, rounded up: the floor of a replacement in the same slot (ADR-10).
func (f Fees) Bumped(percent int) Fees {
	up := func(v *big.Int) *big.Int {
		n := new(big.Int).Mul(v, big.NewInt(int64(100+percent)))
		n.Add(n, big.NewInt(99))
		return n.Quo(n, big.NewInt(100))
	}
	return Fees{Tip: up(f.Tip), Cap: up(f.Cap)}
}

// SendWithFloor is Send with fees at least floor: a replacement pays the bumped fees of the transaction it replaces
// when they are above the fees of the market.
func (qu *Queue) SendWithFloor(ctx context.Context, slot Slot, c Call, previous *common.Hash, floor *Fees) (common.Hash, error) {
	ep := qu.reader.Endpoint()
	var tip hexutil.Big
	var block *pendingBlock
	batch := []rpc.BatchElem{
		{Method: "eth_maxPriorityFeePerGas", Result: &tip},
		{Method: "eth_getBlockByNumber", Args: []any{chain.PendingTag, false}, Result: &block},
	}
	fctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	err := ep.Client.Client().BatchCallContext(fctx, batch)
	cancel()
	for _, b := range batch {
		err = errors.Join(err, b.Error)
	}
	if err != nil {
		return common.Hash{}, fmt.Errorf("fees: %s: %s", ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	if block == nil || block.BaseFee == nil {
		return common.Hash{}, fmt.Errorf("fees: %s: the pending block has no base fee", ep.Name)
	}
	tipCap := (*big.Int)(&tip)
	feeCap := new(big.Int).Mul((*big.Int)(block.BaseFee), big.NewInt(2))
	feeCap.Add(feeCap, tipCap)
	if floor != nil {
		if floor.Tip.Cmp(tipCap) > 0 {
			tipCap = floor.Tip
		}
		if floor.Cap.Cmp(feeCap) > 0 {
			feeCap = floor.Cap
		}
		if tipCap.Cmp(feeCap) > 0 {
			feeCap = tipCap
		}
	}

	to := c.To
	chainID := new(big.Int).SetUint64(qu.chainID)
	signed, err := qu.signer.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID: chainID, Nonce: slot.Nonce, GasTipCap: tipCap, GasFeeCap: feeCap, Gas: c.Gas,
		To: &to, Value: new(big.Int), Data: c.Data,
	}), chainID)
	if err != nil {
		return common.Hash{}, err
	}
	hash := signed.Hash()
	qu.mu.Lock()
	qu.fees[slot.ID] = Fees{Tip: new(big.Int).Set(tipCap), Cap: new(big.Int).Set(feeCap)}
	qu.mu.Unlock()

	// The hash is stored before the send: a restart finds every hash that may be on the network.
	q := repository.New(qu.db)
	var n int64
	if previous == nil {
		n, err = q.MarkOperatorTxSent(ctx, repository.MarkOperatorTxSentParams{ID: slot.ID, TxHash: hash.Bytes(), Purpose: c.Purpose})
	} else {
		n, err = q.ReplaceOperatorTxHash(ctx, repository.ReplaceOperatorTxHashParams{
			ID: slot.ID, TxHash: hash.Bytes(), OldHash: previous.Bytes(), Purpose: c.Purpose,
		})
	}
	if err != nil || n != 1 {
		if err == nil {
			err = errors.New("the slot changed")
		}
		return common.Hash{}, fmt.Errorf("store the hash: %s", decision.ErrorDetail(err))
	}

	sctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	err = ep.Client.SendTransaction(sctx, signed)
	cancel()
	switch {
	case err != nil && errors.Is(ctx.Err(), context.Canceled):
		// Shutdown: the failure is the cancellation itself and is not logged, as in the tracker and the listener.
		// The send is still treated as sent; the hash is stored for the tracker.
	case err != nil:
		qu.logger.WarnContext(ctx, "operator transaction send failed; treated as sent", "purpose", c.Purpose,
			"endpoint", ep.Name, "nonce", slot.Nonce, "tx_hash", hash.Hex(), "error", qu.reader.Describe(err, qu.callTimeout))
	default:
		qu.logger.DebugContext(ctx, "operator transaction sent", "purpose", c.Purpose, "endpoint", ep.Name,
			"nonce", slot.Nonce, "tx_hash", hash.Hex())
	}
	return hash, nil
}

// Receipt reads the receipt of hash on the current endpoint: nil when there is none yet or the read fails.
func (qu *Queue) Receipt(ctx context.Context, hash common.Hash) *types.Receipt {
	ep := qu.reader.Endpoint()
	rctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	r, err := ep.Client.TransactionReceipt(rctx, hash)
	if err != nil {
		return nil
	}
	return r
}

// NonceCount is the operator's transaction count at the latest block: a slot below it is used on chain.
func (qu *Queue) NonceCount(ctx context.Context) (uint64, error) {
	ep := qu.reader.Endpoint()
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	n, err := ep.Client.NonceAt(cctx, qu.signer.Address(), nil)
	if err != nil {
		return 0, fmt.Errorf("transaction count: %s: %s", ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	return n, nil
}

// SentFees returns the fees of the last send of a slot by this process, nil when it sent none.
func (qu *Queue) SentFees(id uuid.UUID) *Fees {
	qu.mu.Lock()
	defer qu.mu.Unlock()
	f, ok := qu.fees[id]
	if !ok {
		return nil
	}
	return &f
}

// PendingFees returns the fees of a transaction the node still knows, nil when it knows none.
func (qu *Queue) PendingFees(ctx context.Context, hash common.Hash) *Fees {
	ep := qu.reader.Endpoint()
	var tx *struct {
		Tip *hexutil.Big `json:"maxPriorityFeePerGas"`
		Cap *hexutil.Big `json:"maxFeePerGas"`
	}
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	if err := ep.Client.Client().CallContext(cctx, &tx, "eth_getTransactionByHash", hash); err != nil || tx == nil || tx.Tip == nil || tx.Cap == nil {
		return nil
	}
	return &Fees{Tip: (*big.Int)(tx.Tip), Cap: (*big.Int)(tx.Cap)}
}

// BlockHash returns the hash of the block number on chain now; zero when the chain has no such block.
func (qu *Queue) BlockHash(ctx context.Context, number uint64) (common.Hash, error) {
	ep := qu.reader.Endpoint()
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	var head *struct {
		Hash common.Hash `json:"hash"`
	}
	if err := ep.Client.Client().CallContext(cctx, &head, "eth_getBlockByNumber", hexutil.EncodeUint64(number), false); err != nil {
		return common.Hash{}, fmt.Errorf("block %d: %s: %s", number, ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	if head == nil {
		return common.Hash{}, nil
	}
	return head.Hash, nil
}

// Head returns the number and timestamp of the latest block.
func (qu *Queue) Head(ctx context.Context) (number, timestamp uint64, err error) {
	ep := qu.reader.Endpoint()
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	h, err := ep.Client.HeaderByNumber(cctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("latest block: %s: %s", ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	return h.Number.Uint64(), h.Time, nil
}

// Balance returns the native balance of the operator in wei.
func (qu *Queue) Balance(ctx context.Context) (*big.Int, error) {
	ep := qu.reader.Endpoint()
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	b, err := ep.Client.BalanceAt(cctx, qu.signer.Address(), nil)
	if err != nil {
		return nil, fmt.Errorf("operator balance: %s: %s", ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	return b, nil
}

// BlockTime returns the timestamp of block number; an error when the chain has no header for it yet.
func (qu *Queue) BlockTime(ctx context.Context, number *big.Int) (uint64, error) {
	ep := qu.reader.Endpoint()
	cctx, cancel := context.WithTimeout(ctx, qu.callTimeout)
	defer cancel()
	h, err := ep.Client.HeaderByNumber(cctx, number)
	if err != nil {
		return 0, fmt.Errorf("block %s: %s: %s", number, ep.Name, qu.reader.Describe(err, qu.callTimeout))
	}
	return h.Time, nil
}
