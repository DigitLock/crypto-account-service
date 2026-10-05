# ADR-13 — WebSocket for real-time signals, polling as the source of record

- **Status:** Accepted
- **Date:** 2026-10-03. Updated 2026-10-05: the `card-auth` subscription named after the provider test of S2
- **Related:** BR-3, BR-7, ADR-6, ADR-12, milestones S2 and W1, SRS — Binance UC-205, SRS — Card Spend UC-1

## Context

- Polling leaves balances minutes old. A trade on a pair that has no stream yet is found only by the periodic discovery pass.
- `card-auth` must learn that a debit executed inside a 2.5 s deadline. Base serves preconfirmed data in two ways: reads with the `pending` block tag and WebSocket subscriptions. Its public endpoints are HTTP only and rate-limited.
- A WebSocket stream alone is not enough: it has no history, it drops events while disconnected, and the Binance account stream covers the spot wallet only.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Polling only | Simplest | Minutes-old data; late discovery of new pairs; `card-auth` polls receipts inside a tight deadline |
| 2 | Import data from WebSocket events | Real time | Second mapping path for the same record; no history; gaps after every disconnect; partial coverage |
| 3 | Events as signals: an event starts the existing sync or reports an awaited fact; stored data still comes from polling | Fresh data with one data path; a lost connection degrades to polling | One more component per source: connection lifecycle to manage |

## Decision

Option 3.

**Common rules**

- Stored data comes only from polling reads (ADR-6). No ledger entry or snapshot is written from an event payload.
- A listener reconnects with backoff and jitter, answers pings, and renews the connection before the source's lifetime limit.
- After every connect or reconnect the polling path covers what may have been missed.
- A listener that is down changes nothing else: polling continues. The state is visible in metrics.

**Binance (milestone W1)**

- Source: the user data stream of the WebSocket API, one subscription per connection.
- Listeners are owned by the instance that runs the sync engine.
- After every connect or reconnect a catch-up run of the affected streams is made.
- Events are merged: several events for one stream inside a short window start one run. Triggered runs pass through the same rate limiter.
- Balance events → run the `balances` stream now.
- Trade execution events → run `trades:{pair}` now; create the stream if the pair has none.

**`card-auth` (milestone S2)**

- Source: Base Flashblocks through Alchemy, the primary RPC provider of Base Sepolia (SRS — Card Spend §4, issue 1). One subscription `eth_subscribe ["pendingLogs", {address, topics}]`, filtered by the controller address and the `Debited` topic; on a chain without Flashblocks, such as Anvil, the standard `logs` subscription with the same filter.
- No subscription to new blocks or to unfiltered Flashblocks: the unfiltered stream is about 140 logs per second, and a block subscription would cost an estimated 1–1.7M compute units per day on the free plan of the provider.
- A `Debited` log with the awaited `authId` is the inclusion signal for the decision (ADR-12). A preconfirmed log carries a zero `blockHash`; nothing of it is stored, the receipt read fills the row.
- Receipt polling runs beside it: it is the fallback and the only way to see a reverted debit. On Flashblocks the receipt itself is preconfirmed: both paths see the debit before the block is sealed.
- Final statuses still come from the tracker's reads.

## Trade-offs

- One long-lived connection per exchange connection: memory, connection limits of the source, staggered reconnects after an outage.
- Triggered runs spend rate budget. Merging and the limiter bound it.
- The Binance stream covers the spot wallet only: funding and Earn stay on the schedule.
- `card-auth` depends on a third-party RPC provider for WebSocket access; the fallback endpoint is HTTP only, so the listener is down while the primary is.
- More to test: a fake WebSocket server and disconnect scenarios.
- The Binance subscription may require an Ed25519 key instead of HMAC: to be verified before W1.
