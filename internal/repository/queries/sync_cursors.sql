-- name: InsertSyncCursor :exec
INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ListStreamHealth :many
-- Health of the streams of a connection of a tenant, ordered by stream. The cursor is not read.
SELECT sc.stream, sc.mode, sc.next_run_at, sc.last_success_at, sc.last_error, sc.consecutive_failures
FROM sync_cursors sc
JOIN connections c ON c.id = sc.connection_id
WHERE sc.connection_id = sqlc.arg(connection_id) AND c.tenant_id = sqlc.arg(tenant_id)
ORDER BY sc.stream;
