-- name: InsertSyncCursor :exec
INSERT INTO sync_cursors (connection_id, stream, mode, cursor, next_run_at)
VALUES ($1, $2, $3, $4, $5);
