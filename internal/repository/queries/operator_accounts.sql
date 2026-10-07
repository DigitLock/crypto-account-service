-- name: LockOperatorAccount :one
-- The nonce row of an operator on a network, locked until the end of the transaction.
SELECT next_nonce
FROM operator_accounts
WHERE chain_id = $1 AND address = $2
FOR UPDATE;

-- name: InsertOperatorAccount :exec
INSERT INTO operator_accounts (chain_id, address, next_nonce)
VALUES ($1, $2, $3);

-- name: SetOperatorNextNonce :exec
UPDATE operator_accounts
SET next_nonce = sqlc.arg(next_nonce)
WHERE chain_id = sqlc.arg(chain_id) AND address = sqlc.arg(address);
