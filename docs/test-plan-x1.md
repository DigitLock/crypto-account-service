# Test Plan — X1 Binance balances

- **Milestone:** X1 — Binance balances, version `v0.5.0`, branch `feature/v0.5.0`.
- **Objects under test:** the Binance connector of `server` (signer, HTTP client, time offset, limiter budgets, key check and its periodic repeat, balance snapshot with the wrapper rule), the HTTP fixture format and recorder, the shared connector test suite extended for an exchange, the migration of the `binance` source and its alias rows, the script `scripts/binance/connect.sh`, the metrics of X1, the CI of X1.
- **Parents:** [SRS — Binance](srs/exchange-binance.md) §2.1, UC-201, UC-202, §2.4, §2.5, §2.6, §3, §4 issues 2 and 6; [SRS — Core](srs/core.md) Connector contract, UC-101, UC-102, UC-104, §2.1.3, §3.1; [PRD — Exchange Accounts](prd/exchange-accounts.md) US-201 … US-205, US-210 … US-212; [BRD](brd.md) BR-1, BR-2, BR-3, G-3, G-5; ADR 2, 3, 4, 6.
- **Decisions:** `X1 D-n` and `X1 P-n`, register of the X1 discovery (2026-10-09). `P-1` … `P-9` are the decisions of the milestone package.
- **Status:** Draft. Created in the discovery stage of X1 on 2026-10-09.

## 1. Environment

| Item | Value |
|---|---|
| Go | `1.27.1`; no new dependency without the owner |
| PostgreSQL | 16. `TEST_DATABASE_URL` owner role; `TEST_DATABASE_URL_SERVER` role `cas_server` |
| Fake Binance server | Serves the fixtures of `testdata/fixtures/binance/` in order, with their status and headers, and counts the weight of every request; no network access (FR-216) |
| Fixtures | One JSON file per case: the ordered list of `{method, path, query, http_status, headers, body}`; `query` without `timestamp` and `signature`; `headers` only the used-weight headers and `Retry-After` (X1 D-13). `/api` answers recorded from the test network with `uid` replaced by a fictitious value; `/sapi` answers written by hand from the examples of the Binance documentation, fictitious values; nothing from the real account (X1 P-6) |
| Test network | `https://testnet.binance.vision`, `/api` only. Phases 1 and 4, live rows only, by the owner: `BINANCE_TESTNET_API_KEY` and `BINANCE_TESTNET_API_SECRET` in the ignored `.env`; the live tests are skipped without them. No source row and no connection for the test network (X1 D-20) |
| Real account | Phase 6 only, by the owner: `BINANCE_API_KEY` and `BINANCE_API_SECRET` in the ignored `.env`, created right before st7, reading only; `cas_dev`, tenant `x1-real`, `owner_ref` `owner-x1`, label `Binance` (X1 D-18). Evidence by option B (P-1): date, verdict, counts of assets per account type; no amount, no asset name, no account identifier. The output with amounts stays in the owner's terminal |
| Runs | `go test -race -count=1 -p 1 ./...`; in CI the workflow `go` in its parallel jobs, every package in exactly one job |
| Secrets | No key, secret, signature or real `uid` in a log line, an error, `last_error`, a fixture, a stage report, a prompt or a file of the repository. Test keys of the fixtures are fictitious markers |

## 2. Conventions

- Row format: `ID | Description | Preconditions | Steps | Expected | Req | Status`. A requirement without a document name is of SRS — Binance.
- ID `X1-Tnnn`; the hundreds digit is the phase. A Go test carries the tag `// X1-Tnnn — Req: …`.
- `Status`: `—` not run, `Pass`, `Fail <attempt>`, `Blocked`.
- Severity: P0 a key, secret, signature, real account identifier or real amount in a log, a fixture, a report or the repository; a key with a permission beyond reading accepted; a request sent while its budget is paused; a call that needs a trade, withdrawal or transfer permission. P1 a requirement of the scope not met. P2 metric, script or message. P3 cosmetic.
- **Common** preconditions: migrated test database with the `binance` source; `base_url` of the source pointing at the fake Binance server; tenant A with a service token; a fictitious read-only key K whose fixtures answer `enableReading` `true` and nothing else.
- "Weight" of a request: the weight of SRS — Binance §2.1.2.

## 3. Phases

| Phase | Scope | Stage | Rows |
|---|---|---|---|
| 1 | Source, transport, fixtures | st2 | 11 |
| 2 | Limiter budgets | st3 | 10 |
| 3 | Key check | st4 | 12 |
| 4 | Balance snapshot | st5 | 16 |
| 5 | Connector suite, script, done-when on fixtures | st6 | 6 |
| 6 | Real account | st7 | 8 |
| 7 | CI and repository | st2, st8 | 5 |

## 4. Test matrix

### Phase 1 — Source, transport, fixtures (st2)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T101 | Migration of the `binance` source | Database at schema version 9 | Up; read `sources` and `asset_aliases` of `binance`; down | Row `binance`, kind `EXCHANGE`, enabled, `config` with the values of §3.1; identity alias rows of the base and quote assets of the symbols of `exchangeInfo` at release; no row for a wrapper code; down restores version 9; the assertion of `migrations/schema_test.go` that `binance` has no row is replaced | §2.4 seed data, §3.1; X1 D-5 | — |
| X1-T102 | Configuration and defaults | A `binance` row with `config` complete; with values missing; with a malformed value | Parse the configuration | Values as stored; a missing or malformed value takes its default of §3.1 | §3.1; Core §3.1 | — |
| X1-T103 | HMAC-SHA256 signature | The HMAC example of the Binance documentation | Sign its query with its secret | The signature equals the documented one; `signature` is the last parameter; parameters of a `POST` go in the query | §2.1.1 Signature; X1 D-4 | — |
| X1-T104 | Time offset | Fake server whose `/api/v3/time` is 1500 ms ahead | A signed request; another after `time_sync_interval`; read `/metrics` | The first signed request reads the time first (weight 1 on the `/api` budget); `timestamp` carries the offset; the time is read again after `time_sync_interval`, not before; `binance_time_offset_ms` = −1500 | §2.1.1 Time, §2.5.1; X1 D-8 | — |
| X1-T105 | Timestamp outside `recvWindow` | Fake server answers `-1021` once, then accepts; then `-1021` twice | Two signed requests | First: the offset is read again and the request repeated once, success. Second: one repeat, then a plain failure | EC-221; X1 D-3, X1 D-8 | — |
| X1-T106 | Answers to errors | Fake server answers in turn: `401`; `-2015`; `-2014`; `-1022`; `500`; `-1001`; `-1008`; a closed connection; no answer within 10 s; `403`; an unknown code | One request per answer | Key rejected for the first four; unreachable for the next five; `403` unreachable with one WARN line and no pause; the unknown code a plain failure | §2.1.1; X1 D-3 | — |
| X1-T107 | Secrets never printed | Key and secret with marker values; log at `debug` | Fail a request of each kind of T106; fail a run of the engine on each answer of Binance of T106 (in st5, where the connector runs in the engine: X1 D-24); the closed connection and the 10 s timeout at the level of the connector only (X1 D-38) | The markers and any signature appear in no log line, error, `last_error` or file; a signed URL in a log line has no `signature` | §3.2 Security; Core Connector contract Secrets; FR-217; X1 D-24, X1 D-38 | — |
| X1-T108 | Amounts as decimals | Answers with `0.00100000`, `0.00000000`, `12.50000000` | Parse | `0.001`, `0`, `12.5`; no float on the path | §2.1.1 Amounts; Core Connector contract | — |
| X1-T109 | Fixture format and fake server | A fixture file of three calls | Replay; send a fourth request | The answers come in order with their status and headers; the fourth request fails the test; the weight of each request is counted per budget; no network access | §2.6; X1 D-13 | — |
| X1-T110 | Recorder | Recording through a fake upstream with a marker key in `X-MBX-APIKEY` and a host in the URL | Record a signed `account` call | The file holds method, path, query without `timestamp` and `signature`, status, the used-weight headers and the body; no host, no key, no signature; `uid` replaced by the fictitious value | FR-217, §2.6; X1 D-13, X1 P-6 | — |
| X1-T111 | Signed call on the test network | Owner; test-network key in `.env` | Run the live test of `account` | `200`; a `uid` present (not printed); the time offset read; the live test skipped when the variables are unset | Package st2 exit; X1 D-20 | — |

### Phase 2 — Limiter budgets (st3)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T201 | Budgets | `budget_share` 0.5 | Read `Budgets` | One `/api` budget of 3000 per minute; one budget per `/sapi` endpoint of X1 (`apiRestrictions`, `get-funding-asset`, `flexible/position`, `locked/position`) of 6000 per minute; no budget per account | §2.1.1 Rate limits; X1 D-6 | — |
| X1-T202 | Reservation before every request | Fake server counting requests; recording limiter | One request of each endpoint row of §2.1.2; a session without a limiter | Each request reserves the weight of its row in its budget before it is sent, the time call included; no request without a limiter. The sequences of the key check (1 + 20) and of the snapshot (20 + 1 + 150 per Earn page) are checked by T301 and T405 | Core FR-110; §3.2 Performance; X1 D-29 | — |
| X1-T203 | Used weight of `/api` from the header | Answers carry `X-MBX-USED-WEIGHT-1M` above the weight reserved in the window | Requests until the header value plus the next cost exceeds the budget | The budget counts at least the header value (`Observe`); the next reservation waits for the next window | FR-213; X1 D-7 | — |
| X1-T204 | Used weight of a `/sapi` endpoint | Answers of `flexible/position` carry `X-SAPI-USED-IP-WEIGHT-1M` | Paged reads | Only the budget of that endpoint takes the header value | FR-213; X1 D-7 | — |
| X1-T205 | Share never exceeded | Burst of signed calls of several sessions on one limiter | Count the weight per window at the fake server; read the metrics registry (`/metrics` of `server` from st4) | No window above the share of any budget; `binance_used_weight{budget}` = last header value | FR-213, §2.5.1; X1 D-16 | — |
| X1-T206 | `429` with `Retry-After` | `/api` answers `429`, `Retry-After: 30` | A signed call; then requests of both families | `RateLimitError` with the `/api` budget and 30 s; the budget paused; no `/api` request before 30 s; `/sapi` budgets go on | FR-214, EC-205; Core EC-107 | — |
| X1-T207 | `418` with `Retry-After` | `/api` answers `418`, `Retry-After: 120` | As T206 | As T206 with 120 s; `/sapi` answers only `429` (§2.1.1 Rate limits), so no `418` case for `/sapi` | FR-214 | — |
| X1-T208 | `429` without `Retry-After` | `/sapi` endpoint answers `429` without the header | A call of that endpoint | The budget of that endpoint paused for 60 s | FR-214; X1 D-3 | — |
| X1-T209 | Limit metrics | T206, T208 | Read the metrics registry; in st5 a run of the engine | `binance_rate_limit_responses_total{code}` +1 per answer (st3); `rate_limit_rejections_total{source="binance"}` +1 per failed run (st5, with the engine) | §2.5.1; Core §2.5.1; X1 D-16, X1 D-29 | — |
| X1-T210 | One limiter per source | Two connections; `CreateConnection` during a sync | Count reservations | Engine and `CreateConnection` reserve in the same budgets (st4, with the registration of the connector) | Core FR-110; X1 D-29 | — |

### Phase 3 — Key check (st4)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T301 | Read-only key | Common; key K | `CreateConnection` with K | `ACTIVE`, `EXCHANGE`, permissions `["READ"]`, fingerprint; `external_account` = the fictitious `uid`; cursor `balances` due at once (from st5; st4 declares no stream, X1 D-33); audit `CONNECTION_CREATED`; reservations 1 in `sapi:/sapi/v1/account/apiRestrictions` and 20 in `api`, besides the time call | UC-201, FR-202; Core UC-101; §3.2 Performance | — |
| X1-T302 | Each permission beyond reading | Nine variants of `key_read_only.json`: one of `enableWithdrawals`, `enableInternalTransfer`, `permitsUniversalTransfer`, `enableSpotAndMarginTrading`, `enableMargin`, `enableFutures`, `enableVanillaOptions`, `enablePortfolioMarginTrading`, `enableFixApiTrade` `true` | `CreateConnection` per fixture | `FAILED_PRECONDITION / KEY_NOT_READ_ONLY` naming the Binance field; no connection, no ciphertext | FR-201, EC-201; Core EC-101, FR-102; X1 P-5 | — |
| X1-T303 | Unknown permission | `enableNewThing` `true`; then `false` | `CreateConnection` | `true`: rejected, `enableNewThing` named; `false`: accepted | EC-214, FR-201; X1 D-1 | — |
| X1-T304 | Fields that are not permissions | `ipRestrict` `true`; `createTime`; a non-boolean field; `enableFixReadOnly` `true` | `CreateConnection` | Accepted in each case, permissions `["READ"]` | EC-214; X1 D-1 | — |
| X1-T305 | Reading not enabled | `enableReading` `false` | `CreateConnection` | `KEY_INVALID`; nothing stored | UC-201 step 2 | — |
| X1-T306 | Key rejected by Binance | `apiRestrictions` answers `-2015`; then `401`, `-2014`, `-1022` | `CreateConnection` | `KEY_INVALID`, with the hint that the key may be restricted to another IP; nothing stored | EC-202; X1 D-3 | — |
| X1-T307 | Account call rejected | `apiRestrictions` read-only; `account` answers `-2015` | `CreateConnection` | `KEY_INVALID`; nothing stored | UC-201 step 4 | — |
| X1-T308 | IP restriction noted | `ipRestrict` `false`; then `true` | `CreateConnection`; read the audit row | Both accepted; details of `CONNECTION_CREATED` hold `ip_restricted` `false`, then `true`; the response is unchanged | EC-213; X1 D-2 | — |
| X1-T309 | Periodic check stops a key | T301 connection; `KEY_CHECK_INTERVAL` short | Next check with `-2015`; another connection with `enableWithdrawals` `true` | Both `CREDENTIALS_INVALID` with audit rows: reason key rejected; reason not read-only with the field named; streams stop (from st5, when streams exist). A check that stays read-only moves `permissions_checked_at` and writes no audit row | FR-203, EC-203, EC-204; Core FR-119, EC-108, EC-116 | — |
| X1-T310 | Periodic check cannot reach Binance | T301 connection | Checks answered `429`, unreachable, `403` | Status unchanged; next check by the backoff and not before the end of a pause | Core EC-118 | — |
| X1-T311 | Binance unavailable at creation | Fake answers `429`; unreachable | `CreateConnection` | `UNAVAILABLE`; nothing stored | Core EC-104 | — |
| X1-T312 | Same account again | T301 connection | `CreateConnection` with another key of the same `uid` | `ALREADY_EXISTS` | Core EC-102, FR-104 | — |

### Phase 4 — Balance snapshot (st5)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T401 | Spot | `account` with balances above 0 and one at 0 | Run `balances` | `account` called with `omitZeroBalances=true`; one `SPOT` row per balance with `free` or `locked` above 0 | UC-202 step 1; X1 D-9 | — |
| X1-T402 | Funding | `get-funding-asset` with `locked`, `freeze`, `withdrawing` above 0 | Run `balances` | `FUNDING` row: `free` as given, `locked` = `locked` + `freeze` + `withdrawing` | UC-202 step 2 | — |
| X1-T403 | Earn flexible | Two flexible positions | Run `balances` | `EARN_FLEXIBLE` rows, `totalAmount` in `free`, `locked` 0 | UC-202 step 3 | — |
| X1-T404 | Earn locked | Three locked positions, two of one asset | Run `balances` | `EARN_LOCKED` rows, `amount` in `locked`, summed per asset: two rows | UC-202 step 4 | — |
| X1-T405 | Paging | 150 flexible positions in pages of 100; `total` given | Run `balances` | Pages read with `size` 100 and `current` 1, 2 until `total` is reached or a page is short; more than 100 pages fail the snapshot; 150 reserved per page; the whole snapshot reserves 20 in `api`, 1 in the funding budget and 150 per Earn page | UC-202 steps 3–4; §3.2 | — |
| X1-T406 | Wrapper asset | Spot `LDUSDT`; a flexible position of `USDT`; no alias `LDUSDT` | Run `balances` | No `SPOT` row of `LDUSDT`; `USDT` counted once, in `EARN_FLEXIBLE` | EC-209, FR-205 | — |
| X1-T407 | Real asset starting with `LD` | Spot `LDO`; alias `LDO`; a flexible position of `O` absent or present | Run `balances` | `LDO` kept in `SPOT` | EC-216 | — |
| X1-T408 | `LD` code without a position | Spot `LDXYZ`; no flexible position of `XYZ`; no alias | Run `balances` | Kept under its native code; `unmapped_assets_total` +1 | UC-202 step 5; EC-208 | — |
| X1-T409 | Unknown asset | Spot asset without an alias | Run `balances` | Kept under its native code; `unmapped_assets_total` +1 | EC-208; Core EC-109 | — |
| X1-T410 | One source fails | A snapshot exists; then each of the four sources fails in turn (error, `429`) | Run `balances` per case | No snapshot written; the previous one returned; `stale` after `stale_after` | FR-204, EC-215, EC-211; Core FR-109 | — |
| X1-T411 | Balance dropped to zero | A snapshot with asset X; the next answers without X | Run `balances`; `GetBalances` | The new snapshot has no row of X; `GetBalances` returns no balance of X: zero | EC-228; Core §2.1.3; X1 D-9 | — |
| X1-T412 | Time of the snapshot | Fake server time with an offset | Run `balances`; `GetBalances` | `as_of` = server time at the first request of the run | UC-202; X1 D-9 | — |
| X1-T413 | Malformed amount | A balance `abc`; a negative one; one with 19 decimals | Run `balances` per case | The snapshot is refused as a whole: `abc` and the negative one by the connector, 19 decimals by the ledger writer; the previous one stays | Core EC-117 | — |
| X1-T414 | Stream and trigger | T301 connection | Read the declared streams; `TriggerSync` | `balances` every 15 min from `sources.config`; first run at creation; `TriggerSync` runs it | UC-202 trigger; US-210; Core FR-120 | — |
| X1-T415 | `GetBalances` | A snapshot of all four account types | `GetBalances` by `connection_id` | Rows of `SPOT`, `FUNDING`, `EARN_FLEXIBLE`, `EARN_LOCKED`; `as_of`; `stale` `false` | US-203, US-204; Core §2.1.3 | — |
| X1-T416 | Spot step on the test network | Owner; test-network key in `.env` | Run the live test of the spot step (`make binance-live`) | Balances read with the zero-row rule; status and count of `SPOT` balances printed; nothing stored; funding and Earn not called | Package st5 exit; X1 D-20 | — |

### Phase 5 — Connector suite, script, done-when on fixtures (st6)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T501 | Shared connector suite | `internal/connector/connectortest` with the snapshot and key-check parts; the Binance connector on the fake server | Run the suite with no network | Snapshot: the same answers give an equal snapshot; one failing source fails the whole snapshot; a rate limit gives `RateLimitError`, a pause of its budget and no further request. Key check: read-only accepted, non-read rejected with names, a rejected key `ErrKeyRejected` | FR-216, ADR-2; X1 D-12 | — |
| X1-T502 | Fixture scan | `testdata/fixtures/binance/` | Scan every file | A valid fixture; no `X-MBX-APIKEY`, no `signature` parameter or member, no hexadecimal value of 64 characters, no `timestamp`, no URL, no host, no key marker of the tests; `uid` only the fictitious value. The word Signature of Binance's message of `-1022` is allowed | FR-217 | — |
| X1-T503 | Done-when 1 with the real binaries | `server` and `casctl` as binaries on the test database; fake Binance server | `CreateConnection` with each permission of T302 and an unknown one; then with K | Each rejected with `KEY_NOT_READ_ONLY` naming it, nothing stored (no connection, ciphertext or cursor); K accepted `ACTIVE`, `["READ"]`, and its first balance snapshot written by the engine of `server` | Done-when 1; FR-201; X1 P-5 | — |
| X1-T504 | Done-when 4 | T502; branch pushed | `secrets` workflow and `make secrets` | Green; `testdata/` holds no key and no real account identifier | Done-when 4; FR-217 | — |
| X1-T505 | Read-only by construction | Code of X1 | List the endpoints the connector can call | Only the read endpoints of §2.1.2 of X1; none needs a trade, withdrawal or transfer permission | ADR-3; §2.1.2 | — |
| X1-T506 | `connect.sh` | `server` against the fake Binance server; marker key and secret in a test `.env` | Run `connect.sh create` with a key that can withdraw, then with the read-only key; `buf` and `jq` through shims that record their argv and their environment; run `connect.sh delete <id>` | The first is refused with the gRPC code, message and reason (a code of capitals only, X1 D-41); the connection is created `ACTIVE`; the key and the secret reach `jq` on its standard input (X1 D-40): the markers are in no argv, no environment, no output and no file left in the temporary directories; the output holds only `connection_id`, status, fingerprint and permissions; delete removes the connection; a remote `CAS_GRPC_ADDR` and a missing key are refused | X1 D-19, X1 D-39, X1 D-40, X1 D-41 | — |

### Phase 6 — Real account (st7, owner's evidence)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T601 | Preconditions and dry run | Identity review finished, residence Serbia; key created with reading only, HMAC still offered, in `.env` only | Owner: `make binance-real` (`TestT601_RealAccountDryRun`, X1 D-43): the key check and the snapshot once with the real key | Every endpoint of X1 answers; counts of assets per account type printed; no amount; no connection created | Package §5; X1 D-4, X1 D-20 | — |
| X1-T602 | Connection | T601; tenant `x1-real` with a token | `scripts/binance/connect.sh create` (`owner_ref` `owner-x1`, label `Binance`); `make casctl ARGS="connection inspect <connection_id>"` (X1 D-45) | `ACTIVE`, `["READ"]`; audit details `ip_restricted` `false` | UC-201; EC-213; X1 D-2, X1 D-18, X1 D-19 | — |
| X1-T603 | Balances equal the UI | T602; first snapshot | Owner compares `GetBalances` with the UI per account type: `scripts/binance/connect.sh balances <connection_id>` for the counts, `… --amounts` for the local comparison only (X1 D-44) | Date, verdict, four counts (spot, funding, Earn flexible, Earn locked), each equal to the UI. Funding count checked against the note of the funding endpoint; positions with `collateralAmount` or `redeemingAmt`, if any, described without names | FR-206; done-when 2; X1 D-9, X1 D-10, X1 D-11; P-1 | — |
| X1-T604 | Wrapper assets | T603 | Owner reads the LD counts of `make binance-real`: spot codes starting with `LD`, those with a flexible position of the rest of the code, those with an equal amount | Form and mirroring of the wrapper codes described without names, e.g. "one Earn wrapper asset mapped by the `LD` rule"; SRS — Binance §4 issue 2 closed with the date | Issue 2 | — |
| X1-T605 | Regional restrictions | T601 | The statuses of the six endpoints in the report of `make binance-real` | Every endpoint of X1 answered for the account; SRS — Binance §4 issue 6 closed with the date | Issue 6 | — |
| X1-T606 | Done-when 3 | T602; `KEY_CHECK_INTERVAL=30m` | `server` runs at least 2 h; read `/metrics` at the end | `sync_runs_total{source="binance",stream="balances",result="success"}` at least 8 and no `failure`; `key_checks_total{source="binance",result="success"}` at least 4, no `invalid` or `failure` (X1 D-50); `binance_rate_limit_responses_total` 0; `rate_limit_rejections_total{source="binance"}` 0; `binance_used_weight` under `budget_share` of every budget | Done-when 3; FR-213; X1 P-8, X1 D-50 | — |
| X1-T607 | `/sapi` headers observed | T601 (X1 D-48) | Owner reads the used-weight headers of the four `/sapi` endpoints in the report of `make binance-real`, not the bodies | Header names and used weights as SRS — Binance §2.1.1 | §2.1.1 Rate limits | — |
| X1-T608 | Deletion | T603 – T607 recorded | `scripts/binance/connect.sh delete <connection_id>`; `connect.sh balances <connection_id>`; `make casctl ARGS="connection inspect <connection_id>"` | `NOT_FOUND`; no secret, snapshot or cursor of the connection left; only the audit rows, without `uid` | US-205; Core UC-104, FR-118; X1 P-7 | — |

### Phase 7 — CI and repository (st2, st8)

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| X1-T701 | Secret scan | Branch | Push; `make secrets` | Green; `testdata/` and `.env.example` hold no key; `.env.example` holds the names of the Binance variables without values | Handoff §4 | — |
| X1-T702 | `go` workflow | Branch | Push | The new packages run in exactly one job; every job within 12 min | Handoff §3.4 | — |
| X1-T703 | `cas.v1` unchanged | Branch | `make proto-check` | The build equals the frozen image | Core §2.1.1 | — |
| X1-T704 | Documents match the code | st8 | Compare SRS — Binance, SRS — Core, PRD — Exchange Accounts, glossary, `docs/README.md` with the code | No mismatch | Package 8.2 | — |
| X1-T705 | Tag guard by hand | Pull request merged | `git fetch`; HEAD = `origin/main`; `git merge-base --is-ancestor <last commit of the branch> HEAD`; `gh run watch --exit-status` for each run of the merge commit; then `git tag -a v0.5.0` | Every step passes before the tag; the tag is on the merge commit | Handoff §3.4; X1 P-9 | — |

### Coverage of the requirements

| FR | Rows | FR | Rows |
|---|---|---|---|
| FR-201 | T302, T303, T503 | FR-206 | T603 |
| FR-202 | T301 | FR-213 | T203, T204, T205, T606 |
| FR-203 | T309 | FR-214 | T206, T207, T208 |
| FR-204 | T410 | FR-216 | T501 |
| FR-205 | T406 | FR-217 | T107, T110, T502, T504 |

| EC | Rows | EC | Rows |
|---|---|---|---|
| EC-201 | T302 | EC-213 | T308, T602 |
| EC-202 | T306 | EC-214 | T303, T304 |
| EC-203 | T309 | EC-215 | T410 |
| EC-204 | T309 | EC-216 | T407 |
| EC-205 | T206 | EC-221 | T105 |
| EC-208 | T408, T409 | EC-228 | T411 |
| EC-209 | T406 | Core EC-104 | T311 |
| EC-211 | T410 | Core EC-118 | T310 |

### Coverage of the exit criteria

| Criterion | Rows |
|---|---|
| A key with any non-read permission is rejected | T503, T302, T303 |
| Balances equal the exchange UI on the real read-only account | T603 |
| No "rate limit exceeded" answer in normal operation | T606 |
| Fixtures contain no key and no real account identifier | T504, T502 |

## 5. Run log

Batches run in the QA stage st8 in phase order; one line per batch and attempt. Cycle: fix → retest → regression of C1, S2 and S3. Go tests: `go test -race -count=1 -p 1 -json ./...` with `-json` passed to `go test`, never through `GOFLAGS`.

| Date | Batch | Attempt | Pass | Fail | Failed IDs | Notes |
|---|---|---|---|---|---|---|

## 6. Success criteria and sign-off

- Every row of §4 is `Pass`, locally and in CI; phase 6 rows by the owner on the real account, phase 1 and 4 live rows on the test network.
- 0 open P0 and P1.
- The proto equals its frozen image.
- SRS — Binance and SRS — Core match the code.
- Open P2 and P3 are listed in `known-issues.md` with their row IDs.

| Role | Name | Date | Result |
|---|---|---|---|
| Owner, QA | Igor Kudinov | — | — |
