-- The newest run of a source and the newest run of each tenant on a source (the reconciliation worker, the gauge
-- reconciliation_mismatches, GetReconciliationReport): one index led by the source. It replaces the index of 000008,
-- led by the tenant, which no query needs any more.
CREATE INDEX reconciliation_runs_source_tenant_created_idx
    ON reconciliation_runs (source_id, tenant_id, created_at DESC, id DESC);

DROP INDEX reconciliation_runs_tenant_source_created_idx;
