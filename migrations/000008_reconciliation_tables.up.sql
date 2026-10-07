-- Tables of S3 (SRS - Core §2.4.1 reconciliation_runs, balance_checkpoints; S3 D-5, S3 D-6) and the rights of
-- cas_server on them (SRS - Core §3.2; S3 D-7). Nothing for cas_card_auth.

-- Append-only: one row per reconciliation run of one tenant on one source.
CREATE TABLE reconciliation_runs (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES tenants (id),
    source_id   SMALLINT    NOT NULL REFERENCES sources (id),
    period_from TIMESTAMPTZ NOT NULL,
    period_to   TIMESTAMPTZ NOT NULL,
    to_block    BIGINT      NOT NULL,
    totals      JSONB       NOT NULL,
    mismatches  JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The newest run of a tenant and source: GetReconciliationReport without run_id, and the reconciliation worker.
CREATE INDEX reconciliation_runs_tenant_source_created_idx
    ON reconciliation_runs (tenant_id, source_id, created_at DESC);

-- The last balance checkpoint per connection and native asset; a new checkpoint replaces the row.
CREATE TABLE balance_checkpoints (
    connection_id UUID            NOT NULL REFERENCES connections (id) ON DELETE CASCADE,
    native_asset  TEXT            NOT NULL,
    asset         TEXT            NOT NULL,
    block_number  BIGINT,
    block_hash    TEXT,
    taken_at      TIMESTAMPTZ     NOT NULL,
    balance       NUMERIC(38, 18) NOT NULL,
    -- ledger_total (sum of IN minus sum of OUT) and gap are differences: they may be negative.
    ledger_total  NUMERIC(38, 18) NOT NULL,
    gap           NUMERIC(38, 18) NOT NULL,
    checked_at    TIMESTAMPTZ     NOT NULL DEFAULT now(),
    PRIMARY KEY (connection_id, native_asset),
    CONSTRAINT balance_checkpoints_balance_check CHECK (balance >= 0)
);

GRANT SELECT, INSERT ON reconciliation_runs TO cas_server;
-- Rows are removed only by the cascade of a deleted connection.
GRANT SELECT, INSERT, UPDATE ON balance_checkpoints TO cas_server;
-- The eligibility of a return in reconciliation (S3 D-7): added to the column grant of 000006.
GRANT SELECT (block_number) ON operator_txs TO cas_server;
