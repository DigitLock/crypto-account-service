-- name: ListBalanceConnections :many
-- The selected connections of a tenant in the order of ListConnections, each with its newest snapshot by
-- import time and the health of its balance streams (family balances).
SELECT c.id, s.code AS source_code, s.kind AS source_kind, s.config AS source_config,
       -- The zero UUID and the epoch stand for "no snapshot": sqlc reads these columns as not null.
       COALESCE(snap.id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS snapshot_id,
       COALESCE(snap.taken_at, 'epoch'::timestamptz)::timestamptz AS taken_at,
       health.streams, health.succeeded,
       COALESCE(health.oldest_success, 'epoch'::timestamptz)::timestamptz AS oldest_success
FROM connections c
JOIN sources s ON s.id = c.source_id
LEFT JOIN LATERAL (
    SELECT bs.id, bs.taken_at
    FROM balance_snapshots bs
    WHERE bs.connection_id = c.id
    ORDER BY bs.created_at DESC, bs.id DESC
    LIMIT 1
) snap ON true
CROSS JOIN LATERAL (
    SELECT count(*) AS streams, count(sc.last_success_at) AS succeeded, min(sc.last_success_at) AS oldest_success
    FROM sync_cursors sc
    WHERE sc.connection_id = c.id AND split_part(sc.stream, ':', 1) = 'balances'
) health
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(owner_ref)::text IS NULL OR c.owner_ref = sqlc.narg(owner_ref)::text)
  AND (sqlc.narg(connection_id)::uuid IS NULL OR c.id = sqlc.narg(connection_id)::uuid)
ORDER BY c.created_at, c.id;

-- name: ListSnapshotBalances :many
-- Amounts as plain decimals: trim_scale removes trailing zeros, numeric text has no exponent.
SELECT snapshot_id, account_type, asset, native_asset,
       trim_scale(free)::text AS free, trim_scale(locked)::text AS locked
FROM snapshot_balances
WHERE snapshot_id = ANY(sqlc.arg(snapshot_ids)::uuid[])
ORDER BY snapshot_id, account_type, native_asset;

-- name: ListLedgerPage :many
-- One page of the ledger of a tenant above after_seq in ascending seq, the filters combined with AND.
-- page_limit is the page size plus one, to know whether more entries follow. No OFFSET.
SELECT le.seq, le.connection_id, le.type, le.leg, le.direction, le.asset, le.native_asset,
       trim_scale(le.amount)::text AS amount, le.group_id, le.external_id, le.occurred_at
FROM ledger_entries le
JOIN connections c ON c.id = le.connection_id
WHERE le.tenant_id = sqlc.arg(tenant_id)
  AND le.seq > sqlc.arg(after_seq)
  AND (sqlc.narg(owner_ref)::text IS NULL OR c.owner_ref = sqlc.narg(owner_ref)::text)
  AND (sqlc.narg(connection_id)::uuid IS NULL OR le.connection_id = sqlc.narg(connection_id)::uuid)
  AND (sqlc.narg(types)::text[] IS NULL OR le.type = ANY(sqlc.narg(types)::text[]))
  AND (sqlc.narg(occurred_from)::timestamptz IS NULL OR le.occurred_at >= sqlc.narg(occurred_from)::timestamptz)
  AND (sqlc.narg(occurred_to)::timestamptz IS NULL OR le.occurred_at < sqlc.narg(occurred_to)::timestamptz)
ORDER BY le.seq
LIMIT sqlc.arg(page_limit);

-- name: ConnectionExists :one
SELECT EXISTS (SELECT 1 FROM connections WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id))::bool;

-- name: LockConnectionForTrigger :one
-- First lock of TriggerSync: the connection row, then its cursors.
SELECT status, last_manual_sync_at
FROM connections
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
FOR UPDATE;

-- name: SetLastManualSync :exec
UPDATE connections
SET last_manual_sync_at = sqlc.arg(at)
WHERE id = sqlc.arg(id);

-- name: MakeStreamsDue :exec
-- Every stream of the connection is due now. The failure counters are not changed.
UPDATE sync_cursors
SET next_run_at = sqlc.arg(at)
WHERE connection_id = sqlc.arg(connection_id);
