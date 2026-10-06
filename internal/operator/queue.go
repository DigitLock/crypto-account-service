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
}

// New returns the queue of the operator of signer on chainID.
func New(db DB, reader *chain.Reader, s signer.Signer, chainID uint64, callTimeout time.Duration, logger *slog.Logger, now func() time.Time) *Queue {
	return &Queue{db: db, reader: reader, signer: s, chainID: chainID, callTimeout: callTimeout, logger: logger, now: now}
}

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

	// The hash is stored before the send: a restart finds every hash that may be on the network.
	q := repository.New(qu.db)
	var n int64
	if previous == nil {
		n, err = q.MarkOperatorTxSent(ctx, repository.MarkOperatorTxSentParams{ID: slot.ID, TxHash: hash.Bytes()})
	} else {
		n, err = q.ReplaceOperatorTxHash(ctx, repository.ReplaceOperatorTxHashParams{ID: slot.ID, TxHash: hash.Bytes(), OldHash: previous.Bytes()})
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
	if err != nil {
		qu.logger.WarnContext(ctx, "operator transaction send failed; treated as sent", "purpose", c.Purpose,
			"endpoint", ep.Name, "nonce", slot.Nonce, "tx_hash", hash.Hex(), "error", qu.reader.Describe(err, qu.callTimeout))
	} else {
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
