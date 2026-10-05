-- name: ListDueStreams :many
-- Streams due at now of connections that are ACTIVE or DEGRADED with an ACTIVE tenant; all connections,
-- or one when connection_id is set. The availability of the source is decided in Go (connector.Set).
SELECT sc.connection_id, sc.stream, sc.next_run_at
FROM sync_cursors sc
JOIN connections c ON c.id = sc.connection_id
JOIN tenants t ON t.id = c.tenant_id
WHERE sc.next_run_at <= sqlc.arg(now)
  AND c.status IN ('ACTIVE', 'DEGRADED')
  AND t.status = 'ACTIVE'
  AND (sqlc.narg(connection_id)::uuid IS NULL OR sc.connection_id = sqlc.narg(connection_id)::uuid)
ORDER BY sc.next_run_at, sc.connection_id, sc.stream;

-- name: GetSyncConnection :one
SELECT c.id, c.tenant_id, c.status, c.external_account, c.credentials_enc, c.kek_version,
       s.code AS source_code, s.kind AS source_kind, s.enabled AS source_enabled, s.config AS source_config
FROM connections c
JOIN sources s ON s.id = c.source_id
WHERE c.id = $1;

-- name: GetStreamCursor :one
SELECT mode, cursor
FROM sync_cursors
WHERE connection_id = $1 AND stream = $2;

-- name: LockConnectionStatus :one
-- First lock of the health transaction: the connection row, then its cursor.
SELECT tenant_id, status
FROM connections
WHERE id = $1
FOR UPDATE;

-- name: LockStreamFailures :one
SELECT consecutive_failures
FROM sync_cursors
WHERE connection_id = $1 AND stream = $2
FOR UPDATE;

-- name: RecordStreamSuccess :execrows
UPDATE sync_cursors
SET last_success_at = sqlc.arg(last_success_at), consecutive_failures = 0, last_error = NULL,
    next_run_at = sqlc.arg(next_run_at)
WHERE connection_id = sqlc.arg(connection_id) AND stream = sqlc.arg(stream);

-- name: RecordStreamFailure :execrows
UPDATE sync_cursors
SET last_error = sqlc.arg(last_error), consecutive_failures = sqlc.arg(consecutive_failures),
    next_run_at = sqlc.arg(next_run_at)
WHERE connection_id = sqlc.arg(connection_id) AND stream = sqlc.arg(stream);

-- name: AnyStreamOverThreshold :one
SELECT EXISTS (
    SELECT 1 FROM sync_cursors
    WHERE connection_id = sqlc.arg(connection_id) AND consecutive_failures >= sqlc.arg(threshold)::int
)::bool;

-- name: SetConnectionStatus :exec
UPDATE connections
SET status = sqlc.arg(status)
WHERE id = sqlc.arg(id);
