# C4 Level 1 — System Context

```mermaid
C4Context
    title System Context — Crypto Account Service

    System_Ext(proc, "Issuer processor", "Sends authorizations, reversals and refunds. Simulated.")
    System_Ext(partner, "Partner backend", "Tenant. Registers cards and wallets, reads statuses.")
    System(et, "Expense Tracker", "Consumer of balances and ledger.")

    System(crs, "Currency Rate Service", "Rates for quotes.")
    System(cas, "Crypto Account Service", "One view of crypto accounts. Card spend from a self-custody wallet.")
    System_Ext(kyc, "KYC provider", "KYC status. Design only.")

    Person(holder, "Cardholder", "Owns the wallet. Grants and revokes the token allowance.")
    System_Ext(chain, "EVM network", "Executes transactions. Holds the cardholder wallet and the settlement treasury.")
    System_Ext(binance, "Binance", "Exchange balances and history.")

    Rel(proc, cas, "Authorization, reversal, refund", "HTTP/JSON")
    Rel(partner, cas, "Cards, limits, statuses", "gRPC")
    Rel(et, cas, "Balances, ledger", "gRPC")
    Rel(cas, crs, "Rates", "gRPC")
    Rel(cas, kyc, "KYC status", "design only")
    Rel(cas, chain, "Debit, refund, events", "JSON-RPC, WebSocket")
    Rel(cas, binance, "Read-only account data", "HTTPS, WebSocket")
    Rel(holder, chain, "Sets, revokes allowance", "ERC-20 approve")

    UpdateRelStyle(proc, cas, $offsetX="-220", $offsetY="-5")
    UpdateRelStyle(partner, cas, $offsetX="10", $offsetY="-25")
    UpdateRelStyle(et, cas, $offsetX="30", $offsetY="10")
    UpdateRelStyle(cas, crs, $offsetX="-20", $offsetY="-30")
    UpdateRelStyle(cas, kyc, $offsetX="-40", $offsetY="-40")
    UpdateRelStyle(cas, chain, $offsetX="-150", $offsetY="-5")
    UpdateRelStyle(cas, binance, $offsetX="20", $offsetY="-10")
    UpdateRelStyle(holder, chain, $offsetX="-75", $offsetY="-55")
    UpdateLayoutConfig($c4ShapeInRow="3", $c4BoundaryInRow="1")
```

- Notation: C4 model, Mermaid `C4Context`. Blue: CAS and other systems of the same owner. Grey: external systems.
- Not drawn: status events from CAS to the partner backend (design only).

## Actors and systems

| Element | Type | Relationship with CAS |
|---|---|---|
| Cardholder | Person | Owns the wallet. Never calls CAS directly: acts on-chain (allowance) and through the partner's app. |
| Partner backend | External system | Tenant. Registers cards and wallets, sets limits, reads statuses. Receives status events (design only). |
| Issuer processor | External system, simulated | Sends authorizations, reversals and refunds. Sets the decision time budget. |
| Expense Tracker | System of the same owner, consumer | Reads balances and the ledger. |
| Currency Rate Service | System of the same owner, supplier | Rates for fiat-to-token quotes. |
| KYC provider | External system, design only | KYC status before a wallet is bound to a card. |
| EVM network | External system | Hosts the cardholder wallet, the spend contract and the settlement treasury. CAS submits debits and refunds and reads events. |
| Binance | External system | Source of exchange balances and history through a read-only API key. Account events over WebSocket (ADR-13). |

## Boundaries

- CAS never holds user funds and never signs for the cardholder.
- CAS never serves rates (ADR-1) and never trades or withdraws on exchanges (ADR-3).
- CAS stores no personal data: the owner is an opaque reference (ADR-7).
