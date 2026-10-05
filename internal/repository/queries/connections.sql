-- name: InsertConnection :exec
-- The ID and the times come from the caller. No RETURNING: the caller has every value.
INSERT INTO connections (
    id, tenant_id, owner_ref, source_id, external_account, label, status,
    credentials_enc, kek_version, key_fingerprint, permissions, permissions_checked_at, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13
);

-- name: GetConnection :one
-- One connection of a tenant: another tenant's connection is no row, like an unknown ID.
SELECT c.id, s.code AS source_code, s.kind AS source_kind, c.owner_ref, c.label, c.status,
       c.external_account, c.key_fingerprint, c.permissions, c.created_at
FROM connections c
JOIN sources s ON s.id = c.source_id
WHERE c.id = sqlc.arg(id) AND c.tenant_id = sqlc.arg(tenant_id);

-- name: ListConnectionsPage :many
-- One page in the order created_at, id, after the position (after_created_at, after_id). The row comparison
-- on (tenant_id, created_at, id) is a range of the index connections_tenant_created_idx; no OFFSET.
SELECT c.id, s.code AS source_code, s.kind AS source_kind, c.owner_ref, c.label, c.status,
       c.external_account, c.key_fingerprint, c.permissions, c.created_at
FROM connections c
JOIN sources s ON s.id = c.source_id
WHERE c.tenant_id = sqlc.arg(tenant_id)
  AND (c.tenant_id, c.created_at, c.id) > (sqlc.arg(tenant_id), sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
  AND (sqlc.narg(owner_ref)::text IS NULL OR c.owner_ref = sqlc.narg(owner_ref)::text)
ORDER BY c.created_at, c.id
LIMIT sqlc.arg(page_limit);

-- name: DeleteConnection :execrows
-- Deletes a connection of a tenant; the database cascades to cursors, snapshots and ledger entries.
-- 0 rows: no such connection in the tenant, or another call deleted it first.
DELETE FROM connections
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id);
