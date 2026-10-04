# ADR-10 — Go on-chain stack

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-8, FR-5, FR-16, ADR-3, ADR-12, SRS — Card Spend §2.4

## Context

- `card-auth` reads state, sends transactions and tracks receipts inside a 2.5 s deadline.
- One operator key = one nonce sequence. Several service instances may run.
- Amounts must stay exact end to end (BRD §9.2).

## Options

**Client library**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | go-ethereum: `ethclient` and bindings generated from the ABI | Reference implementation; typed calls; same language as the service | Large dependency |
| 2 | Hand-written JSON-RPC client | Minimal dependency | ABI encoding, signing and fee logic written again |
| 3 | Node.js sidecar (viem, ethers) | Richest tooling | Second runtime; extra hop in the decision path |

**Nonce source**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Ask the node for the pending count before every send | No state | Races between instances; wrong after a failed or dropped send |
| 2 | Reserve the nonce in the database under a row lock; store the intent before the send | Deterministic; restart-safe; works with several instances | Must be checked against the chain at start |

## Decision

- **Client:** go-ethereum with generated bindings for `CardSpendController` and the token.
- **Nonce:** reserved in the database; the `operator_txs` row is written before the send (FR-5). At start `next_nonce` is checked against the chain.
- **Fees:** EIP-1559. A replacement raises the fee on the same nonce.
- **Amounts:** `*big.Int` base units in code, `NUMERIC(78,0)` in the database, strings in the API. Fiat math in decimals, never floats.
- **Signing:** behind a `Signer` interface. Environment key now; a KMS or HSM signer can replace it without touching callers.
- **Reorg detection:** the stored block hash is compared on every tracker cycle.

## Trade-offs

- go-ethereum is a large module: longer builds, bigger binary.
- One operator sends sequentially. More throughput needs more operator keys — out of MVP.
- A key in the environment is acceptable for test networks only.
