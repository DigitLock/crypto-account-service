-- Returns of card-auth (SRS - Card Spend UC-2, UC-3 for returns), role cas_card_auth.
-- Amounts are passed and read as text: no float between the service and the database.

-- name: GetReturnForRequest :one
-- UC-2 step 2: a return of the tenant by return_id with what a repeated request needs.
SELECT r.id, r.request_hash, r.status, r.token_amount::text AS token_amount, a.auth_id
FROM returns r
JOIN authorizations a ON a.id = r.authorization_id
WHERE r.tenant_id = sqlc.arg(tenant_id) AND r.return_id = sqlc.arg(return_id);

-- name: LockAuthorizationForReturn :one
-- UC-2 steps 3 - 5: the authorization of the return, locked until the end of the transaction, so that two returns
-- of one authorization are accepted one after another.
SELECT id, status, debited_amount::text AS debited_amount, returned_amount::text AS returned_amount,
       COALESCE(fiat_amount::text, '')::text AS fiat_amount
FROM authorizations
WHERE tenant_id = sqlc.arg(tenant_id) AND auth_id = sqlc.arg(auth_id)
FOR UPDATE;

-- name: GetReturnedFiat :one
-- UC-2 step 5: the fiat amount of the returns of an authorization so far.
SELECT COALESCE(sum(fiat_amount), 0)::text AS returned
FROM returns
WHERE authorization_id = $1;

-- name: InsertTombstone :one
-- UC-2 step 3: the tombstone of a reversal for an unknown auth_id. A concurrent authorization with the same auth_id
-- is no row: the caller reads it.
INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, status, decline_reason, received_at, decided_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(auth_id), sqlc.arg(chain_auth_id), 'DECLINED', 'REVERSED_BEFORE_AUTH',
        sqlc.arg(now), sqlc.arg(now))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: InsertReturn :one
-- UC-2 steps 3, 4 and 6. A concurrent return with the same return_id is no row: the caller reads it.
INSERT INTO returns (tenant_id, authorization_id, return_id, chain_refund_id, type, request_hash, fiat_amount,
                     token_amount, status, created_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(authorization_id), sqlc.arg(return_id), sqlc.arg(chain_refund_id), sqlc.arg(type),
        sqlc.arg(request_hash), sqlc.narg(fiat_amount)::text::numeric, sqlc.arg(token_amount)::text::numeric,
        sqlc.arg(status), sqlc.arg(created_at))
ON CONFLICT DO NOTHING
RETURNING id;

-- name: AddReturnedAmount :exec
-- UC-2 step 6.
UPDATE authorizations
SET returned_amount = returned_amount + sqlc.arg(amount)::text::numeric
WHERE id = sqlc.arg(id);

-- name: ListReturnsToSend :many
-- Tracker: ACCEPTED returns, and RETRYING returns whose last attempt is return_retry_interval old.
SELECT r.id, r.chain_refund_id, r.token_amount::text AS token_amount, a.chain_auth_id
FROM returns r
JOIN authorizations a ON a.id = r.authorization_id
LEFT JOIN LATERAL (
    SELECT max(t.created_at) AS last_attempt FROM operator_txs t WHERE t.return_row_id = r.id
) l ON true
WHERE r.status = 'ACCEPTED'
   OR (r.status = 'RETRYING' AND (l.last_attempt IS NULL OR l.last_attempt <= sqlc.arg(due_before)::timestamptz))
ORDER BY r.created_at, r.id
LIMIT sqlc.arg(page_limit);

-- name: ListReturnsInFlight :many
-- Tracker: SUBMITTED and INCLUDED returns with the current transaction of their latest slot.
SELECT r.id, r.status, r.chain_refund_id, r.token_amount::text AS token_amount, a.chain_auth_id,
       COALESCE(t.id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS tx_id,
       t.tx_hash, COALESCE(t.nonce, -1)::bigint AS nonce, COALESCE(t.status, '')::text AS tx_status
FROM returns r
JOIN authorizations a ON a.id = r.authorization_id
LEFT JOIN LATERAL (
    SELECT o.id, o.tx_hash, o.nonce, o.status FROM operator_txs o
    WHERE o.return_row_id = r.id AND o.purpose = 'REFUND'
    ORDER BY o.created_at DESC, o.nonce DESC
    LIMIT 1
) t ON true
WHERE r.status IN ('SUBMITTED', 'INCLUDED')
ORDER BY r.created_at, r.id;

-- name: LockReturnToSend :one
-- Tracker: a return about to be sent, with the latest slot of an earlier attempt that was never sent.
SELECT r.status,
       COALESCE(p.id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS planned_id, COALESCE(p.nonce, -1)::bigint AS planned_nonce
FROM returns r
LEFT JOIN LATERAL (
    SELECT o.id, o.nonce FROM operator_txs o
    WHERE o.return_row_id = r.id AND o.status = 'PLANNED'
    ORDER BY o.created_at DESC
    LIMIT 1
) p ON true
WHERE r.id = $1
FOR UPDATE OF r;

-- name: SetReturnStatus :execrows
-- Tracker: a status change of a return from one of the expected statuses; attempts grows with each send.
UPDATE returns
SET status = sqlc.arg(status), attempts = attempts + sqlc.arg(attempt)::int
WHERE id = sqlc.arg(id) AND status = ANY (sqlc.arg(from_statuses)::text[]);

-- name: SetOperatorTxStatus :exec
-- Tracker: the status of an operator transaction; block number and hash only from a sealed receipt.
UPDATE operator_txs
SET status = sqlc.arg(status),
    block_number = COALESCE(sqlc.narg(block_number), block_number),
    block_hash = COALESCE(sqlc.narg(block_hash), block_hash)
WHERE id = sqlc.arg(id);

-- name: CountReturnsNotConfirmed :one
-- returns_not_confirmed (SRS - Card Spend §2.5.1).
SELECT count(*) FROM returns WHERE status IN ('ACCEPTED', 'SUBMITTED', 'INCLUDED', 'RETRYING');

-- name: CloseOpenReturns :one
-- FR-14: the open returns of a DEBIT_LOST authorization close as NOTHING_TO_RETURN with no tokens; returned_amount
-- drops by their amounts (UC-3 row 3, st6b). Returns the number closed.
WITH open_returns AS (
    SELECT id, token_amount FROM returns
    WHERE authorization_id = sqlc.arg(authorization_id) AND status IN ('ACCEPTED', 'RETRYING', 'SUBMITTED')
    FOR UPDATE
), closed AS (
    UPDATE returns r SET status = 'NOTHING_TO_RETURN', token_amount = 0
    FROM open_returns o WHERE r.id = o.id
    RETURNING o.token_amount
)
UPDATE authorizations a
SET returned_amount = a.returned_amount - (SELECT COALESCE(sum(c.token_amount), 0) FROM closed c)
WHERE a.id = sqlc.arg(authorization_id)
RETURNING (SELECT count(*) FROM closed)::bigint AS closed;
