# Crypto Account Service — Documentation

Source of truth for CAS requirements and design. Markdown in the repository.

## Document map

| Document | Path | Answers | Status |
|---|---|---|---|
| BRD | `brd.md` | Why, and what the business needs | Approved |
| PRD — Card Spend | `prd/card-spend.md` | What the card spend module does, for whom, in which stages | Approved |
| PRD — Exchange Accounts | `prd/exchange-accounts.md` | What exchange connections deliver, in which stages | Approved |
| SRS — Core | `srs/core.md` | Tenants, connections, sync engine, ledger model, gRPC API | Approved |
| SRS — Card Spend | `srs/card-spend.md` | Authorization flow, states, edge cases, contract, idempotency | Approved |
| SRS — Binance | `srs/exchange-binance.md` | Key check, limits, balances and history sync for Binance | Approved |
| SRS — EVM Connector | `srs/evm-connector.md` | Wallet balances, event logs from final blocks, reorg guard, completeness check, inputs for reconciliation | Approved |
| ADR | `adr/` | One decision per file | 1–13 accepted |
| C4 | `c4/context.md`, `c4/container.md` | Context and container diagrams | Approved |
| Glossary | `glossary.md` | Card, on-chain, exchange and CAS terms | Approved |
| Roadmap | `roadmap.md` | Order of the milestones, versions, status, log | Living |
| Test Plan — S1 | `test-plan-s1.md` | Test matrix of the contracts, run log, sign-off of S1 | Signed off 2026-10-04 |
| Test Plan — C1 | `test-plan-c1.md` | Test matrix of Core, run log, sign-off of C1 | Approved |
| Test Plan — S2 | `test-plan-s2.md` | Test matrix of `card-auth` and the card registry, run log, sign-off of S2 | Approved |
| Test Plan — S3 | `test-plan-s3.md` | Test matrix of the EVM connector, the treasury connection and reconciliation, run log, sign-off of S3 | Approved |
| Deployment Guide | `deployment-guide.md` | How to deploy the contracts and run `card-auth` on Base Sepolia; test accounts, admin operations with `cast`; from S3, `server` with the EVM connector and reconciliation (§13) | Pre-approved, used in S2 st9b and S3 st8b |
| Backlog | `backlog.md` | Postponed items | Living |

Status: `Draft` → `Pre-approved` (reviewed by the owner) → `Approved` (merged to `main`). `Living`: updated after each milestone, no approval status.

## Conventions

- **Language:** English.
- **Form:** lists, tables, Mermaid diagrams. No narrative paragraphs.
- **Structure:** section numbers mirror the BRD / PRD / SRS templates.
- **Unused section:** keep the heading, write `N/A — <reason>`.
- **Requirement:** `BR` = ID + one statement with "must" + acceptance criterion. `FR` = ID + one testable statement with "must", listed under the acceptance criteria of its use case.
- **Scope tag:** every `BR`, `US` and use case carries a milestone ID, `Design only` (outlined, not built) or `Backlog` (recorded, not specified). An `FR` has the milestone of its use case.
- **Amounts:** decimal strings or integer base units. Never floats.
- **ADR:** Context → Options → Decision → Trade-offs. Status: Proposed, Accepted, Superseded.

## Identifiers

| Prefix | Meaning | Defined in |
|---|---|---|
| `G-n` | Business goal | BRD §4 |
| `BR-n` | Business requirement | BRD §8 |
| `FR-n` | Functional requirement. Non-functional requirements are not numbered: SRS §3 | SRS §2 |
| `US-n` | User story | PRD §3.2 |
| `EC-n` | Edge case | PRD §3.3.2, SRS |
| `UC-n` | Use case | SRS §2.3 |
| `ADR-n` | Architecture decision | `adr/` |
| `C`, `S`, `X`, `W`, `E` + number | Milestone: Core, card Spend, eXchanges, WebSocket triggers, Ecosystem | BRD §7.3 |

Traceability: `G → BR → US → FR → test`. A PRD names the goals and BRs it delivers (§2.1). A `US` names its `BR`; an `FR` names its `US` or `BR`. Where no `BR` exists, the parent is a need of BRD §9.2.

IDs are unique across documents. `US`, `UC`, `EC` and `FR` numbers come from the range of their module:

| Module | Range |
|---|---|
| Card Spend | 1–99 |
| Core | 101–199 |
| Exchange Accounts, Binance | 201–299 |
| EVM Connector | 301–399 |
