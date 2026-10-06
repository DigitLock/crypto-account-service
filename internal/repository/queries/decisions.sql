-- Decision path of card-auth (SRS - Card Spend UC-1 steps 3 - 9), role cas_card_auth.
-- Decimals are passed and read as text: no float between the service and the database.

-- name: GetDecisionState :one
-- Step 3: an authorization of the tenant with what a repeated request needs. A tombstone has no request_hash.
-- tx_hash by the rule of SRS - Card Spend §2.1.4.
SELECT a.id, a.status, a.decline_reason, a.request_hash, a.token,
       COALESCE(a.token_amount::text, '')::text AS token_amount, COALESCE(trim_scale(a.rate)::text, '')::text AS rate,
       a.buffer_bps, COALESCE('0x' || encode(d.tx_hash, 'hex'), '')::text AS tx_hash
FROM authorizations a
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.authorization_id = a.id AND t.purpose = 'DEBIT' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) d ON true
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.auth_id = sqlc.arg(auth_id);

-- name: GetDecisionStateByID :one
-- GetDecisionState by the primary key: the state of an authorization that left RECEIVED without the engine.
SELECT a.id, a.status, a.decline_reason, a.request_hash, a.token,
       COALESCE(a.token_amount::text, '')::text AS token_amount, COALESCE(trim_scale(a.rate)::text, '')::text AS rate,
       a.buffer_bps, COALESCE('0x' || encode(d.tx_hash, 'hex'), '')::text AS tx_hash
FROM authorizations a
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.authorization_id = a.id AND t.purpose = 'DEBIT' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) d ON true
WHERE a.id = $1;

-- name: InsertReceivedAuthorization :one
-- Step 4. A concurrent insert of the same auth_id is no row: the caller reads the winner's row.
INSERT INTO authorizations (tenant_id, auth_id, chain_auth_id, request_hash, fiat_amount, fiat_currency, merchant,
                            status, received_at, deadline_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(auth_id), sqlc.arg(chain_auth_id), sqlc.arg(request_hash),
        sqlc.arg(fiat_amount)::text::numeric, sqlc.arg(fiat_currency), sqlc.arg(merchant), 'RECEIVED',
        sqlc.arg(received_at), sqlc.arg(deadline_at))
ON CONFLICT (tenant_id, auth_id) DO NOTHING
RETURNING id;

-- name: InsertAuthorizationEvent :exec
-- One row per status change; from_status is null for the first.
INSERT INTO authorization_events (authorization_id, from_status, to_status, reason, created_at)
VALUES (sqlc.arg(authorization_id), sqlc.narg(from_status), sqlc.arg(to_status), sqlc.narg(reason), sqlc.arg(created_at));

-- name: GetCardForDecision :one
-- Step 6: the card of the tenant, its wallet and the chain of the wallet's connection.
SELECT k.id, k.status, k.daily_limit::text AS daily_limit, c.external_account AS wallet_address,
       COALESCE((s.config ->> 'chain_id')::bigint, 0)::bigint AS chain_id
FROM cards k
JOIN connections c ON c.id = k.connection_id
JOIN sources s ON s.id = c.source_id
WHERE k.tenant_id = sqlc.arg(tenant_id) AND k.card_ref = sqlc.arg(card_ref);

-- name: GetCardDaySpend :one
-- Step 8: token amounts of the card's approved authorizations received in [day_start, day_end).
SELECT COALESCE(sum(token_amount), 0)::text AS spent
FROM authorizations
WHERE card_id = sqlc.arg(card_id)
  AND status IN ('APPROVED', 'DEBIT_CONFIRMED', 'DEBIT_LOST')
  AND received_at >= sqlc.arg(day_start) AND received_at < sqlc.arg(day_end);

-- name: SetAuthorizationFacts :execrows
-- The card of step 6 and the quote of step 7, while the authorization is RECEIVED. A value not known yet is null.
UPDATE authorizations
SET card_id        = sqlc.narg(card_id),
    wallet_address = sqlc.narg(wallet_address),
    chain_id       = sqlc.narg(chain_id),
    rate           = sqlc.narg(rate)::text::numeric,
    buffer_bps     = sqlc.narg(buffer_bps),
    token          = sqlc.narg(token),
    token_amount   = sqlc.narg(token_amount)::text::numeric
WHERE id = sqlc.arg(id) AND status = 'RECEIVED';

-- name: DeclineReceivedAuthorization :execrows
-- A decline of steps 5 - 9: RECEIVED -> DECLINED. No row: the authorization is no longer RECEIVED.
UPDATE authorizations
SET status = 'DECLINED', decline_reason = sqlc.arg(decline_reason), decided_at = sqlc.arg(decided_at)
WHERE id = sqlc.arg(id) AND status = 'RECEIVED';
