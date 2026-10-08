-- Reads of the stored reconciliation runs by server (SRS - Core §2.1.1, §3.2): the reconciliation worker, the gauge
-- reconciliation_mismatches and GetReconciliationReport. Role cas_server.

-- name: GetTreasuryLogsCursor :one
-- The logs cursor of a connection; no row when the stream has none.
SELECT cursor
FROM sync_cursors
WHERE connection_id = sqlc.arg(connection_id) AND stream = 'logs';

-- name: GetNewestRunToBlock :one
-- to_block of the newest run of a source, of any tenant; no row when the source has none. The newest run of each
-- tenant is one probe of the index (source_id, tenant_id, created_at DESC, id DESC); the newest of those wins.
SELECT n.to_block
FROM tenants t
CROSS JOIN LATERAL (
    SELECT r.to_block, r.created_at, r.id
    FROM reconciliation_runs r
    WHERE r.source_id = sqlc.arg(source_id) AND r.tenant_id = t.id
    ORDER BY r.created_at DESC, r.id DESC
    LIMIT 1
) n
ORDER BY n.created_at DESC, n.id DESC
LIMIT 1;

-- name: ListNewestRunMismatches :many
-- The newest run of each tenant on each source, with its source code and its mismatches: one probe of the index
-- (source_id, tenant_id, created_at DESC, id DESC) per source and tenant.
SELECT s.code AS source_code, n.mismatches
FROM sources s
CROSS JOIN tenants t
CROSS JOIN LATERAL (
    SELECT r.mismatches
    FROM reconciliation_runs r
    WHERE r.source_id = s.id AND r.tenant_id = t.id
    ORDER BY r.created_at DESC, r.id DESC
    LIMIT 1
) n
ORDER BY s.code;

-- name: GetReconciliationRun :one
-- A run of a tenant on a source: the one of run_id, or the newest when run_id is null; the index
-- (source_id, tenant_id, created_at DESC, id DESC).
SELECT r.id, s.code AS source_code, r.period_from, r.period_to, r.to_block, r.totals, r.mismatches, r.created_at
FROM reconciliation_runs r
JOIN sources s ON s.id = r.source_id
WHERE r.tenant_id = sqlc.arg(tenant_id) AND s.code = sqlc.arg(source_code)
  AND (sqlc.narg(run_id)::uuid IS NULL OR r.id = sqlc.narg(run_id)::uuid)
ORDER BY r.created_at DESC, r.id DESC
LIMIT 1;
