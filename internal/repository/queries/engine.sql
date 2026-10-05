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

-- name: ListDueKeyChecks :many
-- Connections with a key whose permissions were last checked at or before due_before (FR-119); ACTIVE or
-- DEGRADED with an ACTIVE tenant. The availability of the source is decided in Go. Never a wallet: no key.
SELECT c.id, c.permissions_checked_at
FROM connections c
JOIN tenants t ON t.id = c.tenant_id
WHERE c.credentials_enc IS NOT NULL
  AND c.status IN ('ACTIVE', 'DEGRADED')
  AND t.status = 'ACTIVE'
  AND (c.permissions_checked_at IS NULL OR c.permissions_checked_at <= sqlc.arg(due_before))
  AND (sqlc.narg(connection_id)::uuid IS NULL OR c.id = sqlc.narg(connection_id)::uuid)
ORDER BY c.permissions_checked_at NULLS FIRST, c.id;

-- name: SetPermissionsChecked :execrows
UPDATE connections
SET permissions_checked_at = sqlc.arg(checked_at)
WHERE id = sqlc.arg(id);

-- name: ListConnectionsToSync :many
-- Connections whose streams the engine may run; the source decides availability in Go.
SELECT c.id, c.external_account, s.code AS source_code, s.kind AS source_kind, s.enabled AS source_enabled,
       s.config AS source_config
FROM connections c
JOIN sources s ON s.id = c.source_id
WHERE c.status IN ('ACTIVE', 'DEGRADED')
ORDER BY c.created_at, c.id;

-- name: ListCursorStreams :many
SELECT stream
FROM sync_cursors
WHERE connection_id = $1;

-- name: InsertMissingSyncCursor :execrows
-- The cursor of a stream declared later (FR-122): existing cursors are not changed.
INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (connection_id, stream) DO NOTHING;

-- name: CountConnectionsByStatus :many
SELECT status, count(*) AS n
FROM connections
GROUP BY status;

-- name: ListSourceStaleness :many
-- Per source: the oldest last_success_at among the streams the engine runs (connections ACTIVE or DEGRADED
-- of an ACTIVE tenant), and how many of those streams ever succeeded. oldest is now when none did.
SELECT s.code, s.kind, s.enabled, s.config,
       count(sc.last_success_at) AS succeeded,
       COALESCE(min(sc.last_success_at), sqlc.arg(now)::timestamptz)::timestamptz AS oldest
FROM sources s
LEFT JOIN connections c ON c.source_id = s.id AND c.status IN ('ACTIVE', 'DEGRADED')
    AND c.tenant_id IN (SELECT id FROM tenants WHERE status = 'ACTIVE')
LEFT JOIN sync_cursors sc ON sc.connection_id = c.id
GROUP BY s.id, s.code, s.kind, s.enabled, s.config
ORDER BY s.code;

-- name: TryEngineLock :one
-- The engine lock: a session-level advisory lock, held on one dedicated connection (SRS — Core §3.2).
SELECT pg_try_advisory_lock(sqlc.arg(key)::bigint)::bool;

-- name: EngineLockHeld :one
-- Whether this session still holds the engine lock.
SELECT EXISTS (
    SELECT 1 FROM pg_catalog.pg_locks
    WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid() AND objsubid = 1
      AND classid = (sqlc.arg(key)::bigint >> 32)::oid
      AND objid = (sqlc.arg(key)::bigint & 4294967295)::oid
)::bool;
