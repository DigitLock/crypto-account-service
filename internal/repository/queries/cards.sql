-- name: InsertCard :one
-- A second card with the same card_ref in the tenant is no row: the caller compares it with the request.
INSERT INTO cards (tenant_id, card_ref, owner_ref, connection_id, status, daily_limit, created_at, updated_at)
VALUES (sqlc.arg(tenant_id), sqlc.arg(card_ref), sqlc.arg(owner_ref), sqlc.arg(connection_id), 'ACTIVE',
        sqlc.arg(daily_limit)::text::numeric, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (tenant_id, card_ref) DO NOTHING
RETURNING id;

-- name: GetCard :one
-- One card of a tenant with the address of its wallet. Another tenant's card is no row.
SELECT k.id, k.card_ref, k.owner_ref, k.connection_id, c.external_account AS wallet_address, k.status,
       k.daily_limit::text AS daily_limit, k.created_at, k.updated_at
FROM cards k
JOIN connections c ON c.id = k.connection_id
WHERE k.tenant_id = sqlc.arg(tenant_id) AND k.card_ref = sqlc.arg(card_ref);

-- name: LockCard :one
-- The card row of UpdateCard, locked until the end of the transaction.
SELECT id, status, daily_limit::text AS daily_limit
FROM cards
WHERE tenant_id = sqlc.arg(tenant_id) AND card_ref = sqlc.arg(card_ref)
FOR UPDATE;

-- name: UpdateCard :exec
UPDATE cards
SET daily_limit = sqlc.arg(daily_limit)::text::numeric, status = sqlc.arg(status), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ListCardsPage :many
-- One page in the order created_at, card_ref after the position; a range of cards_tenant_created_idx.
SELECT k.id, k.card_ref, k.owner_ref, k.connection_id, c.external_account AS wallet_address, k.status,
       k.daily_limit::text AS daily_limit, k.created_at, k.updated_at
FROM cards k
JOIN connections c ON c.id = k.connection_id
WHERE k.tenant_id = sqlc.arg(tenant_id)
  AND (k.created_at, k.card_ref) > (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_card_ref)::text)
  AND (sqlc.narg(owner_ref)::text IS NULL OR k.owner_ref = sqlc.narg(owner_ref)::text)
ORDER BY k.created_at, k.card_ref
LIMIT sqlc.arg(page_limit);

-- name: ConnectionHasCards :one
-- EC-114: any card bound to the connection.
SELECT EXISTS (SELECT 1 FROM cards WHERE connection_id = $1);
