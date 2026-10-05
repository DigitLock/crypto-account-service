-- name: InsertConnection :exec
-- The ID and the times come from the caller. No RETURNING: the caller has every value.
INSERT INTO connections (
    id, tenant_id, owner_ref, source_id, external_account, label, status,
    credentials_enc, kek_version, key_fingerprint, permissions, permissions_checked_at, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13
);
