# Architecture Decision Records

One file per decision: `NNNN-short-title.md`. Structure: Context → Options → Decision → Trade-offs.

| ADR | Decision | Status |
|---|---|---|
| [ADR-1](0001-prices-in-crs-accounts-in-cas.md) | Prices live in CRS, accounts in CAS. CAS never serves rates; consumers value a portfolio as CAS balance × CRS rate. | Accepted |
| [ADR-2](0002-connector-interface.md) | Code adapters behind one connector interface. Exchange differences are capability flags, not branches in the sync engine. No config-driven adapter, no CCXT. | Accepted |
| [ADR-3](0003-read-only-exchanges-isolated-write-path.md) | Exchange connections are read-only. The only write path (on-chain debit and refund) is isolated in the `card-auth` binary. | Accepted |
| [ADR-4](0004-secrets-encrypted-at-rest.md) | Secrets are encrypted at rest (AES-256-GCM, master key from env). The API exposes a key fingerprint only. | Accepted |
| [ADR-5](0005-canonical-ledger.md) | Canonical ledger. Idempotency key `(connection_id, stream, external_id, leg)`. Raw source payload stored next to each entry. | Accepted |
| [ADR-6](0006-polling-with-cursors.md) | Sync by polling with cursors: backfill and incremental. Polling is the source of record. | Accepted |
| [ADR-7](0007-api-protocols-and-tenancy.md) | gRPC for consumer APIs. HTTP/JSON only for the inbound processor API of `card-auth`, because the processor defines that contract. One service token per tenant. Data is scoped by `tenant` + opaque `owner_ref`. | Accepted |
| [ADR-8](0008-network-and-token.md) | Anvil locally, Base Sepolia publicly, own `MockUSDC` (6 decimals). No mainnet. | Accepted |
| [ADR-9](0009-card-spend-controller.md) | `CardSpendController`: allowance model, pull from wallet to treasury, never holds funds. Idempotent `debit(authId)` with on-chain expiry and `refund(refundId)`, daily limit, roles, pause. Smart-account modules recorded as the alternative. | Accepted |
| [ADR-10](0010-go-onchain-stack.md) | Go on-chain stack: go-ethereum with generated bindings. Operator nonce persisted before send. Amounts in token base units. | Accepted |
| [ADR-11](0011-event-indexer.md) | The event indexer is a connection kind (`EVM_WALLET`). `external_id = tx_hash:log_index`. Only final blocks are indexed. | Accepted |
| [ADR-12](0012-authorization-decision-timing.md) | Authorization decision timing: synchronous card decision over an asynchronous on-chain debit. Fail-closed. | Accepted |
| [ADR-13](0013-websocket-signals.md) | WebSocket for real-time signals only: an event starts the existing sync or reports an awaited fact. Stored data still comes from polling. | Accepted |
