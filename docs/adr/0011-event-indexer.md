# ADR-11 — Event indexer as a connection type

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-4, BR-5, BR-12, ADR-5, ADR-6, SRS — EVM Connector, SRS — Card Spend UC-4

## Context

- One ledger must hold exchange and on-chain operations (BR-4).
- On-chain movements of a wallet are visible as event logs: token `Transfer`, contract `Debited` and `Refunded`.
- Reconciliation (UC-4) needs these events in the database.
- Sync is polling with cursors (ADR-6). The ledger is append-only with an idempotency key (ADR-5).

## Options

**Where the indexer lives**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Separate indexer service | Scales on its own | Second sync engine, cursor store and health model |
| 2 | Third-party indexer or webhooks | Nothing to build | External dependency and data contract |
| 3 | Connection kind `EVM_WALLET` in the existing sync engine | Reuses cursors, health, ledger and idempotency; shows the model is source-agnostic | The engine must support a block-based cursor |

**Reorg handling**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Index up to the chain head, re-read the last K blocks, rewrite on mismatch | Freshest data | Ledger entries can change or vanish: breaks append-only |
| 2 | Index final blocks only | Entries never change | The ledger lags by the finality window |

## Decision

- Indexer = connection kind `EVM_WALLET`: address and chain ID, no secret.
- Streams: token `Transfer` logs where the wallet is sender or recipient; `Debited` and `Refunded` logs of the controller.
- Balances: token balances of the wallet read at the head → snapshot with account type `WALLET`. A snapshot is replaced by the next one, so a reorg cannot leave a wrong value behind.
- Cursor: block number in `sync_cursors`.
- Logs: final blocks only. Same finality rule as the card flow (SRS — EVM Connector §2.1.1).
- `external_id = tx_hash:log_index`.
- Mapping to ledger types:

| Log | Ledger type |
|---|---|
| `Debited` | `CARD_DEBIT` |
| `Refunded` | `CARD_REFUND` |
| Other incoming `Transfer` | `DEPOSIT` |
| Other outgoing `Transfer` | `WITHDRAWAL` |

- A `Transfer` in the same transaction as a `Debited` or `Refunded` of that wallet is not recorded a second time.
- The settlement treasury is watched the same way, as a connection of the platform's own tenant. It sees every debit and refund and every other transfer of the treasury; reconciliation reads its entries.

## Trade-offs

- On-chain operations appear in the ledger after the finality window: about 20 minutes on Base. The real-time path does not depend on it: `card-auth` tracks its own transactions.
- The RPC provider limits the block range of a log query: backfill runs in chunks.
- Native-coin transfers emit no logs and are not covered. Acceptable: the funding token is ERC-20.
