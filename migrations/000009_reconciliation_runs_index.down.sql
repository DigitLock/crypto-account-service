CREATE INDEX reconciliation_runs_tenant_source_created_idx
    ON reconciliation_runs (tenant_id, source_id, created_at DESC);

DROP INDEX reconciliation_runs_source_tenant_created_idx;
