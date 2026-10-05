-- Reads of the authorizations by server (SRS - Core §2.1.1): read-only, role cas_server.
-- Decimals without trailing zeros; base units as integer strings; an absent value is the empty string.
-- tx_hash (SRS - Card Spend §2.1.4): of the DEBIT row, or of the REFUND rows of a return, that is INCLUDED or
-- CONFIRMED; else of the newest such row with a hash; else empty. Read in the same statement, never per row.

-- name: GetAuthorization :one
SELECT a.id, a.auth_id, a.status, a.decline_reason, COALESCE(trim_scale(a.fiat_amount)::text, '')::text AS amount, a.fiat_currency,
       a.token, COALESCE(a.token_amount::text, '')::text AS token_amount, COALESCE(trim_scale(a.rate)::text, '')::text AS rate, a.buffer_bps,
       a.debited_amount::text AS debited_amount, a.returned_amount::text AS returned_amount,
       k.card_ref, a.received_at, a.decided_at, COALESCE('0x' || encode(d.tx_hash, 'hex'), '')::text AS tx_hash
FROM authorizations a
LEFT JOIN cards k ON k.id = a.card_id
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.authorization_id = a.id AND t.purpose = 'DEBIT' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) d ON true
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.auth_id = sqlc.arg(auth_id);

-- name: ListAuthorizationsPage :many
-- Order received_at descending, auth_id, after the position (after_received_at, after_auth_id). Filters
-- combine with AND; a null filter does not filter. A tombstone has no card: a card or owner filter skips it.
SELECT a.id, a.auth_id, a.status, a.decline_reason, COALESCE(trim_scale(a.fiat_amount)::text, '')::text AS amount, a.fiat_currency,
       a.token, COALESCE(a.token_amount::text, '')::text AS token_amount, COALESCE(trim_scale(a.rate)::text, '')::text AS rate, a.buffer_bps,
       a.debited_amount::text AS debited_amount, a.returned_amount::text AS returned_amount,
       k.card_ref, a.received_at, a.decided_at, COALESCE('0x' || encode(d.tx_hash, 'hex'), '')::text AS tx_hash
FROM authorizations a
LEFT JOIN cards k ON k.id = a.card_id
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.authorization_id = a.id AND t.purpose = 'DEBIT' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) d ON true
WHERE a.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(after_received_at)::timestamptz IS NULL
       OR a.received_at < sqlc.narg(after_received_at)::timestamptz
       OR (a.received_at = sqlc.narg(after_received_at)::timestamptz AND a.auth_id > sqlc.narg(after_auth_id)::text))
  AND (sqlc.narg(card_ref)::text IS NULL OR k.card_ref = sqlc.narg(card_ref)::text)
  AND (sqlc.narg(owner_ref)::text IS NULL OR k.owner_ref = sqlc.narg(owner_ref)::text)
  AND (sqlc.narg(status)::text IS NULL OR a.status = sqlc.narg(status)::text)
  AND (sqlc.narg(received_from)::timestamptz IS NULL OR a.received_at >= sqlc.narg(received_from)::timestamptz)
  AND (sqlc.narg(received_to)::timestamptz IS NULL OR a.received_at < sqlc.narg(received_to)::timestamptz)
ORDER BY a.received_at DESC, a.auth_id
LIMIT sqlc.arg(page_limit);

-- name: ListAuthorizationEvents :many
-- The status history in the order of the events.
SELECT to_status, reason, created_at
FROM authorization_events
WHERE authorization_id = $1
ORDER BY id;

-- name: ListAuthorizationReturns :many
SELECT r.return_id, r.type, r.status, r.token_amount::text AS token_amount, r.created_at,
       COALESCE('0x' || encode(x.tx_hash, 'hex'), '')::text AS tx_hash
FROM returns r
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.return_row_id = r.id AND t.purpose = 'REFUND' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) x ON true
WHERE r.authorization_id = $1
ORDER BY r.created_at, r.id;
