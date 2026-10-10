-- name: InspectConnection :one
-- The state of a connection for casctl connection inspect (X1 D-45): never the account identity or the fingerprint.
SELECT status, permissions, (credentials_enc IS NOT NULL)::boolean AS has_ciphertext
FROM connections
WHERE id = $1;

-- name: InspectResiduals :one
-- What is stored for a connection ID, whether the connection exists or was deleted.
SELECT
    (SELECT count(*) FROM balance_snapshots s WHERE s.connection_id = sqlc.arg(connection_id)::uuid)::bigint AS snapshots,
    (SELECT count(*) FROM snapshot_balances b JOIN balance_snapshots s ON s.id = b.snapshot_id
        WHERE s.connection_id = sqlc.arg(connection_id)::uuid)::bigint AS balance_rows,
    (SELECT count(*) FROM sync_cursors c WHERE c.connection_id = sqlc.arg(connection_id)::uuid)::bigint AS cursors,
    (SELECT count(*) FROM ledger_entries e WHERE e.connection_id = sqlc.arg(connection_id)::uuid)::bigint AS ledger_entries;

-- name: InspectAudit :many
-- The audit rows of a connection per action, and whether any of their details holds a member uid at any depth.
SELECT action, count(*)::bigint AS row_count,
    bool_or(coalesce(jsonb_path_exists(details, '$.**.uid'), false))::boolean AS has_uid
FROM audit_log
WHERE object_id = $1
GROUP BY action
ORDER BY action;

-- name: InspectIPRestricted :one
-- ip_restricted of the latest CONNECTION_CREATED row of a connection (X1 D-2); empty when it is not recorded.
SELECT coalesce((details -> 'ip_restricted')::text, '')::text AS ip_restricted
FROM audit_log
WHERE object_id = $1 AND action = 'CONNECTION_CREATED'
ORDER BY created_at DESC, id DESC
LIMIT 1;
