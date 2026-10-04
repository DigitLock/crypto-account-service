# ADR-3 — Read-only exchange connections, isolated write path

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-2, BR-6, G-3, ADR-4, ADR-9, [C4 containers](../c4/container.md)

## Context

- Exchange connections need read access only: balances and history.
- Card spend must sign on-chain transactions with an operator key.
- One process that holds both exchange secrets and a signing key widens the damage of a single compromise.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Keep the whole service read-only; card spend in a separate repository | Strongest isolation | Two repositories; the indexer and ledger are split from the flow that feeds them |
| 2 | One binary does everything | Simplest deployment | Operator key and exchange secrets share a process; "read-only" can no longer be verified |
| 3 | One repository, two binaries: `server` (read-only) and `card-auth` (write path) | Process-level isolation; shared model and migrations | Two deployables; the shared database needs role separation |

## Decision

Option 3.

- `server`: exchange adapters contain no trade, withdrawal or transfer calls. A key with trade, withdrawal or transfer permission is rejected when the connection is created.
- `card-auth`: the only service that signs transactions. The operator key exists only in its environment.
- Admin transactions (limits, pause) are signed outside the services: by the CLI with the separate `ADMIN` key (ADR-9).
- Database roles: `card_auth` cannot read exchange secrets; `server` has no signing key.
- CI rule: packages of `server` must not import the signer package.

## Trade-offs

- Two deployables and two configurations.
- The shared database couples the binaries: a schema change affects both.
- The read-only guarantee depends on the exchange exposing key permissions. Where it does not, the check is a documented manual step.
