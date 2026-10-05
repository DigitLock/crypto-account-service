# Test Plan — S2 card-auth

- **Milestone:** S2 — card-auth, version `v0.3.0`, branch `feature/v0.3.0`.
- **Objects under test:** `card-auth`, the processor CLI, `CardService` in `server`, the migrations of the card tables, the role `cas_card_auth`, the deployment scripts on Base Sepolia, the CI workflows of S2.
- **Parents:** [SRS — Card Spend](srs/card-spend.md) UC-1, UC-2, UC-3; [SRS — Core](srs/core.md) UC-103, UC-104 step 1; [PRD — Card Spend](prd/card-spend.md) EC-1 … EC-12; [BRD](brd.md) BR-6 … BR-11, G-1, G-2; ADR 3, 7, 10, 12, 13.
- **Status:** Draft. Created in the discovery stage of S2 on 2026-10-05, completed stage by stage, signed off in st10 (§6).

## 1. Environment

| Item | Value |
|---|---|
| Go | `1.27.1`; go-ethereum pinned in `go.mod` (version fixed in st2) |
| PostgreSQL | 16. `TEST_DATABASE_URL` owner role; `TEST_DATABASE_URL_SERVER` role `cas_server`; **new** `TEST_DATABASE_URL_CARD_AUTH` role `cas_card_auth` |
| Local chain | Anvil of Foundry 1.8.3, chain ID 31337, started by the tests (`anvil` on `PATH`). Contracts deployed by the tests from the frozen ABI and the build of `contracts/` |
| Public chain | Base Sepolia, chain ID 84532: phase 8 only. Alchemy primary, `https://sepolia.base.org` fallback. Keys in `.env` only |
| CRS | A fake gRPC server in tests. A local CRS for phase 8 when a non-USD authorization is demonstrated |
| Chain listener | A fake WebSocket JSON-RPC server in tests (phase 6); the provider in phase 8 |
| Runs | `go test -race -count=1 -p 1 ./...` |
| Secrets | Operator, admin, treasury and cardholder keys of the tests are generated at run time and funded with `anvil_setBalance`. No real key anywhere |

## 2. Conventions

- Row format: `ID | Description | Preconditions | Steps | Expected | Req | Status`. A section without a document name is of SRS — Card Spend.
- ID `S2-Tnnn`; the hundreds digit is the phase. A Go test names its row: `TestT411_SameAuthIdSameBody`.
- `Status`: `—` not run, `Pass`, `Fail <attempt>`, `Blocked`.
- Severity: P0 a double debit, a lost return, a secret or another tenant's data exposed, a transaction on a chain outside the allow-list; P1 a requirement of the scope not met; P2 metric, CLI or message; P3 cosmetic.
- Time in tests is a fake clock where the chain is not involved; Anvil time is moved with `evm_setNextBlockTimestamp` and `evm_increaseTime`.
- **Common** preconditions: migrated test database; tenant A with a Basic pair; Anvil with `MockUSDC` and the controller deployed; operator with gas and `OPERATOR`; a cardholder wallet with 100 USDC, allowance 100 USDC, wallet daily limit 50 USDC; a card `card_A` of tenant A bound to that wallet, daily limit 200 USDC, status `ACTIVE`; treasury with allowance to the controller.
- **Tenant B:** a second tenant with its own Basic pair, wallet and card.

## 3. Phases

| Phase | Scope | Stage | Rows |
|---|---|---|---|
| 1 | Configuration, health, start checks | st2 | 7 |
| 2 | Contracts of the APIs | st3 | 7 |
| 3 | Schema, roles, card registry | st4 | 19 |
| 4 | Decision on Anvil | st5 | 26 |
| 5 | Returns and tracker | st6 | 22 |
| 6 | Chain listener | st7 | 8 |
| 7 | Processor CLI and hard tests | st8 | 11 |
| 8 | Base Sepolia | st9 | 9 |
| 9 | CI and repository | st2, st10 | 6 |

## 4. Test matrix

### Phase 1 — Configuration, health, start checks (st2)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T101 | Required configuration | — | Start `card-auth` without each of: `OPERATOR_PRIVATE_KEY`, `DATABASE_URL`, `rpc_url`, `chain_id`, `controller_address`, `token_address`, `token_decimals` | Exit ≠ 0; the message names the variable and prints no value | §3.1 | — |
| S2-T102 | Defaults | Required variables only | Read the parsed configuration | `decision_deadline` 2.5 s, `debit_validity` 4 s, `rpc_read_timeout` 500 ms, `receipt_poll_interval` 200 ms, `quote_buffer_bps` 100, `finality_mode` `confirmations` 10, `tracker_interval` 2 s, `return_retry_interval` 30 s, HTTP 8092, health 8093 | §3.1 | — |
| S2-T103 | Invalid configuration | — | `debit_validity` ≤ `decision_deadline` + 1 s; `chain_id` outside the allow-list; a key that is not 32 bytes; `finality_mode` unknown; a malformed duration | Exit ≠ 0 in each case | §3.1, handoff §4 | — |
| S2-T104 | Health | `card-auth` running | `GET /healthz`, `/readyz`, `/metrics` on 8093; make the database unreachable; `/readyz` | 200, 200, 200 text format; then 503 | §3.1 | — |
| S2-T105 | Operator key never printed | Log captured at `debug` | Start, run one authorization, fail one send | The log, the configuration dump and every error contain neither the key nor `DATABASE_URL` | §3.2 Security, handoff §4 | — |
| S2-T106 | Start checks of the chain | Anvil | Start against an endpoint whose `eth_chainId` ≠ `chain_id`; against a controller whose `token()` ≠ `token_address`; against a token whose `decimals()` ≠ `token_decimals` | Exit ≠ 0 in each case; nothing sent | §3.2 Chain access, handoff §4 | — |
| S2-T107 | Nonce checked at start | Anvil; `operator_accounts.next_nonce` below and above the chain's count | Start | Below: `next_nonce` raised to the chain's count, log line. Above: kept (slots reserved, UC-3 row 9 resolves them) | ADR-10 | — |

### Phase 2 — Contracts of the APIs (st3)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T201 | OpenAPI valid | `api/openapi/card-auth.yaml` (path fixed in st3) | Validate with the linter chosen in st2 | Valid OpenAPI 3.1; no warning of the ruleset | ADR-7, §2.1 | — |
| S2-T202 | OpenAPI against the SRS | Contract | Compare paths, bodies, codes with §2.1.1 – §2.1.4, the error body and its codes included | Three operations; every field with the stated type and requiredness; codes 200, 401, 404, 409, 422; amounts are strings; `securitySchemes` Basic | §2.1.1 – §2.1.4 | — |
| S2-T203 | Responses match the schema | `card-auth` running | Every response of phases 4 and 5 is validated against the OpenAPI in the test harness | No schema violation | §2.1 | — |
| S2-T204 | `CardService` in the proto | `proto/cas/v1/card_service.proto` | `buf lint`; read the descriptors | Six methods: `RegisterCard`, `UpdateCard`, `GetCard`, `ListCards`, `GetAuthorization`, `ListAuthorizations`; `STANDARD` rules pass; no `GetReconciliationReport` | SRS — Core §2.1.1, §2.1.5 | — |
| S2-T205 | Additive change only | Image of C1 | `buf breaking --against` the image of `v0.2.0` | Passes: nothing of C1 changed | C1 summary §4 | — |
| S2-T206 | New frozen image | st3 commit | `make proto-freeze`; after the commit `make proto-check` | New `proto/frozen/cas_v1.json`; `proto-check` green; the old image is in git history | QA gate st3 | — |
| S2-T207 | Freeze guard still works | New image | On a throw-away copy remove a field of `CardService`; add one | `buf breaking` fails; the image comparison fails | C1-T205 | — |

### Phase 3 — Schema, roles, card registry (st4)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T301 | Apply on a clean database | Empty database | Migrations up; down to the version of C1; up again | Six new tables: `cards`, `authorizations`, `authorization_events`, `returns`, `operator_txs`, `operator_accounts`; the ten tables of C1 unchanged; down removes exactly the six | §2.4, SRS — Core §2.4 | — |
| S2-T302 | Uniqueness | Migrated database | Insert duplicates of: `cards (tenant_id, card_ref)`; `authorizations (tenant_id, auth_id)`; `authorizations.chain_auth_id`; `returns (tenant_id, return_id)`; `returns.chain_refund_id`; `operator_txs (chain_id, operator_address, nonce)` | Refused in each case | §2.4 | — |
| S2-T303 | Value constraints | Migrated database | Unknown `status`, `decline_reason`, `type`, `purpose`; negative `token_amount`, `debited_amount`, `returned_amount`, `daily_limit`; `chain_auth_id` of 31 bytes; `wallet_address` of 19 bytes | Refused in each case | §2.4, handoff §4 | — |
| S2-T304 | Rights of `cas_card_auth` | Role `cas_card_auth` | As `cas_card_auth`: DDL; `SELECT credentials_enc FROM connections`; `UPDATE`/`DELETE` on `authorization_events`; `INSERT`/`UPDATE`/`DELETE` on `cards`, `tenants`, `api_credentials`; `DELETE` on `authorizations`, `returns`, `operator_txs` | Permission denied in each case | SRS — Core §3.2, ADR-3 | — |
| S2-T305 | Rights of `cas_card_auth`, allowed | Role `cas_card_auth` | `SELECT` on `cards`, `connections` without `credentials_enc`, `tenants`, `api_credentials`, `sources`; `INSERT`/`UPDATE` on `authorizations`, `returns`, `operator_txs`, `operator_accounts`; `INSERT` on `authorization_events`, `audit_log`; `SELECT` on `schema_migrations` | Allowed | SRS — Core §3.2 | — |
| S2-T306 | Rights of `cas_server` on the card tables | Role `cas_server` | `SELECT` on `authorizations`, `authorization_events`, `returns`; `INSERT`/`UPDATE`/`DELETE` on them; `INSERT`/`UPDATE` on `cards` | Reads allowed; writes on the card-auth tables denied; `cards` writes allowed | SRS — Core §3.2 | — |
| S2-T307 | Role separation in the processes | Both binaries built | Start `server` with `TEST_DATABASE_URL_CARD_AUTH` and `card-auth` with `TEST_DATABASE_URL_SERVER` | Each refuses or fails its first write: the roles are not interchangeable | ADR-3 | — |
| S2-T308 | `RegisterCard` | Common | `RegisterCard` with a wallet connection of the owner | Response of §2.1.5: `ACTIVE`, `wallet_address` EIP-55, `daily_limit` echoed; row in `cards`; audit `CARD_REGISTERED` with the acting credential | FR-115, FR-117, SRS — Core §2.1.5 | — |
| S2-T309 | `RegisterCard` repeated | T308 | Same request again; then with another `daily_limit`; then with another connection | Existing card returned; `ALREADY_EXISTS`; `ALREADY_EXISTS` | SRS — Core §2.1.5 | — |
| S2-T310 | EC-113 | Common; an exchange connection; a `CREDENTIALS_INVALID` wallet connection; a wallet of another owner | `RegisterCard` on each | `FAILED_PRECONDITION` in each case; nothing stored | EC-113 | — |
| S2-T311 | Validation of `CardService` | Common | `card_ref` empty and 65 characters; `owner_ref` 129; `daily_limit` empty, `-1`, `1.5`, `1e6`, 79 digits; malformed `connection_id`; unknown `connection_id` | `INVALID_ARGUMENT` for every malformed value; `NOT_FOUND` for the unknown ID | SRS — Core §2.1.1, §2.1.5 | — |
| S2-T312 | `UpdateCard` | T308 | Change the limit; freeze; unfreeze; freeze twice | Each change stored with `updated_at`; audit `CARD_UPDATED` with the changed fields; a second freeze changes nothing and writes no audit row; `UpdateCard` with neither field → `INVALID_ARGUMENT` | FR-115, FR-117, UC-103 | — |
| S2-T313 | `GetCard`, `ListCards` | Three cards of two owners | `GetCard`; `ListCards` all and by `owner_ref`; pages | Fields of §2.1.5; order `created_at`, `card_ref`; pagination as `ListConnections`; unknown `card_ref` → `NOT_FOUND` | FR-115 | — |
| S2-T314 | Tenant isolation of cards | Tenant B | As B: `GetCard`, `UpdateCard`, `ListCards` on A's card; `RegisterCard` on A's connection | `NOT_FOUND`; `NOT_FOUND`; only B's cards; `NOT_FOUND` for the connection | FR-105, BR-13 | — |
| S2-T315 | EC-114 | A connection with one card; a connection without cards | `DeleteConnection` on each | `FAILED_PRECONDITION / CONNECTION_HAS_CARDS`, nothing deleted, no audit row; the other is deleted. The check runs in the delete transaction: a card registered concurrently is not orphaned | EC-114, SRS — Core UC-104 | — |
| S2-T316 | Several cards on one wallet | Common | Register two cards on the same connection | Both created; the wallet daily limit of the contract caps them together (checked in T409) | SRS — Core §2.1.5 | — |
| S2-T317 | `GetAuthorization` | Authorizations of T502 and T507 (phase 5); tenant B | `GetAuthorization` on each; on an unknown ID; as B on A's ID | The fields of SRS — Card Spend §2.1.4 with `returns` and `history`; the tombstone as there; `NOT_FOUND`; `NOT_FOUND`. Served with the role `cas_server` | FR-116, FR-105, SRS — Core §2.1.1 | — |
| S2-T318 | `ListAuthorizations` | 30 authorizations of two cards and two owners, several statuses and times; tenant B | List all; by `card_ref`, `owner_ref`, `status`, `received_from`/`to`, combined; an unknown `status`; pages; as B | Order `received_at` descending, `auth_id`; filters AND, from inclusive, to exclusive; `INVALID_ARGUMENT`; every row once; B sees only B's rows; tombstones included | FR-116, FR-105, SRS — Core §2.1.1 | — |
| S2-T319 | EC-115 | `card_A` with an authorization blocked at step 12 | `UpdateCard`: lower the limit below the in-flight amount and freeze; release the authorization | The in-flight authorization keeps the values it read and is approved; the next one is declined `CARD_FROZEN` | EC-115, UC-103 | — |

### Phase 4 — Decision on Anvil (st5)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T401 | Basic authentication | Common | `POST /v1/authorizations` without a header; unknown username; wrong password; revoked pair; pair of a disabled tenant; a `SERVICE_TOKEN` key_id as username | `401` with the body `{"error":{"code":"UNAUTHENTICATED"…}}`, the same in all six cases | UC-1 step 1, §2.1.1 | — |
| S2-T402 | Validation | Common | `auth_id` empty and 65 characters; `amount` `0`, `-1`, `abc`, `1e2`, 5 decimal places; `currency` `eur`, `EU`, `EURO`; missing `card_ref`; a body that is not JSON; a body over the size limit | `422 INVALID_REQUEST`; nothing stored; no lock taken | UC-1 step 2, §2.1.1 | — |
| S2-T403 | Approve, USD | Common | Authorize 25.40 USD | `200 APPROVED`: `token_amount` `25400000`, no `quote`, `tx_hash`; on chain one `Debited`; row `APPROVED`; history `RECEIVED → DEBIT_SUBMITTED → APPROVED` | UC-1, FR-2, FR-11 | — |
| S2-T404 | Approve, EUR | Common; CRS fake serves `EUR→USD` 1.1642 | Authorize 25.40 EUR | `token_amount` `29866387`; `quote.rate` `1.1642`, `buffer_bps` 100; `rate` and `buffer_bps` stored; the rate is used as served, no inversion | UC-1 step 7, FR-7, §3 of the package | — |
| S2-T405 | Rate with 10 decimal places | CRS fake serves a `double` with more than 10 significant decimals | Authorize | The quote uses the rate formatted to 10 decimal places, then decimal arithmetic; no float in the stored value | §2.1.2 | — |
| S2-T406 | Currency and rate failures | Common | Authorize in `GBP` (no pair); CRS unreachable; CRS answers `is_outdated` | `DECLINED / CURRENCY_NOT_SUPPORTED`; `RATE_UNAVAILABLE`; `RATE_UNAVAILABLE`. No transaction | EC-10, FR-9 | — |
| S2-T407 | Card checks | Common; `card_B` frozen; unknown `card_ref` | Authorize on each | `CARD_FROZEN`; `CARD_NOT_FOUND`; no transaction | EC-11, FR-8 | — |
| S2-T408 | Card daily limit | Card limit 30 USDC; one approved 25.40 today; one `TIMED_OUT` of 10 | Authorize 5 USD; 4 USD; move the clock past the UTC day boundary; authorize 5 USD | `LIMIT_EXCEEDED`; approved (the `TIMED_OUT` one does not count); approved after the boundary | UC-1 step 8, FR-8 | — |
| S2-T409 | On-chain checks | Common | Authorize above the balance; above the allowance; above the remaining wallet daily limit; with the contract paused | `INSUFFICIENT_FUNDS`; `INSUFFICIENT_ALLOWANCE`; `LIMIT_EXCEEDED`; `PROGRAM_PAUSED`. No transaction sent in any case | EC-3, EC-11, FR-8 | — |
| S2-T410 | Read block tag | Common | Capture the RPC requests of step 9 | Every read uses the `pending` tag; the four reads are in one batch and finish within `rpc_read_timeout` | UC-1 step 9, §3.2 | — |
| S2-T411 | Chain unavailable | Common | RPC returns an error; RPC answers after 600 ms; RPC refuses the connection | `DECLINED / CHAIN_UNAVAILABLE` in each case; decision within the deadline | EC-10, FR-9 | — |
| S2-T412 | Fallback endpoint | Two fake RPC endpoints; `rpc_fallback_after` 3 | Fail the primary once, authorize; fail it three times in a row, authorize; restore the primary, wait one tracker cycle, authorize | `CHAIN_UNAVAILABLE` after one failure, no fallback; after three the authorization is served by the fallback; after the probe the primary is used again; the listener never moves | §3.1, §3.2 Chain access | — |
| S2-T413 | EC-1 same body | T403 | Send the same request three times | The stored decision each time; one `Debited` on chain; one row; `auth_decisions_total` counted once | FR-3, EC-1 | — |
| S2-T414 | EC-2 different body | T403 | Same `auth_id`, amount 25.41; same amount, other `card_ref`; same values, keys in another order | `409`; `409`; stored decision (normalized body); the same with `"25.4"` for `"25.40"` | FR-4, EC-2 | — |
| S2-T415 | Same `auth_id` at once | Common; the chain mines after 1 s | Send the same request from 20 goroutines | One `Debited`; every response is the same decision; the waiters returned the decision, none a conflict | §3.2, FR-3 | — |
| S2-T416 | EC-13 per-card lock | Two authorizations for `card_A`, one other card | Send all three at once | The two of `card_A` run one after another, both decided; the third runs in parallel; one in-flight authorization per card at any time | FR-10, EC-13 | — |
| S2-T417 | Lock not acquired by the deadline | The first authorization of the card blocked at step 12 for 3 s | Second authorization | `DECLINED / TIMEOUT` at the deadline; no transaction for it | UC-1 step 5 | — |
| S2-T418 | FR-5 intent before send | Failure injected between step 10 and the send | Authorize | Row `operator_txs` `PLANNED` with the nonce; the authorization is `DEBIT_SUBMITTED`; nothing on chain; UC-3 resolves it (T516) | FR-5, ADR-10 | — |
| S2-T419 | Nonce allocation | Five sequential authorizations; a failed one in between | Read `operator_txs`, `operator_accounts` | Nonces `n … n+4` without a gap; `next_nonce` = `n+5`; the row is locked during the reservation (a concurrent reservation waits) | §3.2, ADR-10 | — |
| S2-T420 | FR-6 `validUntil` | Clock at `12:00:00.700` | Authorize; decode the calldata of the sent transaction | `validUntil` = `12:00:04` (floored to the second); `valid_until` stored; the contract reverts a debit after it (S1 proves the contract; here the value) | FR-6, ADR-12 | — |
| S2-T421 | Fees | Common; fake RPC with a known base fee and tip | Decode the sent transaction | Type 2; `maxPriorityFeePerGas` = the tip of `eth_maxPriorityFeePerGas`; `maxFeePerGas` = 2 × base fee + tip; `gasLimit` = `debit_gas_limit`; no `eth_estimateGas` call | ADR-10, §3.2 | — |
| S2-T422 | EC-4 reverted debit | Common; the chain mines on demand | Authorize; revoke the allowance before the block is mined; mine | `DECLINED / DEBIT_REVERTED`; `operator_txs` `REVERTED`; `debited_amount` 0 | EC-4, FR-8 | — |
| S2-T423 | EC-5 deadline | Common; mining stopped | Authorize | `DECLINED / TIMEOUT` at `deadline_at` ± 50 ms; row `TIMED_OUT`; continues in T512 / T513 | EC-5, FR-1 | — |
| S2-T424 | EC-15 send result unknown | RPC times out on `eth_sendRawTransaction` but relays it | Authorize | Treated as sent; the inclusion signal of step 12 resolves it to `APPROVED` | EC-15 | — |
| S2-T425 | Internal error | Database failure injected at step 4; at step 10 | Authorize | `200 DECLINED / INTERNAL_ERROR` in both; nothing sent; the processor always gets a decision | §2.1.1 | — |
| S2-T426 | Decision latency and metrics | Twenty authorizations: approvals, declines of each reason, one timeout | Read `/metrics` | `auth_decision_seconds` histogram; `auth_decisions_total{decision,reason}` by outcome; p95 of the approvals on Anvil ≤ 2 s (A failure of the p95 is P2 locally) | §2.5.1, §3.2 | — |

### Phase 5 — Returns and tracker (st6)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T501 | Full reversal | T403 approved | `POST …/returns` without `amount` | `200 ACCEPTED`, `token_amount` `25400000`; tracker: `SUBMITTED → INCLUDED → CONFIRMED`; on chain `Refunded`; `returned_amount` = `refunded` of the contract | UC-2, FR-12, EC-8 | — |
| S2-T502 | Partial returns | 25.40 EUR approved as `29866387` | Return 10.00; then 15.40 | `11758420`; the second takes the whole remainder `18107967`; total = debited exactly | UC-2 step 5, FR-12 | — |
| S2-T503 | EC-17 above the remainder | T502 after the first return | Return 15.41 | `422 RETURN_EXCEEDS_DEBIT`; nothing stored | EC-17 | — |
| S2-T504 | EC-9 same `return_id` | T501 | Same request again; same `return_id` with another amount | Stored result, no second transfer; `409` | FR-13, EC-9 | — |
| S2-T505 | EC-16 decision pending | An authorization in `RECEIVED`; one in `DEBIT_SUBMITTED` | Return on each | `409 AUTHORIZATION_IN_PROGRESS`; nothing stored | EC-16 | — |
| S2-T506 | Nothing to return | Authorizations in `DECLINED`, `TIMED_OUT`, `LATE_DEBIT`, `LATE_DEBIT_REFUNDED`, `DEBIT_LOST` | Return on each | `200 NOTHING_TO_RETURN`, `token_amount` `0`; the return row is stored with that status; no transaction | UC-2 step 4 | — |
| S2-T507 | EC-14 tombstone | Common | Return for an unknown `auth_id`; then authorize with that `auth_id` | `NOTHING_TO_RETURN`; a tombstone row `DECLINED / REVERSED_BEFORE_AUTH` with null body fields; the authorization answers `DECLINED / REVERSED_BEFORE_AUTH` without a lock or a read | FR-15, EC-14 | — |
| S2-T508 | EC-12 treasury cannot fund | Treasury allowance 0 | Full return; raise the allowance after two attempts | `RETRYING`, `attempts` 1 then 2, 30 s apart; `returns_not_confirmed` 1; then `CONFIRMED` | EC-12, FR-14 | — |
| S2-T509 | Refund replayed on chain | T501 confirmed | Re-send the signed refund transaction with a new nonce through the operator queue, outside the API | Reverts `RefundAlreadyUsed`; no second transfer; the service row unchanged | FR-13 | — |
| S2-T510 | Finality on Anvil | T403 approved; `finality_confirmations` 10 | Mine 9 blocks; tracker tick; mine 1; tick | `APPROVED` after 9; `DEBIT_CONFIRMED` after 10; `operator_txs` `CONFIRMED` | FR-19, UC-3 row 1 | — |
| S2-T511 | EC-7 dropped debit, resubmitted | T403 approved, not final | `anvil_snapshot` before the debit, `evm_revert`, mine other blocks; tick | Receipt gone, `authorizations(authId)` 0 → a new debit with the same `authId`, new `validUntil`, same nonce slot if free; `APPROVED` stays; `replaced_hashes` grows; then `DEBIT_CONFIRMED` | UC-3 row 2, FR-18, EC-7 | — |
| S2-T512 | EC-7 resubmission fails | As T511; the allowance revoked after the revert | Tick | `DEBIT_LOST`; `debits_lost_total` 1; an open return of the authorization becomes `NOTHING_TO_RETURN` | UC-3 row 3, FR-18, FR-14 | — |
| S2-T513 | EC-6 late debit | T423 `TIMED_OUT` | Mine the debit at `validUntil − 1 s`; tick; let the return confirm | `LATE_DEBIT`; a return of type `LATE_DEBIT` with a generated `return_id`, full amount; `late_debits_total` 1; then `LATE_DEBIT_REFUNDED`; no manual action | UC-3 rows 4–5, FR-17, EC-6 | — |
| S2-T514 | Expired after timeout | T423 `TIMED_OUT` | Move the chain time past `validUntil`; mine; tick | The debit reverts `AuthExpired` or stays unmined with `authorizations(authId)` 0: `DECLINED / TIMEOUT`; `operator_txs` `REVERTED` or released (T516) | UC-3 row 6 | — |
| S2-T515 | Stuck before `validUntil` | Base fee raised above the sent fee cap; mining on | Tick | Replacement with the same nonce, both fee fields +25 %; old hash in `replaced_hashes`; `operator_tx_pending_seconds` grows then resets | UC-3 row 7, FR-20 | — |
| S2-T516 | Release after `validUntil` | Debit unmined past `validUntil` | Tick | A zero-value self-transfer with the same nonce; row `purpose` `RELEASE`, status `RELEASED`; the next authorization uses the next nonce and mines | UC-3 row 8, FR-20 | — |
| S2-T517 | FR-16 restart between send and receipt | Debit sent, process killed before the signal | Start; tick | On-chain state read first: `debited > 0` → `APPROVED` without a second send; `debited = 0` and not expired → wait; expired → T514 | UC-3 row 9, FR-16, EC-18 | — |
| S2-T518 | Restart with a `PLANNED` slot | T418 state, `validUntil` not reached; then the same with `validUntil` passed | Start; tick; authorize once more | First: the planned debit is sent and resolved. Second: `DECLINED / TIMEOUT`, the next authorization takes the free slot; never two transactions for one nonce | UC-3 row 9, FR-16, FR-20 | — |
| S2-T519 | Restart with an open return | Return `SUBMITTED`, process killed | Start; tick | `refundUsed(refundId)` read first; `true` → `INCLUDED`/`CONFIRMED` without a second send; `false` → resent | FR-16, FR-13 | — |
| S2-T520 | Reorg detection | T403 approved | Change the hash of the stored block by `evm_revert` and re-mining | The tracker notices the changed `block_hash` and re-reads the state before any decision | ADR-10 | — |
| S2-T521 | `GET /v1/authorizations/{auth_id}` | Authorizations of T502 and T507 | `GET` each; `GET` an unknown ID; as tenant B `GET` A's ID | Body of §2.1.4 with `returns` and `history`; the tombstone as §2.1.4 states; `404`; `404` | §2.1.4, FR-116, FR-105 | — |
| S2-T522 | Tracker metrics | Scenario of this phase | Read `/metrics` | `returns_not_confirmed`, `operator_tx_pending_seconds`, `operator_gas_balance`, `treasury_refund_capacity`, `late_debits_total`, `debits_lost_total` with the expected values | §2.5.1 | — |

### Phase 6 — Chain listener (st7)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T601 | Subscription | Fake WebSocket server; `rpc_ws_url` set; `listener_subscription` `pendingLogs`, then `logs` | Start with each | `eth_subscribe` with the configured type and `{address: controller, topics: [Debited]}` once; `chain_listener_connected` 1; no subscription to `newHeads` or `newFlashblocks` | ADR-13, FR-23, decision 3 | — |
| S2-T602 | Signal from the subscription | T601; polling answers no receipt | Authorize; the fake server emits the `Debited` log of the `authId` | `APPROVED` on the log; `inclusion_signals_total{source="subscription"}` 1; the later receipt changes nothing | FR-2, FR-23 | — |
| S2-T603 | Signal from polling | T601; the fake server stays silent | Authorize; the chain mines | `APPROVED` on the receipt; `inclusion_signals_total{source="polling"}` 1; a log arriving later is ignored | FR-23 | — |
| S2-T604 | Log of another `authId` or contract | T601 | The fake emits logs with another `authId`, another address | Ignored; no state change | ADR-13 | — |
| S2-T605 | EC-19 listener down | T601 | Close the fake server; authorize | `chain_listener_connected` 0; the authorization is decided by polling; no error to the processor | EC-19, FR-23 | — |
| S2-T606 | Reconnect | T605 | Start the fake server again | Reconnect with backoff and jitter; resubscribe; `chain_listener_connected` 1; ping answered | ADR-13 common rules | — |
| S2-T607 | No `rpc_ws_url` | Variable unset | Start; authorize | Starts; metric 0; decisions by polling; one log line | §3.2 Reliability | — |
| S2-T608 | Nothing stored from a log | T602 | Compare the row with the receipt data | `tx_hash`, block and amount of the row come from the receipt read, not from the log payload; the log only signals | ADR-13 | — |

### Phase 7 — Processor CLI and hard tests (st8)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T701 | CLI commands | `card-auth` running | `authorize`, `return`, `get` with the Basic pair from the environment | The three operations of the processor API; the pair is read from the environment and never printed; JSON output | PRD §4.2, package §2 | — |
| S2-T702 | Replay, same process | Common | CLI sends one `auth_id` 100 times, 10 in parallel | One `Debited`; 100 identical answers | Done-when 1, FR-3 | — |
| S2-T703 | Replay across a restart | Common | Send; kill `card-auth` after the send and before the signal; start; send the same `auth_id` again | One `Debited`; the second answer equals the first | Done-when 1, FR-16 | — |
| S2-T704 | Concurrency | 10 cards, 5 `auth_id` per card, every request sent twice at once | Run | One `Debited` per `auth_id`; per card the debits are in sequence; no `409`; no duplicate nonce | Done-when 1, FR-10 | — |
| S2-T705 | Approve only after the signal | Mining delayed by 800 ms | Authorize while recording the time of the receipt and of the response | The response is after the signal in every run of 20 | Done-when 2, FR-2 | — |
| S2-T706 | Missed deadline, late debit, refund | Mining delayed by 3 s | Authorize; wait | `DECLINED / TIMEOUT`; the debit lands; the return of type `LATE_DEBIT` confirms without any action; final `LATE_DEBIT_REFUNDED` | Done-when 3, FR-17 | — |
| S2-T707 | Full cycle on Anvil | Fresh deployment | Mint, approve, set limit with `cast` as `ADMIN`, authorize 25.40, return 10.00, finality | Balances as in the S1 scenario: wallet 84.60, treasury 15.40, controller 0; every row final | PRD §4.2, S1 scenario | — |
| S2-T708 | All metrics | After T702–T707 | Read `/metrics` | The ten `card-auth` metrics of §2.5.1 exist | §2.5.1 | — |
| S2-T709 | No signer in `server` | Built tree | Check the imports of every package under `cmd/server` and the packages it uses | The signer package of `card-auth` is not imported | ADR-3 | — |
| S2-T710 | Secrets | Database and logs after the phase | Search for the operator key, the Basic password, `DATABASE_URL` | Only the hash of the password in `api_credentials`; nothing else anywhere | §3.2 Security, handoff §4 | — |
| S2-T711 | Processor credential CLI | Migrated database | `casctl` issues a Basic pair for tenant A; lists; revokes | Printed once; only the hash stored; audit `CREDENTIAL_ISSUED`, `CREDENTIAL_REVOKED`; `401` after revocation | SRS — Core UC-105 | — |

### Phase 8 — Base Sepolia (st9)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T801 | Chain guard | Scripts of S2 | Run `Deploy.s.sol` on Anvil 31337; on an Anvil fork with chain ID 1; on 84532 (dry run) | Allowed; refused; allowed | S1 summary §6, package 1.6 | — |
| S2-T802 | Deployment | Funded test accounts in `.env` | Deploy `MockUSDC` and the controller on Base Sepolia | Addresses and transaction links recorded in the Deployment Guide and the test plan; roles granted; no personal wallet | Package §2 Deliverables, handoff §4 | — |
| S2-T803 | Start checks on the public chain | T802 | Start `card-auth` against Alchemy; against the fallback | Both pass T106; chain ID 84532 | Handoff §4 | — |
| S2-T804 | Provider test recorded | Owner's test of st1 | Read SRS §4 issue 1 | `pendingLogs` and preconfirmed receipts confirmed on the free plan, dated; or the fallback rule applied | Package 1.3 | — |
| S2-T805 | End to end | T802; wallet funded and approved; limit set with `cast` | Authorize 5 USD; return 2 USD; wait for `finalized` | `APPROVED` with a `tx_hash` visible in the explorer; `CONFIRMED` return; `DEBIT_CONFIRMED` by the `finalized` tag (about 20 min); links recorded | Done-when, FR-19 | — |
| S2-T806 | p95 measurement | T805 | 30 USD authorizations in sequence from the development machine, one card, listener on; record provider, time of day, N, signals by source | p95 of `auth_decision_seconds` ≤ 2 s; the value and the conditions in the test plan and the session summary | Done-when 4, §3.2 | — |
| S2-T807 | Signals by source on the public chain | T806 | Read `inclusion_signals_total` | Both sources counted; subscription first in most runs when Flashblocks work | FR-23 | — |
| S2-T808 | Budget | After T806 | Read the Alchemy dashboard | CU of the run recorded; per authorization near the estimate of 400 CU | Package §3 | — |
| S2-T809 | Deployment Guide | Fresh clone | Follow the guide from the start | Every step works as written; no step needs a key in a committed file | Package §2 Deliverables | — |

### Phase 9 — CI and repository (st2, st10)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T901 | `go` workflow | Branch | Push | Role `cas_card_auth` created in the job; Foundry installed in the job and the chain tests run against Anvil; green | Package st2 | — |
| S2-T902 | `contracts` workflow on `main` | Decision 4 applied | Merge | The workflow runs on `main` when `contracts/**` changed | Decision 4 | — |
| S2-T903 | `proto` workflow with the new image | st3 commit | Push | Green against the new image | QA gate st3 | — |
| S2-T904 | Secret scan | Branch | Push | Green; `.env.example` holds names only; no address of an internal host | Handoff §4 | — |
| S2-T905 | Documents match the code | st10 | Compare SRS — Card Spend, SRS — Core, ADR-12, ADR-13, glossary, `docs/README.md` with the code | No mismatch | st10.2 | — |
| S2-T906 | No signer import rule in CI | Workflow | A throw-away branch imports the signer from `cmd/server` | The `go` job fails | ADR-3 | — |

### Coverage of the requirements

| FR | Rows |
|---|---|
| FR-1 | T417, T423 |
| FR-2 | T403, T602, T705 |
| FR-3 | T413, T415, T702 |
| FR-4 | T414 |
| FR-5 | T418 |
| FR-6 | T420 |
| FR-7 | T404, T405 |
| FR-8 | T407, T408, T409, T422 |
| FR-9 | T406, T411 |
| FR-10 | T416, T704 |
| FR-11 | T403 |
| FR-12 | T501, T502 |
| FR-13 | T504, T509, T519 |
| FR-14 | T508, T512 |
| FR-15 | T507 |
| FR-16 | T517, T518, T519, T703 |
| FR-17 | T513, T706 |
| FR-18 | T511, T512 |
| FR-19 | T510, T805 |
| FR-20 | T515, T516 |
| FR-23 | T601, T602, T603, T605 |
| FR-115 | T308, T312, T313 |
| FR-116 | T317, T318, T521 |
| FR-117, cards | T308, T312 |

| EC | Rows | EC | Rows |
|---|---|---|---|
| EC-1 | T413 | EC-11 | T407, T409 |
| EC-2 | T414 | EC-12 | T508 |
| EC-3 | T409 | EC-13 | T416 |
| EC-4 | T422 | EC-14 | T507 |
| EC-5 | T423, T514, T515, T516 | EC-15 | T424 |
| EC-6 | T513 | EC-16 | T505 |
| EC-7 | T511, T512 | EC-17 | T503 |
| EC-8 | T501 | EC-18 | T517 |
| EC-9 | T504 | EC-19 | T605 |
| EC-10 | T406, T411 | EC-113 | T310 |
| | | EC-114 | T315 |
| | | EC-115 | T319 |

### Coverage of the exit criteria

| Criterion | Rows |
|---|---|
| Same `auth_id` N times → one debit, across restart and concurrency | T413, T415, T702, T703, T704 |
| `APPROVED` only after an inclusion signal | T403, T602, T705 |
| Missed deadline → decline; late debit refunded without manual action | T423, T513, T706 |
| p95 ≤ 2 s on Base Sepolia | T806 |

## 5. Run log

Filled in st10.

## 6. Success criteria and sign-off

- Every row of §4 is `Pass`, locally and in CI; phase 8 rows by the owner on Base Sepolia.
- 0 open P0 and P1.
- OpenAPI and the proto equal their frozen forms; the generated code is current.
- `docs/srs/card-spend.md` and `docs/srs/core.md` match the code.

## 7. Open items of the draft

- Rows of phase 8 are run by the owner; their evidence — addresses, transaction links, the p95 value — goes into §7 and the Deployment Guide.
- Phase 8 needs the test accounts and the test ETH of package §5.
