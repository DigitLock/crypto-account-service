# ADR-8 — Network and funding token

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-6, BR-7, ADR-9, ADR-12, PRD — Card Spend §4.3

## Context

- Card spend needs an EVM network for development, automated tests and a public demo.
- No real funds and no mainnet (BRD §9.2).
- Decision latency depends on block time and preconfirmations (ADR-12).

## Options

**Public network**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Base Sepolia | 2 s blocks, 200 ms preconfirmations; Base is used by a production card (MetaMask Card) | The public endpoints are HTTP only and rate-limited: a subscription to preconfirmed data needs an RPC provider with WebSocket |
| 2 | Ethereum Sepolia | Most common testnet | 12 s blocks: no way to meet a 2–3 s decision budget |
| 3 | Another L2 testnet | Similar properties | No advantage for this project; one network is enough |

**Funding token**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Own `MockUSDC`: ERC-20, 6 decimals, open mint | No faucet dependency; deterministic tests; same decimals as USDC | Not the real token: no freeze, no `permit` unless added |
| 2 | Public testnet USDC | Closer to production behaviour | Supply depends on an external faucet; tests cannot mint |

## Decision

- Local: Anvil. Public: Base Sepolia, chain ID 84532.
- Token: own `MockUSDC`, 6 decimals.
- Keys: generated test keys only. Never a personal wallet.
- Network parameters are configuration. Nothing in the code is Base-specific except the optional preconfirmation source.

## Trade-offs

- Testnet latency and reliability differ from mainnet: measured numbers are indicative.
- The mock hides real-token behaviour such as address freezing. That case stays a documented risk (PRD §3.3.4).
- A second network later means a new deployment and configuration, no code change.
