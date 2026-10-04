# ADR-5 — Canonical ledger with idempotent import

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-4, FR-106, FR-107, FR-111, FR-114, ADR-2, ADR-6, ADR-11, SRS — Core §2.1.4, §2.4

## Context

- Exchange history comes as several lists with different shapes; on-chain history comes as event logs.
- Sync repeats: windows overlap, runs are retried, the service restarts.
- Consumers need one model and exact amounts. ET works with positive amounts and a direction.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Canonical entries with legs; the raw source record stored beside each entry | One model for all sources; remapping possible from raw | Mapping work per source |
| 2 | Native records per source | No mapping | Every consumer learns every source |
| 3 | Double-entry accounting ledger | Built-in balance invariants | Needs counter-accounts for the outside world; heavy for a read-only mirror |

## Decision

Option 1.

- **Entry:** one movement of one asset. An operation with several assets has several legs with one `group_id`.
- **Amounts:** positive decimals, `NUMERIC(38,18)`; direction `IN` or `OUT`. No floats anywhere on the path.
- **Idempotency key:** `(connection_id, stream, external_id, leg)`. Insert skips an existing key.
- **Atomicity:** entries and the stream cursor are committed in one transaction.
- **Finality:** only final records are imported. Entries are never updated.
- **Raw record:** stored as JSONB with every entry, for audit and remapping.
- **Order for consumers:** `seq` assigned in commit order by a single writer.
- **Level:** the account as a whole. Movements between wallets of one account are not entries.

## Trade-offs

- Each source needs a mapping, and a mapping bug needs a controlled re-import from raw; consumers then pull again.
- Raw records roughly double the storage. JSONB keeps them in the same database: a document store would add a second system for data written once and rarely read.
- No accounting invariants. Completeness is checked instead: ledger sum against balance (SRS — Binance UC-204).
- A single writer caps the write rate. Accepted: the volume is bounded by source rate limits.
