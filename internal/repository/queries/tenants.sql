-- name: CreateTenant :one
INSERT INTO tenants (name)
VALUES ($1)
RETURNING id, name, status, created_at;

-- name: GetTenantByName :one
SELECT id, name, status, created_at
FROM tenants
WHERE name = $1;

-- name: ListTenants :many
SELECT id, name, status, created_at
FROM tenants
ORDER BY name;

-- name: SetTenantStatus :execrows
-- Changes the status only when it differs: 0 rows means no change.
UPDATE tenants
SET status = sqlc.arg(status)
WHERE id = sqlc.arg(id) AND status <> sqlc.arg(status);
