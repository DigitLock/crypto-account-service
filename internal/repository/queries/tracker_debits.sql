-- Debit side of the tracker of card-auth (SRS - Card Spend UC-3), role cas_card_auth.

-- name: ListAuthorizationsToTrack :many
-- Every authorization of this chain that is not final: APPROVED, DEBIT_SUBMITTED, TIMED_OUT, LATE_DEBIT, and RECEIVED
-- (whose chain is not known before its card is read). An authorization with a slot of another chain or another
-- operator key is never touched.
SELECT a.id, a.tenant_id, a.auth_id, a.chain_auth_id, a.status, a.wallet_address, COALESCE(a.token_amount::text, '')::text AS token_amount,
       a.debited_amount::text AS debited_amount, a.valid_until, a.deadline_at
FROM authorizations a
WHERE a.status IN ('APPROVED', 'DEBIT_SUBMITTED', 'TIMED_OUT', 'LATE_DEBIT', 'RECEIVED')
  AND (a.chain_id = sqlc.arg(chain_id) OR a.chain_id IS NULL)
  AND NOT EXISTS (
      SELECT 1 FROM operator_txs o
      WHERE o.authorization_id = a.id AND (o.chain_id <> sqlc.arg(chain_id) OR o.operator_address <> sqlc.arg(operator_address)))
ORDER BY a.received_at, a.id;

-- name: ListAuthorizationTxs :many
-- The operator transactions of an authorization, newest slot first.
SELECT id, purpose, nonce, status, tx_hash, replaced_hashes, block_number, block_hash
FROM operator_txs
WHERE authorization_id = $1
ORDER BY nonce DESC;

-- name: MoveAuthorization :execrows
-- Tracker: a status change of an authorization from an expected status. A null value keeps the stored one.
UPDATE authorizations
SET status = sqlc.arg(to_status),
    decline_reason = COALESCE(sqlc.narg(decline_reason), decline_reason),
    debited_amount = COALESCE(sqlc.narg(debited_amount)::text::numeric, debited_amount),
    returned_amount = returned_amount + COALESCE(sqlc.narg(returned_add)::text::numeric, 0),
    decided_at = COALESCE(decided_at, sqlc.narg(decided_at)),
    valid_until = COALESCE(sqlc.narg(valid_until), valid_until)
WHERE id = sqlc.arg(id) AND status = sqlc.arg(from_status);

-- name: SetAuthorizationValidUntil :exec
-- Row 2: the new validUntil of a resubmitted debit.
UPDATE authorizations SET valid_until = sqlc.arg(valid_until) WHERE id = sqlc.arg(id);

-- name: GetLateDebitReturnStatus :one
-- Row 5: the status of the LATE_DEBIT return of an authorization.
SELECT status FROM returns WHERE authorization_id = $1 AND type = 'LATE_DEBIT';
