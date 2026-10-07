# Test Plan — S2 card-auth

- **Milestone:** S2 — card-auth, version `v0.3.0`, branch `feature/v0.3.0`.
- **Objects under test:** `card-auth`, the processor CLI, `CardService` in `server`, the migrations of the card tables, the role `cas_card_auth`, the deployment scripts on Base Sepolia, the CI workflows of S2.
- **Parents:** [SRS — Card Spend](srs/card-spend.md) UC-1, UC-2, UC-3; [SRS — Core](srs/core.md) UC-103, UC-104 step 1; [PRD — Card Spend](prd/card-spend.md) EC-1 … EC-12; [BRD](brd.md) BR-6 … BR-11, G-1, G-2; ADR 3, 7, 10, 12, 13.
- **Status:** Approved. Created in the discovery stage of S2 on 2026-10-05, completed stage by stage, run in st10a, signed off on 2026-10-07 (§6).

## 1. Environment

| Item | Value |
|---|---|
| Go | `1.27.1`; go-ethereum `v1.17.7` pinned in `go.mod` (st2); bindings by `abigen` of the same version (`make bindings`, `make bindings-check`) |
| PostgreSQL | 16. `TEST_DATABASE_URL` owner role; `TEST_DATABASE_URL_SERVER` role `cas_server`; **new** `TEST_DATABASE_URL_CARD_AUTH` role `cas_card_auth` |
| Local chain | Anvil of Foundry 1.8.3, chain ID 31337, started by the tests (`anvil` on `PATH`). Contracts deployed by the tests from the frozen ABI and the build of `contracts/` |
| Foundry in CI | The `go` workflow checks out the submodules and installs Foundry `v1.8.3` with `foundry-rs/foundry-toolchain@v1` in each of its three jobs: `anvil` and `forge` on `PATH`. `internal/testchain` skips without them locally and fails in CI. From st9a `buf` 1.73.0 is installed for the rehearsal of `scripts/sepolia/` (`internal/rehearsal`), which needs `bash`, `perl`, `jq` and `curl` of the runner as well; from st10a only in the job `hardtest` |
| Public chain | Base Sepolia, chain ID 84532: phase 8 only. Alchemy primary, `https://sepolia.base.org` fallback. Keys in a file outside the repository named by `CAS_SEPOLIA_KEYS` (owner's decision of 2026-10-06; [Deployment Guide](deployment-guide.md) §2) |
| CRS | A fake gRPC server in tests. A local CRS for phase 8 when a non-USD authorization is demonstrated |
| Chain listener | A fake WebSocket JSON-RPC server in tests (phase 6); the provider in phase 8 |
| Runs | `go test -race -count=1 -p 1 ./...`. In CI from st10a the workflow `go` runs the same flags in three parallel jobs, each with its own PostgreSQL 16: `processorapi` (`internal/processorapi`), `hardtest` (`internal/hardtest`, `internal/rehearsal`) and `rest` (every other package of `go list ./...`; format, vet, build, the generated-code checks and a guard that every package runs in exactly one job) |
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
| 2 | Contracts of the APIs | st3 | 8 |
| 3 | Schema, roles, card registry | st4 | 19 |
| 4 | Decision on Anvil | st5a, st5b (§7) | 27 |
| 5 | Returns and tracker | st6a, st6b (§7), st10a | 28 |
| 6 | Chain listener | st7 | 9 |
| 7 | Processor CLI and hard tests | st8 | 10 |
| 8 | Base Sepolia | st9 | 9 |
| 9 | CI and repository | st2, st10 | 6 |

## 4. Test matrix

### Phase 1 — Configuration, health, start checks (st2)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T101 | Required configuration | — | Start `card-auth` without each of: `OPERATOR_PRIVATE_KEY`, `CARD_AUTH_DATABASE_URL`, `CARD_AUTH_RPC_URL`, `CARD_AUTH_CHAIN_ID`, `CARD_AUTH_CONTROLLER_ADDRESS`, `CARD_AUTH_TOKEN_ADDRESS`, `CARD_AUTH_TOKEN_DECIMALS` | Exit ≠ 0; the message names the variable and prints no value | §3.1 | Pass |
| S2-T102 | Defaults | Required variables only | Read the parsed configuration | `decision_deadline` 2.5 s, `debit_validity` 4 s, `rpc_read_timeout` 500 ms, `receipt_poll_interval` 200 ms, `quote_buffer_bps` 100, `finality_mode` `confirmations` 10, `tracker_interval` 2 s, `return_retry_interval` 30 s, HTTP 8092, health 8093 | §3.1 | Pass |
| S2-T103 | Invalid configuration | — | `debit_validity` < `decision_deadline` + 1 s (equal is valid); `min_send_window` equal to or above `decision_deadline`; `chain_id` outside the allow-list; a key that is not 32 bytes; `finality_mode` unknown; a malformed duration | Exit ≠ 0 in each case | §3.1, handoff §4 | Pass |
| S2-T104 | Health | `card-auth` running | `GET /healthz`, `/readyz`, `/metrics` on 8093; make the database unreachable; `/readyz` | 200, 200, 200 text format; then 503 | §3.1 | Pass |
| S2-T105 | Operator key never printed | Log captured at `debug` | Start, run one authorization, fail one send | The log, the configuration dump and every error contain neither the key nor `CARD_AUTH_DATABASE_URL` nor an RPC URL | §3.2 Security, handoff §4 | Pass |
| S2-T106 | Start checks of the chain | Anvil | Start against an endpoint whose `eth_chainId` ≠ `chain_id`; against a controller whose `token()` ≠ `token_address`; against a token whose `decimals()` ≠ `token_decimals` | Exit ≠ 0 in each case; nothing sent | §3.2 Chain access, handoff §4 | Pass |
| S2-T107 | Nonce checked at start | Anvil; `operator_accounts.next_nonce` below and above the chain's count | Start | Below: `next_nonce` raised to the chain's count, log line. Above: kept (slots reserved, UC-3 row 9 resolves them) | ADR-10 | Pass |

### Phase 2 — Contracts of the APIs (st3)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T201 | OpenAPI valid | `api/openapi/card-auth.yaml` | Load through the loader of the package `api/openapi`; validate | Valid OpenAPI 3.0.3, loaded and validated by kin-openapi | ADR-7, §2.1 | Pass |
| S2-T202 | OpenAPI against the SRS | Contract | Compare paths, bodies, codes with §2.1.1 – §2.1.4, the error body and its codes included | Three operations; every field with the stated type and requiredness; codes 200, 401, 404, 409, 422, and 500 for the status query; error codes with `INTERNAL`; amounts are strings; `securitySchemes` Basic. A database failure of the status query answers `500 INTERNAL` without internal detail, valid against the OpenAPI | §2.1.1 – §2.1.4, D-18 | Pass |
| S2-T203 | Responses match the schema | `card-auth` running | Every response of phases 4 and 5 is validated against the OpenAPI in the test harness | No schema violation | §2.1 | Pass |
| S2-T204 | `CardService` in the proto | `proto/cas/v1/card_service.proto` | `buf lint`; read the descriptors | Six methods: `RegisterCard`, `UpdateCard`, `GetCard`, `ListCards`, `GetAuthorization`, `ListAuthorizations`; `STANDARD` rules pass; no `GetReconciliationReport` | SRS — Core §2.1.1, §2.1.5 | Pass |
| S2-T205 | Additive change only | Image of C1 | `buf breaking --against` the image of `v0.2.0` | Passes: nothing of C1 changed | C1 summary §4 | Pass |
| S2-T206 | New frozen image | st3 commit | `make proto-freeze`; after the commit `make proto-check` | New `proto/frozen/cas_v1.json`; `proto-check` green; the old image is in git history | QA gate st3 | Pass |
| S2-T207 | Freeze guard still works | New image | On a throw-away copy remove a field of `CardService`; add one | `buf breaking` fails; the image comparison fails | C1-T205 | Pass |
| S2-T208 | OpenAPI freeze guard | `api/openapi/frozen/card-auth.yaml` | On a throw-away copy: remove a field of a response; remove an operation; add an optional request field. Run `make openapi-check` | `make openapi-check` fails in all three cases: the removed operation by `oasdiff breaking`; the removed response field by `oasdiff breaking` when the field is required, by the byte comparison with the frozen copy when it is optional (`INFO` for `oasdiff`); the added request field by the byte comparison | Owner's decision D-6 | Pass |

### Phase 3 — Schema, roles, card registry (st4)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T301 | Apply on a clean database | Empty database | Migrations up; down to the version of C1; up again | Six new tables: `cards`, `authorizations`, `authorization_events`, `returns`, `operator_txs`, `operator_accounts`; the ten tables of C1 unchanged; down removes exactly the six | §2.4, SRS — Core §2.4 | Pass |
| S2-T302 | Uniqueness | Migrated database | Insert duplicates of: `cards (tenant_id, card_ref)`; `authorizations (tenant_id, auth_id)`; `authorizations.chain_auth_id`; `returns (tenant_id, return_id)`; `returns.chain_refund_id`; `operator_txs (chain_id, operator_address, nonce)` | Refused in each case | §2.4 | Pass |
| S2-T303 | Value constraints | Migrated database | Unknown `status`, `decline_reason`, `type`, `purpose`; negative `token_amount`, `debited_amount`, `returned_amount`, `daily_limit`; `chain_auth_id` of 31 bytes; `wallet_address` of 19 bytes | Refused in each case | §2.4, handoff §4 | Pass |
| S2-T304 | Rights of `cas_card_auth` | Role `cas_card_auth` | As `cas_card_auth`: DDL; `SELECT credentials_enc FROM connections`; `UPDATE`/`DELETE` on `authorization_events`; `INSERT`/`UPDATE`/`DELETE` on `cards`, `tenants`, `api_credentials`; `DELETE` on `authorizations`, `returns`, `operator_txs` | Permission denied in each case | SRS — Core §3.2, ADR-3 | Pass |
| S2-T305 | Rights of `cas_card_auth`, allowed | Role `cas_card_auth` | `SELECT` on `cards`, `connections` without `credentials_enc`, `tenants`, `api_credentials`, `sources`; `INSERT`/`UPDATE` on `authorizations`, `returns`, `operator_txs`, `operator_accounts`; `INSERT` on `authorization_events`, `audit_log`; `SELECT` on `schema_migrations` | Allowed | SRS — Core §3.2 | Pass |
| S2-T306 | Rights of `cas_server` on the card tables | Role `cas_server` | `SELECT` on `authorizations`, `authorization_events`, `returns`; `INSERT`/`UPDATE`/`DELETE` on them; `INSERT`/`UPDATE` on `cards` | Reads allowed; writes on the card-auth tables denied; `cards` writes allowed | SRS — Core §3.2 | Pass |
| S2-T307 | Role separation in the processes | Both binaries built | Start `server` with `TEST_DATABASE_URL_CARD_AUTH` and `card-auth` with `TEST_DATABASE_URL_SERVER` | Each refuses or fails its first write: the roles are not interchangeable | ADR-3 | Pass |
| S2-T308 | `RegisterCard` | Common | `RegisterCard` with a wallet connection of the owner | Response of §2.1.5: `ACTIVE`, `wallet_address` EIP-55, `daily_limit` echoed; row in `cards`; audit `CARD_REGISTERED` with the acting credential | FR-115, FR-117, SRS — Core §2.1.5 | Pass |
| S2-T309 | `RegisterCard` repeated | T308 | Same request again; then with another `daily_limit`; then with another connection | Existing card returned; `ALREADY_EXISTS`; `ALREADY_EXISTS` | SRS — Core §2.1.5 | Pass |
| S2-T310 | EC-113 | Common; an exchange connection; a `CREDENTIALS_INVALID` wallet connection; a wallet of another owner | `RegisterCard` on each | `FAILED_PRECONDITION` in each case; nothing stored | EC-113 | Pass |
| S2-T311 | Validation of `CardService` | Common | `card_ref` empty and 65 characters; `owner_ref` 129; `daily_limit` empty, `-1`, `1.5`, `1e6`, 79 digits; malformed `connection_id`; unknown `connection_id` | `INVALID_ARGUMENT` for every malformed value; `NOT_FOUND` for the unknown ID | SRS — Core §2.1.1, §2.1.5 | Pass |
| S2-T312 | `UpdateCard` | T308 | Change the limit; freeze; unfreeze; freeze twice | Each change stored with `updated_at`; audit `CARD_UPDATED` with the changed fields; a second freeze changes nothing and writes no audit row; `UpdateCard` with neither field → `INVALID_ARGUMENT` | FR-115, FR-117, UC-103 | Pass |
| S2-T313 | `GetCard`, `ListCards` | Three cards of two owners | `GetCard`; `ListCards` all and by `owner_ref`; pages | Fields of §2.1.5; order `created_at`, `card_ref`; pagination as `ListConnections`; unknown `card_ref` → `NOT_FOUND` | FR-115 | Pass |
| S2-T314 | Tenant isolation of cards | Tenant B | As B: `GetCard`, `UpdateCard`, `ListCards` on A's card; `RegisterCard` on A's connection | `NOT_FOUND`; `NOT_FOUND`; only B's cards; `NOT_FOUND` for the connection | FR-105, BR-13 | Pass |
| S2-T315 | EC-114 | A connection with one card; a connection without cards | `DeleteConnection` on each | `FAILED_PRECONDITION / CONNECTION_HAS_CARDS`, nothing deleted, no audit row; the other is deleted. The check runs in the delete transaction: a card registered concurrently is not orphaned | EC-114, SRS — Core UC-104 | Pass |
| S2-T316 | Several cards on one wallet | Common | Register two cards on the same connection | Both created; the wallet daily limit of the contract caps them together (checked in T409) | SRS — Core §2.1.5 | Pass |
| S2-T317 | `GetAuthorization` | Authorizations of T502 and T507 (phase 5); tenant B | `GetAuthorization` on each; on an unknown ID; as B on A's ID | The fields of SRS — Card Spend §2.1.4 with `returns` and `history`; the tombstone as there; `NOT_FOUND`; `NOT_FOUND`. Served with the role `cas_server` | FR-116, FR-105, SRS — Core §2.1.1 | Pass |
| S2-T318 | `ListAuthorizations` | 30 authorizations of two cards and two owners, several statuses and times; tenant B | List all; by `card_ref`, `owner_ref`, `status`, `received_from`/`to`, combined; an unknown `status`; pages; as B | Order `received_at` descending, `auth_id`; filters AND, from inclusive, to exclusive; `INVALID_ARGUMENT`; every row once; B sees only B's rows; tombstones included | FR-116, FR-105, SRS — Core §2.1.1 | Pass |
| S2-T711 | Processor credential CLI | Migrated database | `casctl` issues a Basic pair for tenant A; lists; revokes | Printed once; only the hash stored; audit `CREDENTIAL_ISSUED`, `CREDENTIAL_REVOKED`; `401` after revocation | SRS — Core UC-105 | Pass |

### Phase 4 — Decision on Anvil (st5a, st5b)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T401 | Basic authentication | Common | `POST /v1/authorizations` without a header; unknown username; wrong password; revoked pair; pair of a disabled tenant; a `SERVICE_TOKEN` key_id as username | `401` with the body `{"error":{"code":"UNAUTHENTICATED"…}}`, the same in all six cases | UC-1 step 1, §2.1.1 | Pass |
| S2-T402 | Validation | Common | `auth_id` empty and 65 characters; `amount` `0`, `-1`, `abc`, `1e2`, 5 decimal places, 15 digits before the point; `currency` `eur`, `EU`, `EURO`; `card_ref` missing, empty and 65 characters; a body that is not JSON; a body over the size limit. Then `amount` with 14 digits before the point and a `card_ref` of 64 characters | `422 INVALID_REQUEST`; nothing stored; no lock taken. The last request is accepted | UC-1 step 2, §2.1.1, D-17, D-19 | Pass |
| S2-T403 | Approve, USD | Common | Authorize 25.40 USD | `200 APPROVED`: `token_amount` `25400000`, no `quote`, `tx_hash`; on chain one `Debited`; row `APPROVED`; history `RECEIVED → DEBIT_SUBMITTED → APPROVED` | UC-1, FR-2, FR-11 | Pass |
| S2-T404 | Quote, EUR | Common; CRS fake serves `EUR→USD` with `rate_decimal` `"1.1642000000"` | Authorize 25.40 EUR | st5a, stored values: `token_amount` `29866387`, `rate` `1.1642`, `buffer_bps` 100, `token` `USDC`; one `GetRate(EUR → USD)`: the rate is used as served, no inversion. st5b, response: `quote.rate` `1.1642`, `buffer_bps` 100, `token_amount` `29866387` | UC-1 step 7, FR-7, §3 of the package | Pass |
| S2-T405 | Rate as a decimal | CRS fake serves `rate_decimal` with 10 fractional digits and a wrong `double` in `rate` | Authorize; read the row and the status query | `rate_decimal` parsed exactly as a decimal, the `double` never read; the token amount by integer arithmetic, rounded up once; no float in any amount or rate path; `quote.rate` without trailing zeros: `"1.2500000000"` → `"1.25"`; a `rate_decimal` that is not a positive decimal fitting `NUMERIC(20,10)` → `RATE_UNAVAILABLE` | §2.1.2, D-15 | Pass |
| S2-T406 | Currency and rate failures | Common | Authorize in a currency without a pair, e.g. `JPY` (CRS `NOT_FOUND`); CRS unreachable; CRS answers `is_outdated`; CRS answers an empty `rate_decimal`; CRS answers after the 100 ms budget; `CRS_ADDRESS` unset | `DECLINED / CURRENCY_NOT_SUPPORTED`; then `RATE_UNAVAILABLE` in every other case. No quote stored, no transaction | EC-10, FR-9, D-14, D-15 | Pass |
| S2-T407 | Card checks | Common; `card_B` frozen; unknown `card_ref` | Authorize on each | `CARD_FROZEN`; `CARD_NOT_FOUND`; no transaction | EC-11, FR-8 | Pass |
| S2-T408 | Card daily limit | Card limit 30 USDC; one approved 25.40 today; one `TIMED_OUT` of 10 | Authorize 5 USD; 4 USD; move the clock past the UTC day boundary; authorize 5 USD | `LIMIT_EXCEEDED`; approved (the `TIMED_OUT` one does not count); approved after the boundary | UC-1 step 8, FR-8 | Pass |
| S2-T409 | On-chain checks | Common | Authorize above the balance; above the allowance; above the remaining wallet daily limit; with the contract paused | `INSUFFICIENT_FUNDS`; `INSUFFICIENT_ALLOWANCE`; `LIMIT_EXCEEDED`; `PROGRAM_PAUSED`. No transaction sent in any case | EC-3, EC-11, FR-8 | Pass |
| S2-T410 | Read block tag | Common | Capture the RPC requests of step 9 | Every read uses the `pending` tag; the four reads are in one batch and finish within `rpc_read_timeout` | UC-1 step 9, §3.2 | Pass |
| S2-T411 | Chain unavailable | Common | RPC returns an error; RPC answers after 600 ms; RPC refuses the connection | `DECLINED / CHAIN_UNAVAILABLE` in each case; decision within the deadline | EC-10, FR-9 | Pass |
| S2-T412 | Fallback endpoint | Two fake RPC endpoints; `rpc_fallback_after` 3 | Fail the primary once, authorize; fail it three times in a row, authorize; restore the primary, wait one tracker cycle, authorize | `CHAIN_UNAVAILABLE` after one failure, no fallback; after three the authorization is served by the fallback; after the probe the primary is used again; the listener never moves | §3.1, §3.2 Chain access | Pass |
| S2-T413 | EC-1 same body | T403 | Send the same request three times | The stored decision each time; one `Debited` on chain; one row; `auth_decisions_total` counted once | FR-3, EC-1 | Pass |
| S2-T414 | EC-2 different body | T403 | Same `auth_id`, amount 25.41; same amount, other `card_ref`; same values, keys in another order | `409`; `409`; stored decision (normalized body); the same with `"25.4"` for `"25.40"` | FR-4, EC-2 | Pass |
| S2-T415 | Same `auth_id` at once | Common; the chain mines after 1 s | Send the same request from 20 goroutines | One `Debited`; every response is the same decision; the waiters returned the decision, none a conflict | §3.2, FR-3 | Pass |
| S2-T416 | EC-13 per-card lock | Two authorizations for `card_A`, one other card | Send all three at once | The two of `card_A` run one after another, both decided; the third runs in parallel; one in-flight authorization per card at any time | FR-10, EC-13 | Pass |
| S2-T417 | Lock not acquired in time | Mining stopped; two authorizations of one card sent at once. Then: the first mined about 0.8 s in | Run each 10 times | The one holding the lock times out with its debit sent; the other gets the lock, if at all, with less than `min_send_window` left: `DECLINED / TIMEOUT` before step 10, status `DECLINED`, no nonce, no transaction; both answered by the deadline. With enough time left the second debit goes ahead and both are approved | UC-1 steps 5 and 9a, D-20 | Pass |
| S2-T418 | FR-5 intent before send | Failure injected between step 10 and the send: the fee read fails | Authorize | Row `operator_txs` `PLANNED` with the nonce and no hash; the authorization went `DEBIT_SUBMITTED` before anything was sent, then `TIMED_OUT`, answer `DECLINED / TIMEOUT` (UC-1 step 11); nothing on chain; UC-3 resolves the slot (T516, T518) | FR-5, ADR-10 | Pass |
| S2-T419 | Nonce allocation | Five sequential authorizations; a failed one in between | Read `operator_txs`, `operator_accounts` | Nonces `n … n+4` without a gap; `next_nonce` = `n+5`; the row is locked during the reservation (a concurrent reservation waits) | §3.2, ADR-10 | Pass |
| S2-T420 | FR-6 `validUntil` | Clock at `12:00:00.700` | Authorize; decode the calldata of the sent transaction | `validUntil` = `12:00:04` (floored to the second); `valid_until` stored; the contract reverts a debit after it (S1 proves the contract; here the value) | FR-6, ADR-12 | Pass |
| S2-T421 | Fees | Common; fake RPC with a known base fee and tip | Decode the sent transaction | Type 2; `maxPriorityFeePerGas` = the tip of `eth_maxPriorityFeePerGas`; `maxFeePerGas` = 2 × base fee + tip; `gasLimit` = `debit_gas_limit`; no `eth_estimateGas` call | ADR-10, §3.2 | Pass |
| S2-T422 | EC-4 reverted debit | Common; the chain mines on demand | Authorize; revoke the allowance before the block is mined; mine | `DECLINED / DEBIT_REVERTED`; `operator_txs` `REVERTED`; `debited_amount` 0 | EC-4, FR-8 | Pass |
| S2-T423 | EC-5 deadline | Common; mining stopped. Then: the final write slowed by 300 ms; once with mining stopped, once with the block mined 2.2 s in | Authorize | `DECLINED / TIMEOUT` at `deadline_at` ± 50 ms; row `TIMED_OUT`; continues in T512 / T513. With the slow write the answer is still on time and the row ends `TIMED_OUT`, never `APPROVED` after a `TIMEOUT` answer; the late debit stays on chain for UC-3 row 4 | EC-5, FR-1, D-21 | Pass |
| S2-T424 | EC-15 send result unknown | RPC times out on `eth_sendRawTransaction` but relays it | Authorize | Treated as sent; the inclusion signal of step 12 resolves it to `APPROVED` | EC-15 | Pass |
| S2-T425 | Internal error | Database failure injected at step 4; at step 10 | Authorize | `200 DECLINED / INTERNAL_ERROR` in both; nothing sent; the processor always gets a decision | §2.1.1 | Pass |
| S2-T426 | Decision latency and metrics | Twenty authorizations: approvals, declines of each reason, one timeout | Read `/metrics` | `auth_decision_seconds` histogram; `auth_decisions_total{decision,reason}` by outcome; p95 of the approvals on Anvil ≤ 2 s (A failure of the p95 is P2 locally) | §2.5.1, §3.2 | Pass |
| S2-T319 | EC-115 | `card_A` with an authorization blocked at step 12 | `UpdateCard`: lower the limit below the in-flight amount and freeze; release the authorization | The in-flight authorization keeps the values it read and is approved; the next one is declined `CARD_FROZEN` | EC-115, UC-103 | Pass |

### Phase 5 — Returns and tracker (st6)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T501 | Full reversal | T403 approved | `POST …/returns` without `amount` | `200 ACCEPTED`, `token_amount` `25400000`; tracker: `SUBMITTED → INCLUDED → CONFIRMED`; on chain `Refunded`; `returned_amount` = `refunded` of the contract | UC-2, FR-12, EC-8 | Pass |
| S2-T502 | Partial returns | 25.40 EUR approved as `29866387` | Return 10.00; then 15.40 | `11758420`; the second takes the whole remainder `18107967`; total = debited exactly | UC-2 step 5, FR-12 | Pass |
| S2-T503 | EC-17 above the remainder | T502 after the first return | Return 15.41 | `422 RETURN_EXCEEDS_DEBIT`; nothing stored | EC-17 | Pass |
| S2-T504 | EC-9 same `return_id` | T501 | Same request again; same `return_id` with another amount | Stored result, no second transfer; `409` | FR-13, EC-9 | Pass |
| S2-T505 | EC-16 decision pending | An authorization in `RECEIVED`; one in `DEBIT_SUBMITTED` | Return on each | `409 AUTHORIZATION_IN_PROGRESS`; nothing stored | EC-16 | Pass |
| S2-T506 | Nothing to return | Authorizations in `DECLINED`, `TIMED_OUT`, `LATE_DEBIT`, `LATE_DEBIT_REFUNDED`, `DEBIT_LOST` | Return on each | `200 NOTHING_TO_RETURN`, `token_amount` `0`; the return row is stored with that status; no transaction | UC-2 step 4 | Pass |
| S2-T507 | EC-14 tombstone | Common | Return for an unknown `auth_id`; then authorize with that `auth_id` | `NOTHING_TO_RETURN`; a tombstone row `DECLINED / REVERSED_BEFORE_AUTH` with null body fields; the authorization answers `DECLINED / REVERSED_BEFORE_AUTH` without a lock or a read | FR-15, EC-14 | Pass |
| S2-T508 | EC-12 treasury cannot fund | Treasury allowance 0 | Full return; raise the allowance after two attempts | `RETRYING`, `attempts` 1 then 2, 30 s apart; `returns_not_confirmed` 1; then `CONFIRMED` | EC-12, FR-14 | Pass |
| S2-T509 | Refund replayed on chain | T501 confirmed | Re-send the signed refund transaction with a new nonce through the operator queue, outside the API | Reverts `RefundAlreadyUsed`; no second transfer; the service row unchanged | FR-13 | Pass |
| S2-T510 | Finality on Anvil | T403 approved; `finality_confirmations` 10 | Mine 9 blocks; tracker tick; mine 1; tick | `APPROVED` after 9; `DEBIT_CONFIRMED` after 10; `operator_txs` `CONFIRMED` | FR-19, UC-3 row 1 | Pass |
| S2-T511 | EC-7 dropped debit, resubmitted | T403 approved, not final | `anvil_snapshot` before the debit, `evm_revert`, mine other blocks; tick | Receipt gone, `authorizations(authId)` 0 → a new debit with the same `authId`, new `validUntil`, same nonce slot if free; `APPROVED` stays; `replaced_hashes` grows; then `DEBIT_CONFIRMED` | UC-3 row 2, FR-18, EC-7 | Pass |
| S2-T512 | EC-7 resubmission fails | As T511; the allowance revoked after the revert | Tick | `DEBIT_LOST`; `debits_lost_total` 1; an open return of the authorization becomes `NOTHING_TO_RETURN` | UC-3 row 3, FR-18, FR-14 | Pass |
| S2-T513 | EC-6 late debit | T423 `TIMED_OUT` | Mine the debit at `validUntil − 1 s`; tick; let the return confirm | `LATE_DEBIT`; a return of type `LATE_DEBIT` with a generated `return_id`, full amount; `late_debits_total` 1; then `LATE_DEBIT_REFUNDED`; no manual action | UC-3 rows 4–5, FR-17, EC-6 | Pass |
| S2-T514 | Expired after timeout | T423 `TIMED_OUT` | Move the chain time past `validUntil`; mine; tick | The debit reverts `AuthExpired` or stays unmined with `authorizations(authId)` 0: `DECLINED / TIMEOUT`; `operator_txs` `REVERTED` or released (T516) | UC-3 row 6 | Pass |
| S2-T515 | Stuck before `validUntil` | Base fee raised above the sent fee cap; mining on | Tick | Replacement with the same nonce, both fee fields +25 %; old hash in `replaced_hashes`; `operator_tx_pending_seconds` grows then resets | UC-3 row 7, FR-20 | Pass |
| S2-T516 | Release after `validUntil` | Debit unmined past `validUntil` | Tick | A zero-value self-transfer with the same nonce; row `purpose` `RELEASE`, status `RELEASED`; the next authorization uses the next nonce and mines | UC-3 row 8, FR-20 | Pass |
| S2-T517 | FR-16 restart between send and receipt | Debit sent, process stopped before storing the outcome: the row stays `DEBIT_SUBMITTED` | Start a new tracker past `deadline_at`; tick | On-chain state read first, nothing sent at start. Row 11 first: `TIMED_OUT / TIMEOUT` (D-21). Then `debited > 0` → `LATE_DEBIT` with its automatic return (row 4), never `APPROVED`: the processor was told `TIMEOUT` or nothing; `debited = 0` and not expired → wait; expired → T514 | UC-3 rows 9 and 11, FR-16, EC-18, D-21 | Pass |
| S2-T518 | `PLANNED` slot never sent | T418 state: a `PLANNED` slot without a hash, the authorization `TIMED_OUT`; once with `validUntil` not reached, once passed; then the same after a restart, with the authorization left `DEBIT_SUBMITTED` | Tick; start and tick; authorize once more | In every case no debit is sent for the slot: a zero-value self-transfer of the operator fills its nonce at once, row `purpose` `RELEASE`, status `RELEASED` at inclusion; the authorization ends `DECLINED / TIMEOUT` (after `TIMED_OUT` by UC-3 row 11 when it was `DEBIT_SUBMITTED`); the next authorization takes the next nonce and mines; never two transactions for one nonce | UC-3 rows 9 and 11, FR-16, FR-20, D-22 | Pass |
| S2-T519 | Restart with an open return | Return `SUBMITTED`, process killed | Start; tick | `refundUsed(refundId)` read first; `true` → `INCLUDED`/`CONFIRMED` without a second send; `false` → resent | FR-16, FR-13 | Pass |
| S2-T520 | Reorg detection | T403 approved | Change the hash of the stored block by `evm_revert` and re-mining | The tracker notices the changed `block_hash` and re-reads the state before any decision | ADR-10 | Pass |
| S2-T521 | `GET /v1/authorizations/{auth_id}` | Authorizations of T502 and T507 | `GET` each; `GET` an unknown ID; as tenant B `GET` A's ID | Body of §2.1.4 with `returns` and `history`; the tombstone as §2.1.4 states; `404`; `404` | §2.1.4, FR-116, FR-105 | Pass |
| S2-T522 | Tracker metrics | Scenario of this phase | Read `/metrics` | `returns_not_confirmed`, `operator_tx_pending_seconds`, `operator_gas_balance`, `treasury_refund_capacity`, `late_debits_total`, `debits_lost_total` with the expected values | §2.5.1 | Pass |
| S2-T523 | Stuck debit replaced | T511 resubmission pending; base fee raised above its fee cap before its `validUntil` | Tick after `tracker_interval` | Replacement in the same nonce, both fee fields +25 % at least; the old hash in `replaced_hashes`; then it mines, `DEBIT_CONFIRMED`, one `Debited` | UC-3 row 7, FR-20, rules of S2 st6b | Pass |
| S2-T524 | Resubmission past `validUntil` | T511 resubmission kept unmined until past its `validUntil` | Tick until it mines | Released in its nonce (`RELEASE`, `RELEASED`); `authorizations(authId)` read as 0; resubmitted again in the next nonce with a new `validUntil`; mines. `APPROVED` throughout, then `DEBIT_CONFIRMED`; exactly one `Debited`; an alert per attempt; no `DEBIT_LOST`, `debits_lost_total` 0 | UC-3 rows 2, 3, 8, D-23 | Pass |
| S2-T525 | Nonce used outside `card-auth` | T423 `TIMED_OUT`, its debit pending | The operator key fills the same nonce outside `card-auth`; four ticks | One alert at Error level: the operator key was used outside `card-auth`; the row left as it is; next_nonce equal to the chain's count | Rules of S2 st6b | Pass |
| S2-T526 | Rate limit at a refund attempt | A return `ACCEPTED`; the endpoint answers HTTP 429 to the fee read of the send | Tick; then tick with the endpoint answering | One WARN line for the cycle, none for the refund; the return keeps its status and `attempts`; its slot stays `PLANNED` without a hash. The next cycles send it in that slot: `attempts` + 1, one slot | UC-2 step 7, UC-3 rules of S2 st9b, owner's answer of 2026-10-06 | Pass |
| S2-T527 | Tracker calls bounded by `rpc_read_timeout` | The endpoint does not answer `eth_call` for 3 s; `rpc_read_timeout` 500 ms | Start the tracker; again with the endpoint answering | The start fails on `treasury()` with `no answer within 500ms` in less than 1.5 s; then it succeeds | §3.1 `rpc_read_timeout`, owner's answer of 2026-10-06 | Pass |
| S2-T528 | Treasury address after a rate-limited start | One approval: the treasury holds 1 USDC; `eth_call` answered with HTTP 429 | Start the tracker; tick; let the endpoint answer; tick after the 60 s of the metric reads | The start ends on the rate limit; `treasury_refund_capacity` not reported (0) while limited; then reported, 1 000 000, without a restart | §2.5.1, UC-3 rules of S2 st9b, owner's decision of 2026-10-07 | Pass |

### Phase 6 — Chain listener (st7)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T601 | Subscription | Fake WebSocket server; `rpc_ws_url` set; `listener_subscription` `pendingLogs`, then `logs` | Start with each | `eth_subscribe` with the configured type and `{address: controller, topics: [Debited]}` once; `chain_listener_connected` 1; no subscription to `newHeads` or `newFlashblocks` | ADR-13, FR-23, decision 3 | Pass |
| S2-T602 | Signal from the subscription | T601; polling answers no receipt | Authorize; the fake server emits the `Debited` log of the `authId` | `APPROVED` on the log; `inclusion_signals_total{source="subscription"}` 1; the later receipt changes nothing | FR-2, FR-23 | Pass |
| S2-T603 | Signal from polling | T601; the fake server stays silent | Authorize; the chain mines | `APPROVED` on the receipt; `inclusion_signals_total{source="polling"}` 1; a log arriving later is ignored | FR-23 | Pass |
| S2-T604 | Log of another `authId` or contract | T601 | The fake emits logs with another `authId`, another address | Ignored; no state change | ADR-13 | Pass |
| S2-T605 | EC-19 listener down | T601 | Close the fake server; authorize | `chain_listener_connected` 0; the authorization is decided by polling; no error to the processor | EC-19, FR-23 | Pass |
| S2-T606 | Reconnect | T605 | Start the fake server again | Reconnect with backoff and jitter; resubscribe; `chain_listener_connected` 1; ping answered | ADR-13 common rules | Pass |
| S2-T607 | No `rpc_ws_url` | Variable unset | Start; authorize | Starts; metric 0; decisions by polling; one log line | §3.2 Reliability | Pass |
| S2-T608 | Nothing stored from a log | T602 | Compare the row with the receipt data | `tx_hash`, block and amount of the row come from the receipt read, not from the log payload; the log only signals | ADR-13 | Pass |
| S2-T609 | Chain ID of the WebSocket endpoint | Fake WebSocket server answers `eth_chainId` 84532; `chain_id` 31337; `pendingLogs`, then `logs` | Start; authorize; then the fake answers 31337 | `eth_chainId` before any `eth_subscribe`; on 84532 an Error log with `ALERT:`, the endpoint by its variable name and both chain IDs, no part of the URL; no `eth_subscribe`; `chain_listener_connected` 0; the service runs and the authorization is decided by polling; the listener retries with the backoff; on 31337 it subscribes once and the metric is 1 | §3.2 Reliability, ADR-13 | Pass |

### Phase 7 — Processor CLI and hard tests (st8)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T701 | CLI commands | `card-auth` running | `casctl sim authorize`, `return`, `get` with the Basic pair from `CASCTL_PROCESSOR_USERNAME` and `CASCTL_PROCESSOR_PASSWORD` | The three operations of the processor API; one JSON line per answer with the HTTP status; the password is never printed; no database connection is opened | §2.1.1 Processor simulator, PRD §4.2, package §2 | Pass |
| S2-T702 | Replay, same process | Common | CLI sends one `auth_id` 100 times, 10 in parallel | One `Debited`; 100 identical answers | Done-when 1, FR-3 | Pass |
| S2-T703 | Replay across a restart | Common | (a) Send; kill `card-auth` after the send and before the answer; start; send the same `auth_id` again, then once more. (b) Send and get `APPROVED`; kill; start; send the same `auth_id` again | (a) Every repeat answers `DECLINED / TIMEOUT`, never `APPROVED` (D-21); at most one `Debited`; a debit that landed ends `LATE_DEBIT_REFUNDED` without action. (b) The repeat answers the stored `APPROVED`; one `Debited` | Done-when 1, FR-16, D-21, UC-3 row 11 | Pass |
| S2-T704 | Concurrency | 10 cards, 5 `auth_id` per card, every request sent twice at once | Run | One `Debited` per `auth_id`; per card the debits are in sequence; no `409`; no duplicate nonce | Done-when 1, FR-10 | Pass |
| S2-T705 | Approve only after the signal | Mining delayed by 800 ms | Authorize while recording the time the block with the debit is mined (the earliest time a receipt exists) and the time of the response | The response is after the signal in every run of 20 | Done-when 2, FR-2 | Pass |
| S2-T706 | Missed deadline, late debit, refund | Mining delayed by 3 s | Authorize; wait | `DECLINED / TIMEOUT`; the debit lands; the return of type `LATE_DEBIT` confirms without any action; final `LATE_DEBIT_REFUNDED` | Done-when 3, FR-17 | Pass |
| S2-T707 | Full cycle on Anvil | Fresh deployment | With `cast`: mint and approve by the wallet, `setDailyLimit` by `ADMIN`, the refund allowance by the treasury, as the S1 scenario; authorize 25.40, return 10.00, finality | Balances as in the S1 scenario: wallet 84.60, treasury 15.40, controller 0; every row final | PRD §4.2, S1 scenario | Pass |
| S2-T708 | All metrics | One `card-auth` process after an approval, a return and their finality: metrics live in each process | Read `/metrics` | The ten `card-auth` metrics of §2.5.1 exist | §2.5.1 | Pass |
| S2-T709 | No signer in `server` | Built tree | Check the imports of every package under `cmd/server` and the packages it uses | The signer package of `card-auth` is not imported | ADR-3 | Pass |
| S2-T710 | Secrets | Database and logs after the phase | Search for the operator key, the Basic password, `DATABASE_URL` | Only the hash of the password in `api_credentials`; nothing else anywhere | §3.2 Security, handoff §4 | Pass |

### Phase 8 — Base Sepolia (st9)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T801 | Chain guard | Scripts of S2 | Run `Deploy.s.sol` with broadcast on Anvil 31337; on Anvil with chain ID 1; on Anvil with chain ID 84532, locally, no broadcast to a public network | Allowed; refused, no block mined; allowed | S1 summary §6, package 1.6 | Pass |
| S2-T802 | Deployment | Funded test accounts in the keys file (Deployment Guide §2) | Deploy `MockUSDC` and the controller on Base Sepolia | Addresses and transaction links recorded in the Deployment Guide and the test plan; roles granted; no personal wallet | Package §2 Deliverables, handoff §4 | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |
| S2-T803 | Start checks on the public chain | T802 | Start `card-auth` against Alchemy; against the fallback | Both pass T106; chain ID 84532 | Handoff §4 | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |
| S2-T804 | Provider test recorded | Owner's test of st1 | Read SRS §4 issue 1 | `pendingLogs` and preconfirmed receipts confirmed on the free plan, dated; or the fallback rule applied | Package 1.3 | Pass: [SRS — Card Spend](srs/card-spend.md) §4 issue 1; T807; §7 st9b |
| S2-T805 | End to end | T802; wallet funded and approved; limit set with `cast` | Authorize 5 USD; return 2 USD; wait for `finalized` | `APPROVED` with a `tx_hash` visible in the explorer; `CONFIRMED` return; `DEBIT_CONFIRMED` by the `finalized` tag (about 20 min); links recorded | Done-when, FR-19 | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |
| S2-T806 | p95 measurement | T805 | 30 USD authorizations in sequence from the development machine, one card, listener on; record provider, time of day, N, signals by source | p95 of `auth_decision_seconds` ≤ 2 s; the value and the conditions in the test plan and the session summary | Done-when 4, §3.2 | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |
| S2-T807 | Signals by source on the public chain | T806 | Read `inclusion_signals_total` | Both sources counted; subscription first in most runs when Flashblocks work | FR-23 | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |
| S2-T808 | Budget | After T806 | Read the Alchemy dashboard | CU of the run recorded; per authorization near the estimate of 400 CU | Package §3 | Pass: [Deployment Guide](deployment-guide.md) §12.4; CU per authorization not separable (§7) |
| S2-T809 | Deployment Guide | Fresh clone | Follow the guide from the start | Every step works as written; no step needs a key in a committed file | Package §2 Deliverables | Pass: [Deployment Guide](deployment-guide.md) §12, §7 st9b |

### Phase 9 — CI and repository (st2, st10)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S2-T901 | `go` workflow | Branch | Push | Role `cas_card_auth` created in each job; Foundry installed in each job and the chain tests run against Anvil; every package of `go list ./...` in exactly one job; green | Package st2 | Pass: push. Pull request and `main`: §7 |
| S2-T902 | `contracts` workflow on `main` | Decision 4 applied | Merge | The workflow runs on `main` when `contracts/**` changed | Decision 4 | Pass: trigger. `main`: §7 |
| S2-T903 | `proto` workflow with the new image | st3 commit | Push | Green against the new image | QA gate st3 | Pass |
| S2-T904 | Secret scan | Branch | Push | Green; `.env.example` holds names only; no address of an internal host | Handoff §4 | Pass |
| S2-T905 | Documents match the code | st10 | Compare SRS — Card Spend, SRS — Core, ADR-12, ADR-13, glossary, `docs/README.md` with the code | No mismatch | st10.2 | Pass |
| S2-T906 | No signer import rule in CI | Workflow | A throw-away branch imports the signer from `cmd/server` | The workflow `go` fails: T709 in its job `rest` | ADR-3 | Pass: local (§7) |

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

Batches run in the QA stage st10a in phase order; one line per batch and attempt. Cycle: fix → retest → regression of the earlier phases. Go tests: `go test -race -count=1 -p 1 -json ./...` with `-json` passed to `go test`, never through `GOFLAGS`.

| Date | Batch | Attempt | Pass | Fail | Failed IDs | Notes |
|---|---|---|---|---|---|---|
| 2026-10-06 | Phase 1 | 1 | 7 | 0 | — | Full run on `6ec1495`, clean tree, local, macOS arm64, Go 1.27.1, PostgreSQL 16.14, Anvil 1.8.3: 279 tests of C1 and S2, 0 failed, 0 skipped, 7 min 19 s |
| 2026-10-06 | Phase 2 | 1 | 8 | 0 | — | T201 – T204 in the full run; T205 – T208 commands, T207 and T208 on throw-away copies outside the repository (§7) |
| 2026-10-06 | Phase 3 | 1 | 19 | 0 | — | Full run |
| 2026-10-06 | Phase 4 | 1 | 27 | 0 | — | Full run. T426: p95 of the approvals on Anvil 100.9 ms |
| 2026-10-06 | Phase 5 | 1 | 25 | 0 | — | Full run |
| 2026-10-06 | Phase 6 | 1 | 9 | 0 | — | Full run |
| 2026-10-06 | Phase 7 | 1 | 10 | 0 | — | Full run |
| 2026-10-06 | Phase 8 | 1 | 9 | 0 | — | T801 in the full run; T802 – T809 st9b evidence of the owner, not rerun ([Deployment Guide](deployment-guide.md) §12) |
| 2026-10-06 | Phase 9 | 1 | 5 | 0 | — | Runs of the workflows on the pushes of the branch and local checks (§7). T905 Blocked: the documents were compared with the code in st10a part 2; the mismatches without a recorded decision wait for the owner (§7) |
| 2026-10-06 | Phase 9 | 2 | 6 | 0 | — | T905 after the owner's answers of 2026-10-06: every mismatch resolved in the documents or the code (§7) |
| 2026-10-06 | Stability | 1 | 77 | 1 | T703 | `-count=5` of `internal/listener`, `cmd/card-auth`, `internal/hardtest`, `internal/processorapi`: 78 rows. T703 (a) failed 4 of 5: defect of the test (§7). `internal/processorapi` stopped by the default timeout of `go test`, 10 min, after 233 passed tests and none failed: defect of the command |
| 2026-10-06 | Stability | 2 | 72 | 0 | — | `-count=5`: `internal/hardtest` with the fixed test, 45 of 45, 237 s; `internal/processorapi` with `-timeout 30m`, 400 of 400, 1028 s |
| 2026-10-06 | Phases 1 – 8 | 2 | 102 | 0 | — | Regression: full run after the fix of T703, local, macOS arm64, Go 1.27.1, PostgreSQL 16.14, Anvil 1.8.3: 279 tests, 0 failed, 7 min 11 s; T426 p95 92.1 ms |
| 2026-10-06 | Phases 1 – 8 | 3 | 104 | 0 | — | Regression after the code fixes of the owner's answers (T526, T527 new), local, macOS arm64, Go 1.27.1, PostgreSQL 16.14, Anvil 1.8.3: 281 tests, 0 failed, 7 min 20 s; T426 p95 95.8 ms. `-count=3` of the 35 tracker tests of `internal/processorapi` (T501 – T527, `TestTrackerBudget_*` and the tracker checks of §7): 105 of 105, 284 s; `internal/tracker` has no test files |
| 2026-10-07 | Phases 1 – 9 | 4 | 123 | 0 | — | Regression after the fix of T528 (owner's decision of 2026-10-07): full run, local, macOS arm64, Go 1.27.1, PostgreSQL 16.14, Anvil 1.8.3: 282 tests, 0 failed, 7 min 12 s, T426 p95 90.8 ms; `-count=3` of the tracker tests of `internal/processorapi`: 36 tests, 108 of 108, 284 s; the six make checks, the docs checker and gitleaks over the changed files green. Phase 8 rows: st9b evidence; phase 9 rows: unchanged |

## 6. Success criteria and sign-off

- Every row of §4 is `Pass`, locally and in CI; phase 8 rows by the owner on Base Sepolia.
- 0 open P0 and P1.
- OpenAPI and the proto equal their frozen forms; the generated code is current.
- `docs/srs/card-spend.md` and `docs/srs/core.md` match the code.
- Open P2 and P3 are listed in `known-issues.md` with their row IDs.

| Role | Name | Date | Result |
|---|---|---|---|
| Owner, QA | Igor Kudinov | 2026-10-07 | Pass: 123 rows, 0 open P0–P3. T901 and T902 complete with the runs on the pull request and on `main`, before the tag |

## 7. Notes of the QA stage

- **Rows and tests:** 105 rows are covered by 118 Go tests named by their row; T526, T527 and T528 added in st10a with the code fixes of the owner's answers. T503, T522 and T608 have no test of their own name: they are covered by the doc comment and a subtest of a test of another row (T503 in `TestT502_PartialReturns`; T522 in `TestT508_TreasuryCannotFund`, `TestT510_Finality`, `TestT512_ResubmissionFails`, `TestT513_LateDebit`, `TestT515_StuckReplaced`; T608 in `TestT602_T608_SignalFromTheSubscription`); not renamed, owner's decision of 2026-10-06. T205 – T208 and T901 – T906 are checks of the contract tooling, CI and the repository; T802 – T809 are evidence of st9b.
- **Tests without a row:** 31, each a check below a row or a rule of a stage: `casctl sim` against a fake API (5, `cmd/casctl`); the signer (4, `internal/signer`); `TestD16_ChainIDs`; `TestGasLimitsCoverMeasured`; the listener backoff and a log with `removed` true (`internal/listener`); in `internal/processorapi` the steps of st5a (`TestStep3_Tombstone`, `TestStep3_WaitersGetTheDecision`, `TestStep5_CardLockTimeout`, `TestPassPathReachesDebit`), two concurrent returns above the remainder, the FR-14 seam, UC-3 row 12, the listener on Anvil, the quiet shutdown of the queue and the tracker, `TestTrackerTouchesOnlyItsOwnRows`, the five `TestTrackerBudget_*`; the rehearsal of `scripts/sepolia/` (2, `internal/rehearsal`).
- **Regression of C1:** the 133 Go tests of C1 ran in both full runs and passed.
- **Stability, attempt 1, T703:** the check of the confirmed `LATE_DEBIT` return of T703 (a) counted the rows of `auth-703a` of every tenant. Each run of the test creates its own tenant on the one database of the phase, so with `-count=5` run *n* counted *n* returns: 2, 3, 4, 5. Each tenant held exactly one. Fixed: the query is scoped by the tenant of the world, as in T706. The single runs were not affected.
- **Stability, attempt 1, `internal/processorapi`:** five runs of 3.5 min exceed the default timeout of `go test`; attempt 2 with `-timeout 30m`.
- **Stability:** `go test -race -count=5` without a failure for `internal/listener` (35 s), `cmd/card-auth` (66 s), `internal/hardtest` (237 s) and `internal/processorapi` (1028 s): the packages with the chain listener, concurrency, timing and process restarts. No package was skipped; the longest single run is 3.5 min.
- **T426:** 20 authorizations, 12 of them approvals, on Anvil: p95 of the approvals 100.9 ms in the first full run, 92.1 ms in the regression, 89.6 – 95.3 ms in the five stability runs. The bound is 2 s.
- **T205:** `buf breaking --against` the frozen image of the tag `v0.2.0` passes.
- **T206:** the image of st3, `d7231ba`, is the one in `proto/frozen/cas_v1.json`; the images of C1 are in history; `make proto-check` green on the clean tree of `6ec1495`.
- **T207:** on a copy of `proto/`: a field of `GetCardRequest` removed → `buf breaking` exits 100; a field added → `buf breaking` passes and the image differs from `proto/frozen/cas_v1.json`.
- **T208:** on copies of `api/openapi/card-auth.yaml`: operation `getAuthorization` removed → `oasdiff breaking` exits 1; required `status` of `AuthorizeResponse` removed → `oasdiff breaking` exits 1 (`ERR`); optional `tx_hash` removed → `oasdiff` reports `INFO` and passes `--fail-on WARN`, the byte comparison fails; optional request field added → the byte comparison fails. `make openapi-check` exits 2 in each case. Expected reworded in st10a by the owner's decision; ID and status kept.
- **T808:** about 400 CU per authorization is the estimate of the package, never measured separately: during T806 the idle load of 36 CU/s masked it; the figure stays an estimate. Budget evidence, owner's decision of 2026-10-06: idle load after `6ec1495` 1.2 CU/s, 5-minute average, 10 minutes after the restart, 2026-10-06 19:05Z; throughput-limited 0 %; about 3M CU per month, about 10 % of the free plan of 30M; 36 CU/s before the fix.
- **T901:** the workflow `go` ran green on every push of the branch, the last on `6ec1495` as one job of 9.6 min: roles `cas_server` and `cas_card_auth` created in the job, Foundry `v1.8.3` installed, the chain tests on Anvil. In st10a the workflow is split into the parallel jobs `processorapi`, `hardtest` and `rest` with the same flags, triggers and path filters; `rest` asserts that the jobs together run every package of `go list ./...` once; actionlint v1.7.12 clean. The first run of the split workflow, the run on the pull request and the run on `main` come after this document is committed.
- **T902:** `contracts.yml` runs on pushes to `main` when `contracts/**` changed; the branch changes `contracts/script/` and `contracts/test/` since `v0.2.0`, so the merge starts it. Its run on `main` follows the merge.
- **T903:** the workflow `proto` ran green on `d7231ba`, which froze the new image, and on `d4a9996`, `ab5697c`, `8c426ee`.
- **T904:** the workflow `secrets` ran green on every push of the branch; `make secrets` green locally; `.env.example` holds names without values; the diff from `v0.2.0` holds no address other than `127.0.0.1` and `localhost` and no connection string with a real password.
- **T906:** proven locally on a copy of the tree: `cmd/server` importing `internal/signer` fails T709. CI runs T709 on every push in the workflow `go`; the package-union guard of the workflow (st10a) keeps `cmd/server` in a job. No throw-away branch, owner's decision of 2026-10-06.
- **T905:** SRS — Card Spend, SRS — Core, ADR-3, ADR-10, ADR-12, ADR-13, the glossary, `docs/README.md`, the C4 container diagram and the Deployment Guide compared with the code in st10a. Attempt 1 Blocked: the mismatches without a recorded decision went to the owner. Attempt 2 Pass, after the owner's answers of 2026-10-06: documents updated where the code is intended or kept as built (SRS — Card Spend 1.2, SRS — Core 1.3, ADR-3, ADR-12, glossary, C4 container, Deployment Guide 1.0, `docs/README.md`); code fixed where the owner chose the documented rule.
- **Code fixes of the owner's answers, st10a:** a rate limit at the fee read of a refund attempt is a rate limit of the cycle — one WARN line, the return keeps its status and `attempts`, the slot stays `PLANNED` (T526; before the fix: a second WARN line, `RETRYING`, `attempts` + 1, the retry delayed by `return_retry_interval`; `attempts` has no limit, it only counts). Each contract call of the tracker is bounded by `rpc_read_timeout` (T527; before the fix: no timeout, a hung `eth_call` held the cycle). The comment of `RegisterCardRequest.connection_id` names `DEGRADED` (D-12); the built image equals `proto/frozen/cas_v1.json`. The gitleaks allow-list of the example `auth_id` is limited to its four files. Both new tests failed on the code of `6ec1495` and pass with the fixes.
- **T528, owner's decision of 2026-10-07:** after a start whose `treasury()` read was rate-limited, the tracker kept no treasury address and `treasury_refund_capacity` stayed unreported until a restart. Now each metric update reads `treasury()` again while the address is unset, bounded by `rpc_read_timeout`, under the rate-limit rules of the cycle; refunds do not need the address. The test failed on the code before the fix (`treasury_refund_capacity = 0 after the endpoint answers, want 1000000`) and passes with it.
- **Rows closed in parts across stages:** T105, T203, T404, T412, T414, T425, T521, T522, T711; each is complete (table below).
- **Not covered by a test:** the CU per authorization on the public chain (T808, an estimate).
- **Manual checks:** phase 8 by the owner on Base Sepolia ([Deployment Guide](deployment-guide.md) §12).

- **Items of the draft, closed:**
  - Phase 8 by the owner, with the test accounts and test ETH of package §5: st9b, `6ec1495`; evidence in the row st9b below and the [Deployment Guide](deployment-guide.md) §12.
  - T107 with `operator_accounts`: st4, `d4a9996`.
  - T105: start and configuration dump in st2, `4c696fc`; one authorization and one failed send in st5b, `88f2b88`.
  - T709: st2, `4c696fc`.
  - T711 in phase 3: issue, list and revoke in st4, `d4a9996`; the `401` after revocation in st5a, `ab5697c`.
  - T319 (EC-115) in phase 4: st5b, `88f2b88`.
- Stage 5 is split by the owner's decision: st5a = UC-1 steps 1–9, every decline, the status query; st5b = steps 10–13 (operator queue, debit, deadline, approval). In st5a no transaction is sent: an authorization that passes every check reaches the Debit step, which the binary of st5a does not have; it declines as `INTERNAL_ERROR` and logs `debit path not built`. Tests of the pass path use a fake Debit step.

| Stage | Rows closed |
|---|---|
| st5a | T401, T402, T404 stored values, T405, T406, T407, T408, T409, T410, T411, T412 without the listener, T414 on a declined decision, T425 at step 4, T203 for the responses of st5a, T711 `401` after revocation; T521 in part: a declined authorization and every `404` |
| st5b | T403, T404 response, T413, T415 – T424, T425 at step 10, T426, T319, T105 rest, T203 for the responses of st5b. Closed on 2026-10-06; T426 p95 of the approvals on Anvil in §5 when the run log is written |
| st6a | T501, T502, T503, T504, T505, T506, T507, T508, T509, T519, T521 rest (returns and the tombstone), T522 for `returns_not_confirmed` and `treasury_refund_capacity`; also two concurrent partial returns above the remainder (exactly one accepted), the `500` of a return (D-18) and the FR-14 seam for `DEBIT_LOST` |
| st6b | T510, T511, T512 (`DEBIT_LOST` only for a revert before `validUntil`, D-23), T513, T514, T515, T516, T517, T518, T520, T523, T524, T525, T522 rest (`late_debits_total`, `debits_lost_total`, `operator_tx_pending_seconds`, `operator_gas_balance`); also UC-3 row 12. Closed on 2026-10-06; every scenario ends with next_nonce equal to the operator's count on chain, no `PLANNED` slot, no gap |
| st7 | T601, T602, T603, T604, T605, T606, T607, T608, T609, each of T601–T606, T608 and T609 for `pendingLogs` and `logs`; T412 rest: the listener never moves. Also: a log with `removed` true is ignored; one end-to-end run on the `logs` subscription of Anvil, decided by the subscription, the block filled later by the tracker; no part of the WebSocket URL path in the log of any listener test. Every scenario with a decision ends with next_nonce equal to the operator's count on chain, no `PLANNED` slot, no gap. The reconnect backoff is a constant of `internal/listener` (§3.2 Reliability). Follow-up of 2026-10-06: the tracker and the listener log no failure at shutdown (a cancelled context ends the cycle quietly); T105 passed 20 of 20 runs alone |
| st8 | T701, T702, T703 (a) and (b), T704, T705, T706, T707, T708, T709, T710. The hard tests are in `internal/hardtest`: the binaries of `card-auth` and `casctl` are built by the test and started as processes, each row on a fresh Anvil with its own operator key and tenant, on one test database for the phase; T701–T704 and T707 go through `casctl sim`, T705 and T706 send HTTP directly to time the answer. Settings of the runs: `receipt_poll_interval` 50 ms, `tracker_interval` 250 ms, `finality_confirmations` 2, `return_retry_interval` 2 s; no listener. T703 (a): mining held, `SIGKILL` once the debit is in the pool, one block after the start; every run ended `LATE_DEBIT_REFUNDED` with one `Debited`. T704: all 50 `auth_id` at once, each sent twice at once. T705: the time the block with the debit was mined is the earliest time of the receipt; the answers came 50–67 ms after it. T707: `mint` and `approve` are sent with `cast` by the wallet, `setDailyLimit` by `ADMIN`, the refund allowance by the treasury, as the S1 scenario does. T708: read on the process of its own row after an approval, a return and its finality: metrics live in each process. T709: in place since st2; it covers the signer package and the packages of `card-auth`, the chain listener added in st8. Also: unit tests of `casctl sim` against a fake API; the operator queue logs no send failure of a cancelled context. Every scenario ends with next_nonce equal to the operator's count on chain, no `PLANNED` slot, no gap. T702–T706 passed 3 of 3 |
| st9a | T801 in `internal/rehearsal` (`TestT801_DeployChainGuard`): `Deploy.s.sol` with broadcast deploys on Anvil 31337 and on Anvil with chain ID 84532, reverts on chain ID 1 with no block mined. Also: the scripts of `scripts/sepolia/` and the [Deployment Guide](deployment-guide.md); their rehearsal `TestRehearsal_BaseSepoliaScripts` on Anvil with chain ID 84532: deploy, setup twice (idempotent), register twice (resumes), `card-auth` without the listener, T805 in short (authorize 5 USD, return 2 USD, `DEBIT_CONFIRMED` and the return `CONFIRMED` by the tag `finalized`, not after 20 blocks), `measure.sh` with N 10; `TestRehearsal_Refusals`: every script refuses `CAS_SEPOLIA_KEYS` unset, a missing keys file, a keys file inside the repository, a keys file others can read, chain ID 1, a public URL with the rehearsal switch and a local URL without it. The rehearsal passed 2 of 2. No request left the machine in the tests. Rows T802 – T809 stay for st9b, run by the owner |
| st9b fix | The RPC budget of the tracker, owner's decision of 2026-10-06 after the live run on Base Sepolia (SRS — Card Spend UC-3, rules of S2 st9b): the final block read once per cycle and shared; no finality call for a row above it, debits and refunds; a row without a block reads its receipt only; the reorg check at most once per distinct block, rows in descending order of their block, and a real read right before a row is set final; a rate limit (HTTP 429, JSON-RPC `-32005` or `429`) ends the cycle with one WARN line; failures of one kind logged once per cycle with `rows`. Tests in `internal/processorapi` through the counting proxy, finality by the tag `finalized` on Anvil: `TestTrackerBudget_ApprovedRows` — a cycle with every row above the final block makes 5 calls (the final block, one block of the reorg check, the operator balance, the treasury balance and allowance) for 1 row and for 30, against 63 for 30 rows before the fix (2 per row + 3); the cycle that sets the 30 rows final makes 34 calls for 30 distinct blocks; `TestTrackerBudget_IncludedRefundBeforeFinal` — no `refundUsed` call for an INCLUDED refund above the final block, sealed or preconfirmed, then `CONFIRMED` and no `no contract code` line; `TestTrackerBudget_RateLimitEndsTheCycle` — the proxy answers 429 to everything: 1 request, one WARN line, no row moves, the next decision approves and the next cycle confirms; `TestTrackerBudget_SameFailureLoggedOnce` — 5 rows, one read of the final block, one line with `rows` 5. T510 – T525 pass. Every scenario ends with next_nonce equal to the operator's count on chain, no `PLANNED` slot, no gap. `deploy.sh`, `setup.sh`, `register.sh`: the RPC defaults to `CARD_AUTH_RPC_URL` of the environment file, else the public endpoint; the rehearsal runs `setup.sh` on it and refuses the public default in a rehearsal |
| st9b | T802, T803, T804, T805, T806, T807, T808, T809, run by the owner on Base Sepolia on 2026-10-06; addresses and transaction links in the [Deployment Guide](deployment-guide.md) §12. T802: `MockUSDC` `0x6c0434c821694513FFfd5364D63f27F05d73f3aB` (block 47768907, gas 544734), `CardSpendController` `0xF75D58dc6E33487dB994D81D0d870D61Eac45F37` (block 47768908, gas 860402), deployed 17:01:29Z through the public endpoint, `token()`, `treasury()`, `decimals()` and both roles checked; five generated accounts, about 0.0017 ETH from the CDP faucet. T803: start passed with Alchemy as the primary (`/readyz` 200, `chain_listener_connected` 1) and with `https://sepolia.base.org` as the primary and no listener (`t803-fb-1` approved by receipt polling). T804: the owner's provider test of st1 (SRS — Card Spend §4 issue 1, 2026-10-05: `pendingLogs` and preconfirmed receipts on the free plan), confirmed on the live run by T807: 11 decisions by the `pendingLogs` subscription. T805: 5 USD `APPROVED` at 17:16:28Z in 0.52 s; return 2 USD `CONFIRMED`; `DEBIT_CONFIRMED` by the tag `finalized` at 17:58:03Z, right after a restart of `card-auth`: the new process moved all 31 open authorizations to `DEBIT_CONFIRMED` in its first cycle (FR-16 on the public chain). T806: 17:20:58Z, Alchemy free plan, listener on (`pendingLogs`), 30 × 1.00 USD in sequence, one card, from the owner's development machine (Serbia): 30 approved; p95 of `auth_decision_seconds` 0.696 s (histogram); client-side p50 510 ms, p95 597 ms, max 729 ms; `PASS`, done-when 4. T807: signals by source: subscription 11, polling 19; receipt polling every 200 ms often sees the preconfirmed receipt first. T808: about 11K requests in 24 h, success 99.7 %, a rate-limited peak of about 1 % at 17:22Z from the tracker before the st9b fix; idle 36 CU/s before the metrics were bounded; CU per authorization could not be separated from the dashboard, no estimate. T809: the guide followed step by step; deviations fixed in it: the RPC default of the scripts, the 429 row, the order of the terminals (server, then `card-auth`). Findings of the run and their fixes: (1) `replacement transaction underpriced` on the load-balanced public endpoint, a stale nonce → the scripts default to `CARD_AUTH_RPC_URL`; (2) the tracker drew HTTP 429 with 63 calls per cycle for 30 open rows → the RPC budget of the cycle, 2 calls per cycle with every row above the final block (row st9b fix); (3) `refundUsed` at the final block before it reached the deployment of the controller, `no contract code` every cycle → no finality call for a row above the final block; (4) idle use of 36 CU/s, about 93M CU a month against the 30M of the free plan, from the chain reads of `operator_gas_balance` and `treasury_refund_capacity` on every cycle → at most once per 60 s, a constant of `internal/tracker` (rules of S2 st9b). `TestTrackerBudget_IdleMetrics`: with no open row, cycles every 2 s over 10 s of fake time make 3 calls (`eth_getBalance` 1, `eth_call` 2), against 18 before; none at 58 s; 3 again at 60 s. `TestT508_TreasuryCannotFund` reads `treasury_refund_capacity` one 60 s period later |

- In st5a, "approved" in T408 and T409 reads "passes the step and reaches the Debit step"; the approval itself is checked in st5b.
- Stage 9 is split by the owner's decision of 2026-10-06: st9a = the scripts, the Deployment Guide and their local rehearsal, without keys; st9b = the owner runs the scripts on Base Sepolia in the owner's own terminal, with the keys file of `CAS_SEPOLIA_KEYS`.
- Stage 6 is split by the owner's decision: st6a = UC-2 and the tracker loop that executes returns; st6b = the debit side of UC-3. In st6a, `CONFIRMED` of a return is read as `refundUsed(refundId)` at the final block of the finality rule (finality_confirmations blocks on top in mode `confirmations`, as T510 counts them).
