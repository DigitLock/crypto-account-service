# ADR-12 — Authorization decision timing

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-7, BR-8, G-1, G-2, ADR-8, ADR-9, SRS — Card Spend UC-1, UC-3

## Context

- The processor waits 2–3 s for a decision, then decides by its own rules: 2 s at Stripe Issuing, 3 s at Marqeta.
- The allowance model has no hold (ADR-9): between the check and the debit the user can move the funds or revoke the allowance.
- Base produces a block every 2 s and a preconfirmation every 200 ms. A preconfirmed transaction can still be dropped; the network targets below 0.1%.
- Finality on L1 takes minutes.

## Options

| # | Option | Latency | Cons |
|---|---|---|---|
| 1 | Approve after checks and reads; debit asynchronously | < 1 s | The debit can fail after approval: the issuer funds the purchase |
| 2 | Approve after the debit is preconfirmed or included | 0.3–2.5 s | A debit can land after the deadline; a preconfirmed debit can be dropped |
| 3 | Approve after finality | Minutes | Outside any card time budget |
| 4 | Delay the user's own transfers on-chain, approve before the debit | < 1 s | Needs the smart-account model (ADR-9, option 2) |

## Decision

Option 2, with four rules:

1. **Deadline:** no inclusion signal within `decision_deadline` (2.5 s) → decline.
2. **Expiry:** every debit carries `validUntil` = received time + `debit_validity` (4 s), in whole seconds. After it the contract rejects the debit. A declined authorization can therefore be debited only inside a short window after the deadline: about 1.5 s, up to one block longer, because the contract compares `validUntil` with the block timestamp.
3. **Late debit:** a debit that lands inside that window is returned in full automatically.
4. **Lost debit:** an approved debit that is dropped is resubmitted with the same `authId`. If that fails, the authorization becomes `DEBIT_LOST`: issuer exposure and an alert.

Also:

- Fail-closed: RPC or rate source unavailable → decline. No stand-in approvals in v1.
- The final status is set asynchronously by the finality rule of the network: the `finalized` block tag on Base, N confirmations on the local chain (SRS — EVM Connector §2.1.1).

## Trade-offs

- Approvals depend on chain liveness: a sequencer or RPC outage declines every payment.
- A small credit risk remains for dropped preconfirmations (rule 4). Accepted: rare, bounded by the daily limit, visible in metrics.
- The decline rate under load follows inclusion latency. Watched through `auth_decision_seconds` and the share of `TIMEOUT`.
- Gas is spent on debits that are later returned.
- The default deadline of 2.5 s fits a 3 s processor budget. A 2 s budget needs a lower deadline and therefore depends on preconfirmations.
- Without preconfirmations the budget is one block: the deadline must grow to about 3 s, the very limit of a 3 s processor budget.

## Open points

- RPC provider for preconfirmed data on Base Sepolia; the approach is decided in ADR-13 — SRS — Card Spend §4, issue 1.
