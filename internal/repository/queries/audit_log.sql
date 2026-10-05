-- name: InsertAuditLog :exec
-- No RETURNING: cas_server has no SELECT on audit_log.
INSERT INTO audit_log (tenant_id, credential_id, action, object_id, details)
VALUES ($1, $2, $3, $4, $5);
