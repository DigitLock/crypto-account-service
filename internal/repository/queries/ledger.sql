-- name: LockLedgerWriter :exec
-- The single writer of ledger entries: seq order equals commit order (FR-114). Held to the end of the transaction.
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: LockConnectionForWrite :one
-- Shared lock: a delete of the connection waits for this transaction. No row: the connection is gone.
SELECT tenant_id, source_id
FROM connections
WHERE id = $1
FOR SHARE;

-- name: ListAssetAliases :many
SELECT native_asset, asset
FROM asset_aliases
WHERE source_id = sqlc.arg(source_id) AND native_asset = ANY(sqlc.arg(native_assets)::text[]);

-- name: InsertLedgerEntry :execrows
-- 0 rows: the idempotency key exists; the entry is skipped and never updated.
INSERT INTO ledger_entries (
    tenant_id, connection_id, stream, external_id, leg, group_id, type, direction,
    asset, native_asset, amount, occurred_at, raw
) VALUES (
    sqlc.arg(tenant_id), sqlc.arg(connection_id), sqlc.arg(stream), sqlc.arg(external_id), sqlc.arg(leg),
    sqlc.arg(group_id), sqlc.arg(type), sqlc.arg(direction), sqlc.arg(asset), sqlc.arg(native_asset),
    (sqlc.arg(amount)::text)::numeric, sqlc.arg(occurred_at), sqlc.arg(raw)
)
ON CONFLICT (connection_id, stream, external_id, leg) DO NOTHING;

-- name: UpdateStreamCursor :execrows
UPDATE sync_cursors
SET cursor = sqlc.arg(cursor), mode = sqlc.arg(mode)
WHERE connection_id = sqlc.arg(connection_id) AND stream = sqlc.arg(stream);

-- name: InsertBalanceSnapshot :one
INSERT INTO balance_snapshots (connection_id, taken_at, created_at)
VALUES ($1, $2, $3)
RETURNING id;

-- name: InsertSnapshotBalance :exec
INSERT INTO snapshot_balances (snapshot_id, account_type, native_asset, asset, free, locked)
VALUES (
    sqlc.arg(snapshot_id), sqlc.arg(account_type), sqlc.arg(native_asset), sqlc.arg(asset),
    (sqlc.arg(free)::text)::numeric, (sqlc.arg(locked)::text)::numeric
);
