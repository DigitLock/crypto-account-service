# Test Plan — S3 EVM connector and reconciliation

- **Milestone:** S3 — EVM connector and reconciliation, version `v0.4.0`, branch `feature/v0.4.0`.
- **Objects under test:** the EVM connector of `server` (streams `balances` and `logs`, finality, reorg guard, start checks, balance checkpoint), the shared connector test suite, the treasury connection, the reconciliation worker and `GetReconciliationReport`, the `casctl` commands `source set`, `source set-treasury` and `reconcile`, the migrations of S3, the CI of S3.
- **Parents:** [SRS — EVM Connector](srs/evm-connector.md) UC-302, UC-303, UC-304, §2.1.1, §2.6, §3; [SRS — Card Spend](srs/card-spend.md) UC-4, §4 issue 4; [SRS — Core](srs/core.md) §2.1.1 (`GetReconciliationReport`, Connector contract), UC-102, UC-105 rows 8–10, §2.4, §3.2; [BRD](brd.md) BR-4, BR-12, G-4; ADR 2, 3, 5, 6, 11.
- **Decisions:** `S3 D-n`, register of the S3 discovery (2026-10-07). The bare `D-n` of SRS — Card Spend are decisions of S2.
- **Status:** Draft. Created in the discovery stage of S3 on 2026-10-07; completed stage by stage, run in st9.

## 1. Environment

| Item | Value |
|---|---|
| Go | `1.27.1`; go-ethereum `v1.17.7`; no new dependency without the owner |
| PostgreSQL | 16. `TEST_DATABASE_URL` owner role; `TEST_DATABASE_URL_SERVER` role `cas_server`; `TEST_DATABASE_URL_CARD_AUTH` role `cas_card_auth` |
| Local chain | Anvil of Foundry 1.8.3, chain ID 31337, started by the tests through `internal/testchain`; `MockUSDC` and the controller deployed by the tests. Reorg: `evm_snapshot`, `evm_revert`, `anvil_mine` |
| Fake JSON-RPC server | Serves the fixtures of `testdata/fixtures/evm/` in order; no network access (FR-316) |
| Fixtures | One JSON file per case: the ordered list of `{method, params, result \| error \| http_status}`; a batch takes one entry per element. Recorded from Anvil by a recording transport that stores no URL; provider errors written by hand; nothing from a public network (S3 D-13, S3 D-28) |
| Public chain | Base Sepolia, chain ID 84532: phase 8 only, by the owner, after the switch of Alchemy to pay as you go with a usage limit of $5 and an alert at $3 (package decision 1). Alchemy primary, `https://sepolia.base.org` fallback |
| Runs | `go test -race -count=1 -p 1 ./...`; in CI the workflow `go` in its parallel jobs, every package in exactly one job |
| Secrets | No key anywhere: the connector holds none. Chain test keys are generated at run time. Provider URLs come from the environment only and never appear in a log, an error, a fixture or a file of the repository |

## 2. Conventions

- Row format: `ID | Description | Preconditions | Steps | Expected | Req | Status`. A requirement without a document name is of SRS — EVM Connector.
- ID `S3-Tnnn`; the hundreds digit is the phase. A Go test names its row: `TestT412_RangeHalvedOnRejection`.
- `Status`: `—` not run, `Pass`, `Fail <attempt>`, `Blocked`.
- Severity: P0 a ledger entry changed or deleted by a sync, a duplicate entry, a transaction sent or a key read by `server`, a call to a chain outside the allow-list, a secret or a provider URL in a log, a fixture or the repository, an ID of another tenant in a report; P1 a requirement of the scope not met; P2 metric, CLI or message; P3 cosmetic.
- **Common** preconditions: migrated test database; Anvil with `MockUSDC` and the controller deployed; `sources.config` of `anvil` with `controller_address` and `backfill_floor` of that deployment; `EVM_RPC_URL_ANVIL` set to Anvil; `finality_mode` `confirmations`, `finality_confirmations` 10; tenant A with a service token and a wallet connection W on `anvil`; tenant `cas-platform` with the treasury connection T (`owner_ref` `treasury`, label `Treasury`) named in `treasury_connection` of `anvil`.
- **Tenant B:** a second partner tenant with its own token, wallet connection and card.
- "Final": blocks mined with `anvil_mine` until the block of the operation is at or below head − 10.

## 3. Phases

| Phase | Scope | Stage | Rows |
|---|---|---|---|
| 1 | Contract, configuration, migrations | st2 | 8 |
| 2 | Finality, reorg guard, start checks | st3 | 15 |
| 3 | Balance stream | st4 | 6 |
| 4 | Log stream, connector suite | st5 | 20 |
| 5 | Completeness and treasury | st6 | 9 |
| 6 | Reconciliation | st7 | 16 |
| 7 | Done-when on Anvil | st8 | 3 |
| 8 | Base Sepolia | st8 | 6 |
| 9 | CI and repository | st2, st9 | 5 |

## 4. Test matrix

### Phase 1 — Contract, configuration, migrations (st2)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T101 | `GetReconciliationReport` added to `cas.v1` | Frozen image of S2 | `buf lint`; `buf breaking` against the image of `v0.3.0`; `make proto-freeze` | Lint passes; no breaking change; new image committed; request `source`, `run_id`; response `run` with the fields and the seven mismatch types of Core §2.1.1 | Core §2.1.1; package decision 3; S3 D-10, S3 D-21 | — |
| S3-T102 | Generated code current | T101 | `make proto-check` on the committed tree | Passes | Core §2.1.1 | — |
| S3-T103 | `sources.config` and alias of both networks | Database migrated to the version of S3 | Read `config` of `anvil` and `base-sepolia`; read `asset_aliases` of `base-sepolia` | The values of §3.1 "Values per network": `base-sepolia` with `controller_address` `0xF75D…5F37`, `backfill_floor` 47768907, `finality_mode` `tag`, `finality_tag` `finalized`; `anvil` with `confirmations` 10; `chain_id` kept; a `treasury_connection` written before the migration kept; alias `0x6c0434c821694513FFfd5364D63f27F05d73f3aB` → `USDC`, decimals 6 | §3.1, §2.4; S3 D-17 | — |
| S3-T104 | Defaults | A source with `chain_id` only | Parse its configuration | `log_range_max` 2000, `rpc_rate_limit` 5 per second, `completeness_interval` 1 h, `sync_interval` `balances` 15 min and `logs` 5 min, `finality_confirmations` 10 | §3.1; S3 D-12 | — |
| S3-T105 | Endpoints from the environment, never printed | `EVM_RPC_URL_ANVIL` and the fallback set with a fake key in the path and the query | Start `server`; fail a run on each endpoint; read the log, the configuration dump, `last_error` of the stream and every error | The URL appears nowhere; the endpoint is named by its variable; host only where a host is logged | §3.2 Security; handoff §4 | — |
| S3-T106 | Migrations of S3 | Database at schema version 6 | Up; check tables and rights with `TEST_DATABASE_URL_SERVER`; down | `reconciliation_runs` with `source_id` and `to_block`; `balance_checkpoints` as Core §2.4; `cas_server`: `SELECT, INSERT` on `reconciliation_runs`, `SELECT, INSERT, UPDATE` on `balance_checkpoints`, `SELECT (block_number)` on `operator_txs`, nothing more; down restores version 6 | Core §2.4, §3.2; S3 D-5, S3 D-6, S3 D-7 | — |
| S3-T107 | Fixture recorder | Anvil | Record a case through the recording transport with a URL that carries a fake key | The file holds methods, parameters and answers only; no URL, no key; the fake server replays it | §2.6; S3 D-13 | — |
| S3-T108 | `casctl source set` | Migrated database | Set `controller_address`, `backfill_floor` and `token_address` of `anvil`; set `token_address` again with another address; then a key outside the list, a malformed address, a negative block, an unknown source, a source of kind `EXCHANGE` | `config` values written, other keys of `config` unchanged; the alias row `USDC`, decimals 6, in EIP-55 form, the second address replacing the first; old and new value printed; no audit row; every invalid case refused with a message, nothing changed | Core UC-105 row 8, EC-122; S3 D-17, S3 D-27 | — |

### Phase 2 — Finality, reorg guard, start checks (st3)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T201 | Final block, mode `tag` | Anvil (it answers `finalized`) or fixtures | Resolve the final block with `finality_tag` `finalized` | The number of the header of `finalized` | §2.1.1 Finality rule | — |
| S3-T202 | Final block, mode `confirmations` | Anvil | Resolve at a head below 10, then above | Below: no final block, the run reads nothing and does not fail; above: head − 10 | §2.1.1 Finality rule | — |
| S3-T203 | No log above F | Fixtures that record every request | One `logs` run with blocks beyond F | Every `eth_getLogs` range ends at or below F | FR-306 | — |
| S3-T204 | Changed hash below the cursor | W synced; the hash of block `next_block − 1` changed (fixtures, or T702 on Anvil) | Run `logs` twice | Each run fails with `REORG_BELOW_FINAL`; cursor, entries and checkpoints unchanged; `evm_reorg_below_final_total` +1 per hit; log line with both hashes | FR-312, EC-308 | — |
| S3-T205 | Missing block below the cursor | The block `next_block − 1` absent | Run `logs` | As T204 | FR-312, EC-308 | — |
| S3-T206 | First run without a guard | New connection, cursor without `last_hash` | Run `logs` | No header of `next_block − 1` is read; the run proceeds | UC-303 step 3 | — |
| S3-T207 | Start check: chain ID | An endpoint answering another chain ID | Run a stream of the network | Run fails before any other read; `evm_start_check_failed{check=chain_id}` 1; critical log line; the failure counts (backoff, `DEGRADED` after the threshold) | FR-305, FR-314, EC-305; S3 D-23 | — |
| S3-T208 | Start check: token | Controller whose `token()` has no row in `asset_aliases` | Run a stream | As T207 with `check=token` | FR-314; S3 D-23 | — |
| S3-T209 | Start check: treasury | (a) `treasury_address` ≠ `treasury()`; (b) `treasury_connection` unset | Run a stream of W | (a) as T207 with `check=treasury`; (b) one WARN line per start, the stream of W runs | FR-314; S3 D-23, S3 D-32 | — |
| S3-T210 | Checks repeated while failing, kept once passed | T207 failing, then the endpoint fixed | Run twice failing, once fixed, once more | The checks run before each failing run; after the first pass they are not repeated until `server` restarts; the metric returns to 0 | §2.1.1; S3 D-23 | — |
| S3-T211 | Chain ID on an endpoint change | Previous run on the primary | Next run on the fallback, the first on it since start | `eth_chainId` of the fallback is read first; another chain ID fails the run as T207; later runs on it do not repeat the check | UC-303 step 1; FR-305; S3 D-23 | — |
| S3-T212 | One endpoint per run, fallback and return | Primary failing once; two connections on the source | Three runs, alternating the connections | Run 1 fails on the primary; run 2 uses only the fallback; run 3 uses the primary; `evm_rpc_fallback_active` 1 after run 2, 0 after run 3; the choice holds across connections of the source | §2.1.1 Common rules, EC-313; S3 D-33 | — |
| S3-T213 | Rate limit | Primary answers HTTP 429, then JSON-RPC `-32005` | Run | `RateLimitError`; the budget of the primary is paused for `Retry-After` when given, else 60 s, the fallback budget is not; the next run uses the fallback; a call without an answer ends after 10 s | EC-313, §3.2 Reliability; Core EC-107; S3 D-34 | — |
| S3-T214 | Read-only, no signer; shared finality code | Code of S3 | `go list -deps` of `cmd/server`; search the connector for send and sign calls; run the tracker tests of S2 | No signer and no `card-auth` package in `server` (T709 extended to the new packages); no `eth_sendRawTransaction`, no key type in the connector; the S2 tracker tests pass with the final block read from `internal/chain` | FR-313, ADR-3; S3 D-4 | — |
| S3-T215 | Source without an RPC URL | `EVM_RPC_URL_ANVIL` unset | Start `server`; create a wallet connection on `anvil`; set the URL; restart | No streams declared, one WARN line naming the variable; the wallet connection is created; after the restart the connection gets its `balances` and `logs` cursors | §2.1.1, §3.1; Core FR-122; S3 D-16 | — |

### Phase 3 — Balance stream (st4)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T301 | Snapshot of every tracked token | W holds 12.5 USDC; a second wallet with 0 | Run `balances` for both | One row per tracked token, the zero included; account type `WALLET`; all in `free`; `locked` 0 | FR-303 | — |
| S3-T302 | One block, its time | Fixtures that record every request | Run `balances` | `balanceOf` pinned by the hash of the head block read in the same run; `as_of` of `GetBalances` = time of that block | FR-304, UC-302 | — |
| S3-T303 | No partial snapshot | A snapshot exists; the `eth_call` fails | Run `balances` | Run fails; no snapshot written; `GetBalances` returns the previous one, `stale` after `stale_after` | FR-303, EC-304; Core FR-109 | — |
| S3-T304 | Base units to decimals | Balance 29866387 base units, `decimals` 6 | Run `balances` | `free` = `29.866387`; no float in the path | §2.1.1 Amounts | — |
| S3-T305 | Interval and trigger | W created | Read the declared streams; call `TriggerSync` | `balances` every 15 min from `sources.config`; first run at creation; `TriggerSync` runs it | UC-302 trigger; Core FR-120 | — |
| S3-T306 | Limiter | Limiter with a counting budget | One `balances` run | Every request reserves cost 1 in the budget of its endpoint before it is sent | Core FR-110; §3.2 | — |

### Phase 4 — Log stream, connector suite (st5)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T401 | Transfers in and out | W receives 10 USDC and sends 3 USDC, final | Run `logs` | `DEPOSIT IN` 10 and `WITHDRAWAL OUT` 3, leg `SINGLE` | FR-308 | — |
| S3-T402 | Mint and burn | W mints 100 and burns 1, final | Run `logs` | `DEPOSIT` 100 from the zero address; `WITHDRAWAL` 1 to it | FR-308 | — |
| S3-T403 | Not imported | W sends to itself; a transfer of 0 to W | Run `logs` | No entry for either | FR-308 | — |
| S3-T404 | Debit | A debit of W, final | Run `logs` of W and of T | W: one `CARD_DEBIT OUT`; T: one `CARD_DEBIT IN`; the `Transfer` of the debit gives no entry in either | FR-307 | — |
| S3-T405 | Refund | A refund to W, final | Run `logs` of W and of T | W: one `CARD_REFUND IN`; T: one `CARD_REFUND OUT`; the paired `Transfer` gives none | FR-307 | — |
| S3-T406 | Several events in one transaction | Fixture: two `Debited` and two `Transfer` in one transaction | Run `logs` | Each event paired with the nearest unpaired `Transfer` of the same token, parties and amount; two entries | EC-310 | — |
| S3-T407 | Event without its transfer | Fixture: `Debited` without a matching `Transfer` | Run `logs` | The entry is created from the event; `evm_unmatched_controller_events_total` +1; WARN line | EC-311 | — |
| S3-T408 | Untracked token | A second ERC-20 without an alias moves tokens to W | Run `logs` | No request names that token; no entry | EC-312 | — |
| S3-T409 | Amount too large | Fixture: a mint of more than 20 integer digits after decimals | Run `logs` | The log is skipped; `evm_skipped_logs_total{reason}` +1; WARN line; the cursor moves past the range; other logs of the range imported | FR-309, EC-315 | — |
| S3-T410 | Entry fields | T401 | Read the entries with `ListLedgerEntries` and `raw` in the database | `external_id` = `tx_hash:log_index`; `group_id` = `logs:` + `external_id`; `occurred_at` = block time; `asset` `USDC`, `native_asset` the token address; `raw` = the log with `event` and `args` | FR-310, §2.4; Core FR-113 | — |
| S3-T411 | Repeated range | T401 synced | Reset the cursor to `backfill_floor`; run `logs` | No new entry; `ledger_duplicates_skipped_total` grows; `seq` unchanged | FR-310, EC-309; Core FR-106 | — |
| S3-T412 | Range halved | Fixtures: the provider rejects 2000 blocks, then 1000, accepts 500 | Run `logs` | The range is halved to 500 and repeated, never skipped; 500 kept for the network until restart; `evm_log_range_blocks` 500; down to one block when needed | FR-311, EC-306 | — |
| S3-T413 | Lagging node | Fixtures: logs empty, the header of the range end missing | Run `logs` | Run fails; the cursor does not move | EC-307 | — |
| S3-T414 | Backfill to incremental | Cursor at `backfill_floor`; history longer than one page | Run until the cursor reaches F | Pages of `log_range_max` blocks; mode `BACKFILL` until the first page ending at F, then `INCREMENTAL`; `SYNC_MAX_PAGES_PER_RUN` respected | UC-303 basic flow; Core UC-102 | — |
| S3-T415 | Stop in a backfill | T414 stopped after two pages | Restart; run | Continues from the committed cursor; no entry twice | EC-314; Core FR-108 | — |
| S3-T416 | Cursor | T401 | Read the `logs` cursor | `{next_block, last_hash, last_time}`: last processed block + 1, its hash and time; first run had none of the last two | §2.4 Cursor formats; S3 D-7 | — |
| S3-T417 | Cursors of existing wallets | A wallet connection created by the code of `v0.3.0`, without cursors | Start `server` of S3 | `balances` and `logs` cursors created; both due at once | Core FR-122, EC-119 | — |
| S3-T418 | Metrics of the import | T401 | Read `/metrics` | `evm_final_block`, `evm_indexer_lag_blocks`, `evm_rpc_requests_total{endpoint,method,result}` present with the values of the run | §2.5.1 | — |
| S3-T419 | Token tracked later | — | — | Not built in S3: SRS note and [backlog](backlog.md) item 15 checked | EC-316; S3 D-15 | — |
| S3-T420 | Shared connector test suite | `internal/connector/connectortest`; the EVM connector on the fake JSON-RPC server | Run the suite with no network | Idempotency, cursor resume and limit handling pass; the suite takes any connector and a fixture set | FR-316, ADR-2; S3 D-14 | — |

### Phase 5 — Completeness and treasury (st6)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T501 | When a checkpoint is returned | W `INCREMENTAL` | A run ending at F; another within `completeness_interval`; a `BACKFILL` page ending at F; a page ending below F | Only the first page carries `Page.Checkpoint`: block number, hash, time, one balance per tracked token | UC-304 steps 1–2; Core Connector contract; S3 D-3 | — |
| S3-T502 | Gap computed and stored | T401 final; checkpoint due | Run `logs` | In the transaction of the page: `balance_checkpoints` row of W per asset with block, balance, ledger total, gap 0; `ledger_gap{source,connection,asset}` 0 | FR-315; Core §2.4, §2.5; S3 D-5, S3 D-19 | — |
| S3-T503 | Gap found | A log skipped by T409, or `backfill_floor` set after a mint | Run `logs` to a checkpoint | Gap ≠ 0 stored and in the metric; the alert condition holds | FR-315 | — |
| S3-T504 | State not served | Fixtures: `eth_call` at F answers "missing trie node" | Run `logs` | Entries and cursor committed; no checkpoint stored; `evm_completeness_skipped_total` +1 | EC-317 | — |
| S3-T505 | Treasury connection created | Tenant `cas-platform` with a token | `CreateConnection` on `anvil` with the treasury address, `owner_ref` `treasury`, label `Treasury`; `casctl source set-treasury anvil <id>` | Connection `ACTIVE` with both cursors and audit `CONNECTION_CREATED`; `treasury_connection` holds its ID and `treasury_address` its address in EIP-55 form; old values printed | §2.4; Core UC-101, UC-105 row 9; S3 D-1, S3 D-2, S3 D-22, S3 D-32 | — |
| S3-T506 | `set-treasury` refusals | Migrated database | Unknown source; unknown connection; a connection of another source; an exchange connection | Refused with a message; `config` unchanged | Core EC-122 | — |
| S3-T507 | Treasury sees every event | Debits and refunds of W and of a wallet of tenant B, final | Run `logs` of T | One `CARD_DEBIT IN` per `Debited` and one `CARD_REFUND OUT` per `Refunded` of both wallets; deposits and withdrawals of the treasury as `DEPOSIT`, `WITHDRAWAL` | §2.1.1 Roles, §2.1.2 filters | — |
| S3-T508 | Zero gap, wallet and treasury | Mint, transfer in, transfer out, debit, refund, final | Run both connections to a checkpoint | `gap` 0 for W and T | FR-315 | — |
| S3-T509 | Checkpoint of the treasury readable by reconciliation | T508 | Read `balance_checkpoints` of T with `TEST_DATABASE_URL_SERVER` | Row present with the block of the treasury's `logs` cursor or below | Card Spend UC-4 rule 5; S3 D-5 | — |

### Phase 6 — Reconciliation (st7)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T601 | Clean run | Authorizations and returns of tenant A by `card-auth` on Anvil, final; T synced | Run reconciliation of `anvil` | One run for A, one for `cas-platform`; both without mismatch; totals: authorizations checked, debits count and sum, returns checked, refunds count and sum | Card Spend UC-4 basic flow | — |
| S3-T602 | `MISSING_DEBIT` | An `APPROVED` authorization of A with `valid_until` ≤ `period_to` and no `Debited` (row written by the test) | Run | A's run: `MISSING_DEBIT` with `auth_id`, `chain_auth_id`, `expected_amount` | Card Spend UC-4 rule 1, FR-21 | — |
| S3-T603 | `AMOUNT_MISMATCH` of a debit | `token_amount` of an authorization ≠ the amount of its event | Run | A's run: `AMOUNT_MISMATCH` with `expected_amount` = `token_amount`, `actual_amount` = event amount, `tx_hash` | Card Spend UC-4 rule 1, FR-21; S3 D-21 | — |
| S3-T604 | `UNKNOWN_DEBIT` | A debit sent on Anvil with the operator key and an `authId` of no tenant, final | Run | `cas-platform` run: `UNKNOWN_DEBIT` with `chain_auth_id`, `tx_hash`, `actual_amount`; no `auth_id`; `reconciliation_mismatches{source,type=UNKNOWN_DEBIT}` 1 | Card Spend UC-4 rule 2, FR-21, FR-22, §4 issue 4 | — |
| S3-T605 | `MISSING_REFUND` | A `CONFIRMED` return of A whose `REFUND` block ≤ `to_block`, without `Refunded` | Run | A's run: `MISSING_REFUND` with `return_id`, `chain_refund_id` | Card Spend UC-4 rule 3, FR-21 | — |
| S3-T606 | `AMOUNT_MISMATCH` of a refund | `token_amount` of a return ≠ the amount of its event | Run | A's run: `AMOUNT_MISMATCH` with `return_id`, both amounts | Card Spend UC-4 rule 3, FR-21 | — |
| S3-T607 | `UNKNOWN_REFUND` | A refund with a `refundId` of no return (operator key on Anvil), final | Run | `cas-platform` run: `UNKNOWN_REFUND`; metric 1 | Card Spend UC-4 rule 4, FR-22 | — |
| S3-T608 | `TREASURY_MISMATCH` | `balance_checkpoints` of T with gap ≠ 0 | Run | `cas-platform` run: `TREASURY_MISMATCH` with `asset`, `checkpoint_balance`, `ledger_total` in asset decimals | Card Spend UC-4 rule 5; S3 D-24 | — |
| S3-T609 | Boundary | An authorization with `valid_until` > `period_to`; a `CONFIRMED` return whose block > `to_block`; a return without a block | Run; sync T further; run again | First run skips all three; the later run checks those now within the boundary | Card Spend UC-4 eligibility; S3 D-7 | — |
| S3-T610 | Tenant scope | Tenants A and B with authorizations; T602 for B | Run; `GetReconciliationReport` with A's token for A's run and for B's `run_id` | A sees its run only; B's `run_id` → `NOT_FOUND`; the `cas-platform` run holds no `auth_id` or `return_id`; a tenant without authorizations on the chain has no run | Card Spend UC-4 runs, Core FR-105; S3 D-6 | — |
| S3-T611 | Period and storage | T601 | Read `reconciliation_runs` | One row per tenant and source with `period_from` (oldest authorization checked; oldest event for `cas-platform`; `period_to` when nothing checked), `period_to` = `last_time`, `to_block` = `next_block − 1` of T's cursor, totals, mismatches | Card Spend UC-4, FR-21; Core §2.4; S3 D-7, S3 D-26 | — |
| S3-T612 | `GetReconciliationReport` | Runs exist | Empty `source`; unknown `source`; malformed `run_id`; empty `run_id`; a `run_id` of another source; no run yet | `INVALID_ARGUMENT`; `NOT_FOUND`; `INVALID_ARGUMENT`; newest run of the caller for the source; `NOT_FOUND`; `NOT_FOUND` | Core §2.1.1; S3 D-10, S3 D-24 | — |
| S3-T613 | Worker trigger | `server` running with the engine lock | T's `logs` cursor unchanged for two ticks, then moved | No run while unchanged; one run per tenant after the move; a standby instance without the lock runs none | Card Spend UC-4 trigger; Core §3.2; S3 D-8 | — |
| S3-T614 | Gauge | T604 | Run the source again with nothing new; read `/metrics` after each run | `reconciliation_mismatches{source,type}` = count of the newest runs; a persisting mismatch keeps the value, it does not grow per run | Card Spend §2.5.1; S3 D-9 | — |
| S3-T615 | `UNEXPECTED_DEBIT` | An authorization of A set `DECLINED` (and one `DEBIT_LOST`) by the test while its `Debited` exists | Run | A's run: `UNEXPECTED_DEBIT` with `auth_id`, `tx_hash`, `actual_amount`; not in the `cas-platform` run | Card Spend UC-4 rule 2a; S3 D-21, S3 D-25 | — |
| S3-T616 | `casctl reconcile` | T601; then `treasury_connection` of a source unset | `casctl reconcile anvil`; `casctl reconcile` on the source without a treasury | One line per run: tenant, run ID, mismatches by type; exit 0, mismatches included; the second refused, no run stored | Core UC-105 row 10, EC-123; S3 D-8, S3 D-24 | — |

### Phase 7 — Done-when on Anvil (st8)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T701 | Scripted scenario | Common; `server` and `card-auth` as binaries on the test database | Mint to W; transfer in; transfer out; authorize (debit); return (refund); mine to finality; wait for the runs | The expected entries of W and T, each once; gap 0 for W and T | FR-317, FR-307, FR-308, FR-315; done-when 1 | — |
| S3-T702 | Reorg below the cursor | T701 synced; a state saved before the cursor | `evm_revert` to it; mine other blocks past the cursor; run `logs` of W and T | Both streams fail with `REORG_BELOW_FINAL`; no entry, cursor or checkpoint changed; metric and critical log line | FR-312; done-when 2 | — |
| S3-T703 | Reconciliation of the scenario | T701 | Run reconciliation; send a debit with an unknown `authId` with the operator key; mine to finality; run again | First runs empty; then the `cas-platform` run reports `UNKNOWN_DEBIT` and the gauge is 1 | FR-21, FR-22; done-when 3 | — |

### Phase 8 — Base Sepolia (st8)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T801 | Backfill of the S2 history | Alchemy on pay as you go, limit $5, alert $3; `server` of S3 against `cas_dev`; T of `cas-platform` on `base-sepolia`; the wallet connection of `USER` (tenant `sepolia-demo`) | Run until both reach F | Entries of `USER` and the treasury from block 47768907: setup transfers, every debit and the refund of S2; the count of debits equals the count on the explorer (discovery I-10: 32 expected) | FR-307, FR-308 | — |
| S3-T802 | Gap on the public chain | T801 | Wait for a checkpoint | Gap 0 for both; or EC-317 counted and the state limit recorded | FR-315; §4 issue 2 | — |
| S3-T803 | Finality distance | T801 running | Read head and `finalized` at intervals over at least one hour | Distance in blocks and minutes, dated, recorded in §4 issue 1 and the Deployment Guide | §4 issue 1 | — |
| S3-T804 | Provider limits | T801 | Read `evm_log_range_blocks` and the errors of the backfill; fallback range | Range and answer limits of Alchemy and of the fallback recorded; `log_range_max` and `rpc_rate_limit` of `base-sepolia` set | §4 issue 6, EC-306; S3 D-12 | — |
| S3-T805 | Reconciliation of the S2 history | T801 | Run reconciliation of `base-sepolia` | Runs of `sepolia-demo` and `cas-platform` without mismatch, or every mismatch explained in §7 | Card Spend UC-4 | — |
| S3-T806 | Cost | After T801 – T805 | Read the Alchemy dashboard | CU of the backfill and of one hour of idle sync recorded with the price shown in the dashboard; within the $5 limit | Package decision 1 | — |

### Phase 9 — CI and repository (st2, st9)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| S3-T901 | Secret scan of fixtures | Branch | Push; `make secrets` | Green; `testdata/` holds no key, no provider URL, no real account; `.env.example` holds the names of §3.1 without values | Handoff §4 | — |
| S3-T902 | `go` workflow | Branch | Push | The new packages run in exactly one job; Anvil available in the jobs that need it; every job within 12 min | Handoff §3.4 | — |
| S3-T903 | `proto` workflow with the new image | st2 commit | Push | Green against the new image | QA gate st2 | — |
| S3-T904 | Documents match the code | st9 | Compare SRS — EVM Connector, SRS — Card Spend UC-4, SRS — Core, glossary, `docs/README.md` with the code | No mismatch | Package 9.2 | — |
| S3-T905 | Tag guard by hand | Pull request merged | `git fetch`; HEAD = `origin/main`; `git merge-base --is-ancestor <last commit of the branch> HEAD`; `gh run watch --exit-status` for each run of the merge commit; then `git tag -a v0.4.0` | Every step passes before the tag; the tag is on the merge commit | Handoff §3.4; package decision 4 | — |

### Coverage of the requirements

| FR | Rows | FR | Rows |
|---|---|---|---|
| FR-303 | T301, T303 | FR-312 | T204, T205, T702 |
| FR-304 | T302 | FR-313 | T214 |
| FR-305 | T207, T211 | FR-314 | T207, T208, T209 |
| FR-306 | T203 | FR-315 | T502, T503, T508, T701, T802 |
| FR-307 | T404, T405, T701, T801 | FR-316 | T420 |
| FR-308 | T401, T402, T403, T701, T801 | FR-317 | T701 |
| FR-309 | T409 | FR-21 | T602, T603, T605, T606, T611, T703 |
| FR-310 | T410, T411 | FR-22 | T604, T607, T703 |
| FR-311 | T412 | Core FR-122 | T215, T417 |

| EC | Rows | EC | Rows |
|---|---|---|---|
| EC-304 | T303 | EC-312 | T408 |
| EC-305 | T207 | EC-313 | T212, T213 |
| EC-306 | T412, T804 | EC-314 | T415 |
| EC-307 | T413 | EC-315 | T409 |
| EC-308 | T204, T205 | EC-316 | T419 |
| EC-309 | T411 | EC-317 | T504, T802 |
| EC-310 | T406 | EC-122 | T108, T506 |
| EC-311 | T407 | EC-123 | T616 |

### Coverage of the exit criteria

| Criterion | Rows |
|---|---|
| Local scenario of mint, transfers, debit and refund: expected entries, zero gap for the wallet and the treasury | T701, T508 |
| A reorg below the cursor stops the stream and changes nothing | T702, T204, T205 |
| Reconciliation of a clean run is empty; an injected unknown debit is reported | T703, T601, T604 |

## 5. Run log

Batches run in the QA stage st9 in phase order; one line per batch and attempt. Cycle: fix → retest → regression of the earlier phases. Go tests: `go test -race -count=1 -p 1 -json ./...` with `-json` passed to `go test`, never through `GOFLAGS`.

| Date | Batch | Attempt | Pass | Fail | Failed IDs | Notes |
|---|---|---|---|---|---|---|

## 6. Success criteria and sign-off

- Every row of §4 is `Pass`, locally and in CI; phase 8 rows by the owner on Base Sepolia.
- 0 open P0 and P1.
- The proto equals its frozen image; the generated code is current.
- SRS — EVM Connector, SRS — Card Spend UC-4 and SRS — Core match the code.
- Open P2 and P3 are listed in `known-issues.md` with their row IDs.

| Role | Name | Date | Result |
|---|---|---|---|
| Owner, QA | Igor Kudinov | — | — |

## 7. Notes of the QA stage

Filled in st9.
