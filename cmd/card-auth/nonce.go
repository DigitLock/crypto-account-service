package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/DigitLock/crypto-account-service/internal/chain"
	"github.com/DigitLock/crypto-account-service/internal/repository"
)

const nonceCheck = "next_nonce of the operator"

// beginner starts transactions; *pgxpool.Pool satisfies it.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// syncNonce is the nonce start check of SRS — Card Spend §3.2 (ADR-10, T107): count is the transaction count
// of the operator at the pending block tag. A missing operator_accounts row is created with count; a next_nonce
// below count is raised to it; a next_nonce above count is kept: its slots are reserved, the tracker resolves them.
func syncNonce(ctx context.Context, db beginner, logger *slog.Logger, chainID uint64, operator common.Address, count uint64) error {
	if chainID > math.MaxInt64 || count > math.MaxInt64 {
		return &chain.StartCheckError{Check: nonceCheck, Detail: "chain ID or transaction count out of range"}
	}
	id, n := int64(chainID), int64(count)
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		q := repository.New(tx)
		next, err := q.LockOperatorAccount(ctx, repository.LockOperatorAccountParams{ChainID: id, Address: operator.Bytes()})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if err := q.InsertOperatorAccount(ctx, repository.InsertOperatorAccountParams{
				ChainID: id, Address: operator.Bytes(), NextNonce: n,
			}); err != nil {
				return err
			}
			logger.InfoContext(ctx, "operator account created with the chain's transaction count",
				"chain_id", chainID, "operator", operator.Hex(), "next_nonce", n)
			return nil
		case err != nil:
			return err
		case next < n:
			if err := q.SetOperatorNextNonce(ctx, repository.SetOperatorNextNonceParams{
				ChainID: id, Address: operator.Bytes(), NextNonce: n,
			}); err != nil {
				return err
			}
			logger.InfoContext(ctx, "operator next_nonce raised to the chain's transaction count",
				"chain_id", chainID, "operator", operator.Hex(), "from", next, "to", n)
		}
		return nil
	})
	if err != nil {
		return &chain.StartCheckError{Check: nonceCheck, Detail: databaseDetail(err)}
	}
	return nil
}

// databaseDetail keeps the message of a PostgreSQL error, which names no host and no user; any other error,
// such as a connection failure, which names them, becomes a fixed text.
func databaseDetail(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("database: %s (SQLSTATE %s)", pgErr.Message, pgErr.Code)
	}
	return "database: cannot read or write operator_accounts"
}
