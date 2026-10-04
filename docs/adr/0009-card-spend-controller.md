# ADR-9 — CardSpendController: allowance model

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-6, BR-8, BR-9, BR-10, G-3, ADR-8, ADR-12, SRS — Card Spend §2.1.5

## Context

- The platform must never hold user funds; the cardholder can stop spending at any time (BR-6).
- The issuer needs the tokens secured at authorization (PRD §3.3.1 C).
- A compromised operator key must have a bounded effect (G-3).

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | ERC-20 allowance on a regular wallet (EOA). Market example: MetaMask Card | Works with any wallet; one `approve`; small contract | No hold: funds can move before the debit; unlimited approvals are risky |
| 2 | Smart account with modules. Market example: Gnosis Pay (Safe, Roles and Delay modules) | The issuer's debit wins the race: the user's own transfers wait 3 min; limits on-chain | The user moves funds to a new account; modules add attack surface; every own transfer is delayed |
| 3 | EOA with delegated code (EIP-7702) or an ERC-4337 account with session keys | Smart-account rules without moving funds | Newer; wallet support varies; larger design and audit scope |
| 4 | Escrow contract with deposits | Funds guaranteed | Custodial in effect: violates BR-6 |

## Decision

Option 1, with these rules:

- **Pull, not hold:** `debit` moves tokens wallet → treasury in one call. The contract balance stays 0.
- **Fixed route:** token and treasury addresses are immutable. `refund` pays only the wallet stored for the `authId`.
- **On-chain idempotency:** `authId` and `refundId` are single-use.
- **Expiry:** `debit` reverts after `validUntil`.
- **Limits:** per-wallet daily limit by UTC day; default 0; set by `ADMIN`.
- **Roles:** `OPERATOR` for debit and refund, `ADMIN` for limits and pause. Separate keys.
- **Pause** blocks debits only. Refunds stay available.
- **No upgradeability:** no proxy.
- **Libraries:** OpenZeppelin `AccessControl`, `Pausable`, `SafeERC20`. Custom errors.
- **Out of MVP:** `permit` (EIP-2612) to remove the separate approve transaction.

## Trade-offs

- No hold: the race between check and debit is handled off-chain (ADR-12).
- A compromised operator can debit only to the treasury, up to min(allowance, balance, daily limit) per wallet per day, until `ADMIN` pauses, and can refund only existing debits to their own wallets. The attacker gains nothing directly; affected cardholders need refunds.
- Refunds depend on the treasury balance and its allowance to the contract.
- Not upgradeable: a fix means a new deployment and a new approval from every cardholder.
- Each per-wallet limit change costs an admin transaction.
- A refund does not restore the daily limit: simpler accounting, stricter for the cardholder.
