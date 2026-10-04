# ADR-6 — Sync by polling with cursors

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-3, BR-4, FR-108, FR-109, FR-110, ADR-2, ADR-5, ADR-13, SRS — Core UC-102

## Context

- No consumer needs real-time data: ET pulls on a schedule.
- Sources are rate-limited and serve history in pages and time windows.
- The service restarts; a long backfill must not start over.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Polling with a stored cursor per stream | Simple; restart-safe; same pattern as CRS and ET | Data is minutes old; requests are spent when nothing changed |
| 2 | WebSocket user streams as the data source | Real time | Connection state and reconnects; gaps must be refilled by polling anyway |
| 3 | Fetch from the source at read time | Always fresh | Slow reads; every read spends the rate budget; no history |

## Decision

Option 1.

- **Stream:** one kind of data of one connection, with its own cursor, schedule and health.
- **Modes:** `BACKFILL` walks through history window by window; `INCREMENTAL` follows new records.
- **Schedule:** per stream family; manual trigger with a cooldown.
- **Rate limiter:** in front of every request, one limiter per source; its budgets are defined by the connector.
- **Failure:** backoff, failure counter, `DEGRADED` after the threshold. Reads keep working on the last data with a stale flag.
- **One engine instance** at a time, guarded by a database lock: one process owns the rate budgets.

## Trade-offs

- Balances are up to one interval old.
- Incremental runs re-read a lookback window to catch records finalized late: some requests return only known data.
- Sync does not scale horizontally. Accepted: throughput is bounded by source limits, not by CAS.
- Real-time events are added as triggers of the same polling runs, not as a second data path (ADR-13).
