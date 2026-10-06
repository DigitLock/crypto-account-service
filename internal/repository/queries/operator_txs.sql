-- Operator queue of card-auth (SRS - Card Spend §2.4 operator_txs, ADR-10): one nonce sequence for debits and
-- refunds, role cas_card_auth.

-- name: InsertPlannedOperatorTx :one
-- The intent of an operator transaction with its reserved nonce, written before anything is sent (FR-5).
INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, return_row_id, status, created_at)
VALUES (sqlc.arg(chain_id), sqlc.arg(operator_address), sqlc.arg(nonce), sqlc.arg(purpose), sqlc.narg(authorization_id),
        sqlc.narg(return_row_id), 'PLANNED', sqlc.arg(created_at))
RETURNING id;

-- name: MarkOperatorTxSent :execrows
-- The hash of the signed transaction, stored before it is sent.
UPDATE operator_txs
SET tx_hash = sqlc.arg(tx_hash), status = 'SENT'
WHERE id = sqlc.arg(id) AND status = 'PLANNED';

-- name: ReplaceOperatorTxHash :execrows
-- A new transaction in the same nonce slot: the old hash moves to replaced_hashes, the new one is current.
UPDATE operator_txs
SET replaced_hashes = array_append(replaced_hashes, tx_hash), tx_hash = sqlc.arg(tx_hash), status = 'SENT'
WHERE id = sqlc.arg(id) AND tx_hash = sqlc.arg(old_hash) AND status = 'SENT';
