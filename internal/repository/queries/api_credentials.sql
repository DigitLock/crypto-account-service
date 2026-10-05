-- name: CreateCredential :one
INSERT INTO api_credentials (tenant_id, kind, key_id, secret_hash)
VALUES ($1, $2, $3, $4)
RETURNING id, key_id, created_at;

-- name: ListServiceTokens :many
-- No secret hash. All tenants when tenant_id is null.
SELECT c.id, c.key_id, t.name AS tenant_name, c.created_at, c.revoked_at
FROM api_credentials c
JOIN tenants t ON t.id = c.tenant_id
WHERE c.kind = 'SERVICE_TOKEN'
  AND (sqlc.narg(tenant_id)::uuid IS NULL OR c.tenant_id = sqlc.narg(tenant_id)::uuid)
ORDER BY t.name, c.created_at, c.key_id;

-- name: RevokeServiceToken :one
-- Revokes a service token that is not revoked yet: no row means unknown or already revoked.
UPDATE api_credentials
SET revoked_at = now()
WHERE key_id = $1 AND kind = 'SERVICE_TOKEN' AND revoked_at IS NULL
RETURNING id, tenant_id;

-- name: GetServiceTokenByKeyID :one
-- Lookup for authentication: one query per request.
SELECT c.id, c.tenant_id, c.secret_hash, c.revoked_at, t.status AS tenant_status
FROM api_credentials c
JOIN tenants t ON t.id = c.tenant_id
WHERE c.key_id = $1 AND c.kind = 'SERVICE_TOKEN';
