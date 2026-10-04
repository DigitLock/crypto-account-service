# C4 Level 2 — Containers

```mermaid
C4Container
    title Container diagram — Crypto Account Service

    System_Ext(proc, "Issuer processor", "Simulated by the CLI.")
    System(crs, "Currency Rate Service", "Rates for quotes.")
    System_Ext(consumers, "Consumer systems", "Partner backends, Expense Tracker.")

    Container_Boundary(cas, "Crypto Account Service") {
        Container(auth, "card-auth", "Go", "Authorization decision. Sends debits and refunds. Holds the operator key.")
        ContainerDb(db, "Database", "PostgreSQL", "Authorizations, cards, connections, ledger. One role per binary.")
        Container(server, "server", "Go", "Connections, balances, ledger, sync engine, indexer, card registry.")

        Container(ctrl, "CardSpendController", "Solidity on EVM: Anvil, Base Sepolia", "Pulls tokens wallet → treasury. Refunds. Limits, roles, pause.")
    }

    Boundary(sources, "Exchanges", "external") {
        System_Ext(binance, "Binance API", "Balances, history, account events.")
    }

    Rel(proc, auth, "Authorize, return, status", "HTTP/JSON")
    Rel(auth, crs, "Rates", "gRPC")
    Rel(consumers, server, "Connections, balances, ledger, cards", "gRPC")
    Rel(auth, db, "Reads, writes", "SQL")
    Rel(server, db, "Reads, writes", "SQL")
    Rel(auth, ctrl, "Debit, refund, state", "JSON-RPC, WebSocket")
    Rel(server, ctrl, "Event logs", "JSON-RPC")
    Rel(server, binance, "Read-only requests, events", "HTTPS, WebSocket")

    UpdateRelStyle(proc, auth, $offsetX="-175", $offsetY="-45")
    UpdateRelStyle(auth, crs, $offsetX="60", $offsetY="-45")
    UpdateRelStyle(consumers, server, $offsetX="10", $offsetY="-55")
    UpdateRelStyle(auth, db, $offsetX="-40", $offsetY="-45")
    UpdateRelStyle(server, db, $offsetX="-40", $offsetY="-45")
    UpdateRelStyle(auth, ctrl, $offsetX="20", $offsetY="-10")
    UpdateRelStyle(server, ctrl, $offsetX="55", $offsetY="35")
    UpdateRelStyle(server, binance, $offsetX="100", $offsetY="-10")
    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

- Notation: C4 model, Mermaid `C4Container`. Blue: CAS containers and systems of the same owner. Grey: external systems.
- Consumer systems: partner backends and Expense Tracker, same gRPC API.
- Not drawn: `MockUSDC` (the contract moves it with `transferFrom`) and the CLI (plays the issuer processor, runs admin operations).

## Containers

| Container | Technology | Responsibility | Secrets it holds |
|---|---|---|---|
| `card-auth` | Go | Authorization decision, quote, debit and refund submission, confirmation tracking | Operator private key (env) |
| `server` | Go | Tenants, connections, card registry, balances, ledger, sync engine, event indexer, reconciliation | Master key for exchange secrets (env) |
| Database | PostgreSQL | One database, one migration set, one role per binary | Encrypted exchange secrets |
| `CardSpendController` | Solidity, deployed on the EVM network: Anvil, Base Sepolia | Pull debit to the treasury, refund, daily limit, roles, pause. Holds no tokens. | None |
| `MockUSDC` | Solidity, ERC-20, 6 decimals | Test funding token | None |
| CLI | Go | Simulates the processor; admin operations | Admin key, test networks only |

## Rules

- `card-auth` is the only service that writes to the chain (ADR-3). Admin transactions (limits, pause) are signed by the CLI with the separate `ADMIN` key.
- The processor-facing API of `card-auth` is HTTP/JSON: the processor defines the contract. Consumer APIs on `server` are gRPC (ADR-7).
- Real-time signals arrive over WebSocket: account events from Binance to `server`, preconfirmed logs from the RPC provider to `card-auth`. Stored data still comes from polling (ADR-13).
- `card-auth` and `server` use separate database roles. The `card-auth` role cannot read exchange secrets; the `server` process never sees the operator key.
- `card-auth` reads the card registry from the database, not through `server`: no extra hop inside the decision time budget.
- Contract roles: `OPERATOR` = `card-auth` key, `ADMIN` = separate key for limits and pause (ADR-9).
- Tokens can move only wallet → treasury (debit) and treasury → wallet (refund). The treasury and token addresses are fixed at deployment.
