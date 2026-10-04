# Crypto Account Service (CAS)

- A backend service that gives consuming systems one view of a user's crypto accounts: exchange accounts and self-custody wallets.
- It also lets a card program charge a self-custody wallet without taking custody of the funds.
- Reference implementation. Card spend runs on test networks only. Exchange access is read-only.

## Status

| Part | State |
|---|---|
| Requirements and design | Written for milestones C1, S1–S3, X1–X2, W1. X3, X4, E1: outlined in the BRD |
| Implementation | S1 — Contracts done: `contracts/` with Foundry tests, Anvil scripts, frozen ABI, CI. C1 — Core in progress, branch `feature/v0.2.0` |
| Milestone | Current: C1 — Core, `v0.2.0`. Closed: S1 — Contracts, `v0.1.0`. Order, versions and log: [roadmap](docs/roadmap.md) |

## Design highlights

| Topic | Decision | Where |
|---|---|---|
| Non-custodial card spend | Token allowance on the cardholder's wallet. Tokens move wallet → treasury at authorization; the contract holds nothing. | [ADR-9](docs/adr/0009-card-spend-controller.md), [PRD](docs/prd/card-spend.md) |
| Synchronous card, asynchronous chain | Approve only after the debit is preconfirmed or included. Deadline → decline. Every debit expires on-chain; a late debit is refunded automatically. | [ADR-12](docs/adr/0012-authorization-decision-timing.md) |
| No double debit | Three independent layers: request idempotency, nonce stored before send, single-use `authId` in the contract. | [SRS — Card Spend](docs/srs/card-spend.md) |
| Bounded platform power | Exchange keys are read-only. The only key that can move tokens lives in a separate binary. It can move them only wallet → treasury within allowance and daily limit, and back as refunds. | [ADR-3](docs/adr/0003-read-only-exchanges-isolated-write-path.md) |
| One ledger for exchanges and chains | Canonical entries with legs, idempotent import, raw record kept, incremental pull by sequence. | [ADR-5](docs/adr/0005-canonical-ledger.md), [SRS — Core](docs/srs/core.md) |
| Exchange integration | Code adapter per exchange, rate budgets read from response headers, trade discovery across pairs, Earn counted once. | [SRS — Binance](docs/srs/exchange-binance.md) |
| On-chain indexing | Final blocks only; a reorg guard instead of rewrites; the ledger total must equal the on-chain balance. | [ADR-11](docs/adr/0011-event-indexer.md), [SRS — EVM Connector](docs/srs/evm-connector.md) |
| Real time | WebSocket events are signals and triggers. Stored data always comes from polling. | [ADR-13](docs/adr/0013-websocket-signals.md) |

## Reading guide

| Time | Read |
|---|---|
| 5 minutes | [BRD](docs/brd.md) §2–§4 and [the context diagram](docs/c4/context.md) |
| 20 minutes | [PRD — Card Spend](docs/prd/card-spend.md), ADR-9, ADR-12 |
| 1 hour | [SRS — Card Spend](docs/srs/card-spend.md), then [SRS — Core](docs/srs/core.md) |

Full map, conventions and identifiers: [docs/README.md](docs/README.md). Terms: [glossary](docs/glossary.md).

## Milestones

| ID | Content |
|---|---|
| C1 | Core: tenants, connections, encrypted secrets, gRPC API, CI |
| S1 | Contracts `CardSpendController` and `MockUSDC`, tests on a local chain |
| S2 | `card-auth`: end-to-end authorization locally, then on Base Sepolia |
| S3 | Event indexer into the ledger; reconciliation |
| X1 | Binance balances: spot, funding, Earn; key permission check; rate limiter |
| X2 | Binance history; backfill and incremental sync; completeness check |
| W1 | Real-time triggers from Binance account events |
| X3 | Second exchange |
| X4 | Kraken |
| E1 | Expense Tracker shows crypto balances valued with CRS rates |

Tracks S and X are independent. Details and dependencies: BRD §7.3.

## Repository layout

`docs/` and `contracts/` exist today. The rest is the planned structure.

```
docs/         requirements and design
contracts/    CardSpendController, MockUSDC, Foundry tests      S1 (done)
proto/        cas/v1 gRPC contract                              C1
cmd/          server, card-auth, CLI                            C1, S2
internal/     connectors, sync engine, ledger, card spend       C1 and later
migrations/   database schema                                   C1
testdata/     recorded fixtures of source responses             X1
```

## Related services

- **[Currency Rate Service (CRS)](https://portfolio.digitlock.systems/currency-rate-service.html):** rates. CAS never serves prices.
- **[Expense Tracker (ET)](https://portfolio.digitlock.systems/expense-tracker.html):** first consumer of balances and history.

## How the documentation was produced

Author-led, AI-assisted. No document is accepted without the author's review.

| Work | Done by |
|---|---|
| Concept, scope, priorities, milestones, security invariants | Author. The project started from the author's handoff: first list of decisions, card spend track, invariants. |
| Document structure and rules | Author: own BRD / PRD / SRS templates, doc-as-code, style rules. |
| Architecture decisions | Author. The AI assistant prepared options and trade-offs; the author chose. Changed or added by the author in review: protocol of the processor API (ADR-7), WebSocket signals (ADR-13), production path for secrets (ADR-4), C4 notation. |
| Text and diagrams | AI assistant, following the author's templates and decisions. The author reviewed each document before the next one started. |
| Verification | Facts about external APIs checked against official documentation; the check date is stated in the document that uses them. Every diagram rendered before commit. Requirement IDs cross-checked by script. |

## License

MIT — see [LICENSE](LICENSE).

## Author

**Igor Kudinov** — Business Systems Analyst.

This project is part of my professional portfolio. It demonstrates:

- the full requirements chain: BRD → PRD → SRS → ADR → C4, traceable from business goal to requirement;
- Web3 payment design: smart contract, wallet allowance, on-chain movement of funds;
- integration analysis: synchronous and asynchronous flows, idempotency, error handling, exchange APIs with rate limits;
- AI-assisted work with mandatory verification.

## Links

- Portfolio: [portfolio.digitlock.systems](https://portfolio.digitlock.systems/)
- GitHub: [github.com/DigitLock](https://github.com/DigitLock)
- LinkedIn: [linkedin.com/in/igor-kudinov](https://linkedin.com/in/igor-kudinov)
