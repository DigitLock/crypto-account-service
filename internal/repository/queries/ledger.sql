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

-- name: UpsertBalanceCheckpoint :one
-- The balance checkpoint of a connection and native asset, in the transaction of its page (SRS - Core Connector
-- contract; S3 D-5): ledger_total = sum of IN minus sum of OUT of the entries of the connection with that native
-- asset, those of the page included; gap = balance - ledger_total. It replaces the previous row.
WITH total AS (
    SELECT COALESCE(SUM(CASE WHEN direction = 'IN' THEN amount ELSE -amount END), 0) AS ledger_total
    FROM ledger_entries
    WHERE connection_id = sqlc.arg(connection_id) AND native_asset = sqlc.arg(native_asset)::text
), balance AS (
    SELECT (sqlc.arg(free)::text)::numeric + (sqlc.arg(locked)::text)::numeric AS balance
)
INSERT INTO balance_checkpoints (
    connection_id, native_asset, asset, block_number, block_hash, taken_at, balance, ledger_total, gap, checked_at
)
SELECT sqlc.arg(connection_id), sqlc.arg(native_asset)::text, sqlc.arg(asset), sqlc.narg(block_number)::bigint,
       sqlc.narg(block_hash)::text, sqlc.arg(taken_at), balance.balance, total.ledger_total,
       balance.balance - total.ledger_total, sqlc.arg(checked_at)
FROM total, balance
ON CONFLICT (connection_id, native_asset) DO UPDATE SET
    asset = EXCLUDED.asset, block_number = EXCLUDED.block_number, block_hash = EXCLUDED.block_hash,
    taken_at = EXCLUDED.taken_at, balance = EXCLUDED.balance, ledger_total = EXCLUDED.ledger_total,
    gap = EXCLUDED.gap, checked_at = EXCLUDED.checked_at
RETURNING trim_scale(ledger_total)::text AS ledger_total, trim_scale(gap)::text AS gap;
