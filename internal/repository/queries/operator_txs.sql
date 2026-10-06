-- Operator queue of card-auth (SRS - Card Spend §2.4 operator_txs, ADR-10): one nonce sequence for debits and
-- refunds, role cas_card_auth.

-- name: InsertPlannedOperatorTx :one
-- The intent of an operator transaction with its reserved nonce, written before anything is sent (FR-5).
INSERT INTO operator_txs (chain_id, operator_address, nonce, purpose, authorization_id, return_row_id, status, created_at)
VALUES (sqlc.arg(chain_id), sqlc.arg(operator_address), sqlc.arg(nonce), sqlc.arg(purpose), sqlc.narg(authorization_id),
        sqlc.narg(return_row_id), 'PLANNED', sqlc.arg(created_at))
RETURNING id;

-- name: MarkOperatorTxSent :execrows
-- The hash of the signed transaction, stored before it is sent. purpose is RELEASE when a debit slot is released.
UPDATE operator_txs
SET tx_hash = sqlc.arg(tx_hash), status = 'SENT', purpose = sqlc.arg(purpose)
WHERE id = sqlc.arg(id) AND status = 'PLANNED';

-- name: ReplaceOperatorTxHash :execrows
-- A new transaction in the same nonce slot — a replacement, a release, a resubmission of a dropped debit: the old hash
-- moves to replaced_hashes, the new one is current, the block of the old one is cleared.
UPDATE operator_txs
SET replaced_hashes = array_append(replaced_hashes, tx_hash), tx_hash = sqlc.arg(tx_hash), status = 'SENT',
    purpose = sqlc.arg(purpose), block_number = NULL, block_hash = NULL
WHERE id = sqlc.arg(id) AND tx_hash = sqlc.arg(old_hash) AND status IN ('SENT', 'INCLUDED', 'REVERTED');

-- name: ListUnminedOperatorTxs :many
-- Tracker, rows 7 - 9: every operator transaction of this chain and operator not mined yet, with what a replacement
-- or a release needs. Rows of another chain or another operator key are never touched.
SELECT t.id, t.purpose, t.nonce, t.status, t.tx_hash, t.replaced_hashes, t.created_at, t.authorization_id, t.return_row_id,
       a.valid_until, a.status AS authorization_status, a.chain_auth_id, a.wallet_address,
       COALESCE(a.token_amount::text, '')::text AS token_amount,
       r.chain_refund_id, COALESCE(r.token_amount::text, '')::text AS refund_amount, ra.chain_auth_id AS refund_auth_id
FROM operator_txs t
LEFT JOIN authorizations a ON a.id = t.authorization_id
LEFT JOIN returns r ON r.id = t.return_row_id
LEFT JOIN authorizations ra ON ra.id = r.authorization_id
WHERE t.chain_id = sqlc.arg(chain_id) AND t.operator_address = sqlc.arg(operator_address)
  AND (t.status = 'SENT' OR (t.status = 'PLANNED' AND t.purpose = 'DEBIT'))
ORDER BY t.nonce;

-- name: OldestUnminedOperatorTx :one
-- operator_tx_pending_seconds (SRS - Card Spend §2.5.1): the created_at of the oldest operator transaction not mined.
SELECT COALESCE(min(created_at), 'epoch'::timestamptz)::timestamptz AS oldest
FROM operator_txs
WHERE chain_id = sqlc.arg(chain_id) AND operator_address = sqlc.arg(operator_address) AND status IN ('PLANNED', 'SENT');

-- name: SetOperatorTxBlock :exec
-- Tracker, reorg: the block of an included transaction as the chain shows it now; null when only preconfirmed.
UPDATE operator_txs
SET block_number = sqlc.narg(block_number), block_hash = sqlc.narg(block_hash)
WHERE id = sqlc.arg(id);

-- name: SetOperatorTxResolved :exec
-- Tracker: a slot used on chain by an earlier hash of the row: that hash becomes current again with its purpose.
UPDATE operator_txs
SET tx_hash = sqlc.arg(tx_hash), purpose = sqlc.arg(purpose), status = sqlc.arg(status),
    block_number = sqlc.narg(block_number), block_hash = sqlc.narg(block_hash)
WHERE id = sqlc.arg(id);
