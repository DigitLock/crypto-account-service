# ADR-2 — Connectors: code adapters behind one interface

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-5, FR-112, FR-216, ADR-5, ADR-6, SRS — Core §2.1.1, SRS — Binance

## Context

- Sources differ in request signing, rate limits, pagination and the shape of history.
- A new source must not change the engine, the ledger schema or the API (BR-5).
- CRS adds providers by configuration only. That works for public endpoints with plain JSON.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Code adapter per source behind one interface; differences declared as capability flags | Signing, limits and paging are explicit and testable | Code for every source |
| 2 | Config-driven generic adapter, as in CRS | New source without code | Cannot express signing schemes, windowed history, per-pair trades |
| 3 | Aggregator library (CCXT) | Many exchanges at once | Hides limits and history gaps; maturity of the Go port; less control over what a key is used for |

## Decision

Option 1.

```go
type Connector interface {
    Code() string
    Capabilities() Capabilities
    Verify(ctx context.Context, cred Credentials) (AccountInfo, error) // identity and permissions
    Streams(ctx context.Context, conn Connection) ([]Stream, error)
    FetchBalances(ctx context.Context, conn Connection) (Snapshot, error)
    FetchPage(ctx context.Context, conn Connection, s Stream, cur Cursor) (Page, error) // entries and next cursor
}
```

- Connectors are registered by source code. The engine knows only the interface.
- Differences are capability flags, never `if source == …` in the engine: key permissions readable, unified ledger, trades per pair, block-based cursor, test environment, event stream.
- What is truly configurable stays in the database: base URL, intervals, rate budgets, asset aliases.
- A shared connector test suite defines "done" for an adapter: idempotency, cursor resume, limit handling.
- The code block above is the sketch of the decision. The interface as built in C1 is in [SRS — Core](../srs/core.md) §2.1.1, Connector contract.

## Trade-offs

- Every source costs code and fixtures.
- The interface will grow when a source does not fit. Growth happens through new flags and stream kinds, not through branches in the engine.
- No free coverage of dozens of exchanges.
