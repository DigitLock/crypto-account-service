-- Reconciliation of one EVM source (SRS - Card Spend UC-4): reads only, plus the insert of a run; role cas_server
-- (SRS - Core §3.2). Base units as integer strings, decimals without trailing zeros, IDs and hashes as 0x hex.
-- tx_hash (SRS - Card Spend §2.1.4): of the DEBIT row, or of the REFUND rows of a return, that is INCLUDED or
-- CONFIRMED; else of the newest such row with a hash; else empty.

-- name: GetReconcileTreasury :one
-- The treasury connection named in sources.config, its tenant (the platform tenant, S3 D-1) and its logs cursor;
-- the cursor is null when the stream has no row.
SELECT c.id, c.tenant_id, t.name AS tenant_name, c.source_id, sc.cursor
FROM connections c
JOIN tenants t ON t.id = c.tenant_id
LEFT JOIN sync_cursors sc ON sc.connection_id = c.id AND sc.stream = 'logs'
WHERE c.id = sqlc.arg(id);

-- name: ListReconcileEvents :many
-- The Debited and Refunded events of the treasury connection: CARD_DEBIT IN and CARD_REFUND OUT of its logs stream,
-- with the fields of the stored log (SRS - EVM Connector §2.4). In the order of the ledger.
SELECT le.type, le.occurred_at,
       COALESCE(le.raw -> 'args' ->> 'authId', '')::text AS auth_id,
       COALESCE(le.raw -> 'args' ->> 'refundId', '')::text AS refund_id,
       COALESCE(le.raw -> 'args' ->> 'amount', '')::text AS amount,
       COALESCE(le.raw ->> 'transactionHash', '')::text AS tx_hash
FROM ledger_entries le
WHERE le.connection_id = sqlc.arg(connection_id) AND le.stream = 'logs'
  AND ((le.type = 'CARD_DEBIT' AND le.direction = 'IN') OR (le.type = 'CARD_REFUND' AND le.direction = 'OUT'))
ORDER BY le.seq;

-- name: ListReconcileTenants :many
-- The partner tenants of a chain: every tenant but the platform tenant with an authorization on the chain.
SELECT t.id, t.name
FROM tenants t
WHERE t.id <> sqlc.arg(platform_tenant_id)
  AND EXISTS (SELECT 1 FROM authorizations a WHERE a.tenant_id = t.id AND a.chain_id = sqlc.arg(chain_id)::bigint)
ORDER BY t.name;

-- name: ListReconcileAuthorizations :many
-- The authorizations of a tenant on a chain eligible for rules 1 and 2a: valid_until at or before period_to.
-- Oldest first.
SELECT a.auth_id, a.chain_auth_id, a.status, COALESCE(a.token_amount::text, '')::text AS token_amount, a.received_at,
       COALESCE('0x' || encode(d.tx_hash, 'hex'), '')::text AS tx_hash
FROM authorizations a
LEFT JOIN LATERAL (
    SELECT t.tx_hash FROM operator_txs t
    WHERE t.authorization_id = a.id AND t.purpose = 'DEBIT' AND t.tx_hash IS NOT NULL
    ORDER BY t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
    LIMIT 1
) d ON true
WHERE a.tenant_id = sqlc.arg(tenant_id) AND a.chain_id = sqlc.arg(chain_id)::bigint
  AND a.valid_until <= sqlc.arg(period_to)::timestamptz
ORDER BY a.received_at, a.auth_id;

-- name: ListReconcileReturns :many
-- The returns of a tenant on a chain eligible for rule 3: CONFIRMED, and the block of its REFUND row at or below
-- to_block. The REFUND row is the one of tx_hash; a return whose row has no block is not eligible.
WITH refund AS (
    SELECT DISTINCT ON (t.return_row_id) t.return_row_id, t.tx_hash, t.block_number
    FROM operator_txs t
    WHERE t.purpose = 'REFUND' AND t.return_row_id IS NOT NULL AND t.tx_hash IS NOT NULL
    ORDER BY t.return_row_id, t.status IN ('INCLUDED', 'CONFIRMED') DESC, t.created_at DESC
)
SELECT r.return_id, r.chain_refund_id, r.token_amount::text AS token_amount,
       ('0x' || encode(x.tx_hash, 'hex'))::text AS tx_hash
FROM returns r
JOIN authorizations a ON a.id = r.authorization_id
JOIN refund x ON x.return_row_id = r.id
WHERE r.tenant_id = sqlc.arg(tenant_id) AND a.chain_id = sqlc.arg(chain_id)::bigint AND r.status = 'CONFIRMED'
  AND x.block_number <= sqlc.arg(to_block)::bigint
ORDER BY r.created_at, r.return_id;

-- name: ListKnownChainAuthIDs :many
-- Which of the authIds belong to an authorization of any tenant (rule 2).
SELECT chain_auth_id
FROM authorizations
WHERE chain_auth_id = ANY(sqlc.arg(ids)::bytea[]);

-- name: ListKnownChainRefundIDs :many
-- Which of the refundIds belong to a return of any tenant (rule 4).
SELECT chain_refund_id
FROM returns
WHERE chain_refund_id = ANY(sqlc.arg(ids)::bytea[]);

-- name: ListReconcileCheckpoints :many
-- The balance checkpoints of the treasury connection with a gap other than 0 (rule 5), in asset units.
SELECT asset, native_asset, trim_scale(balance)::text AS balance, trim_scale(ledger_total)::text AS ledger_total
FROM balance_checkpoints
WHERE connection_id = sqlc.arg(connection_id) AND gap <> 0
ORDER BY asset, native_asset;

-- name: InsertReconciliationRun :one
-- Append-only: one row per run of a tenant on a source.
INSERT INTO reconciliation_runs (tenant_id, source_id, period_from, period_to, to_block, totals, mismatches)
VALUES (
    sqlc.arg(tenant_id), sqlc.arg(source_id), sqlc.arg(period_from), sqlc.arg(period_to), sqlc.arg(to_block),
    sqlc.arg(totals), sqlc.arg(mismatches)
)
RETURNING id, created_at;
