# Test Plan — C1 Core

- **Milestone:** C1 — Core, version `v0.2.0`, branch `feature/v0.2.0`.
- **Objects under test:** `server`, `casctl`, the contract `proto/cas/v1`, the migrations of the Core tables, the CI workflows of C1.
- **Parents:** [SRS — Core](srs/core.md) UC-101, UC-102, UC-104, UC-105; [SRS — EVM Connector](srs/evm-connector.md) UC-301; [BRD](brd.md) BR-1, BR-13, G-6, §9.2; ADR [2](adr/0002-connector-interface.md), [4](adr/0004-secrets-encrypted-at-rest.md), [5](adr/0005-canonical-ledger.md), [6](adr/0006-polling-with-cursors.md), [7](adr/0007-api-protocols-and-tenancy.md).
- **Status:** Draft. Created in the discovery stage of C1, completed stage by stage, signed off in the closing stage (§6).

## 1. Environment

| Item | Value |
|---|---|
| Go | `1.27.1`. Same version locally and in CI |
| PostgreSQL | 16. Development server 16.14; CI service container `postgres:16` |
| Contract tools | buf `1.73.0`; protoc-gen-go `1.36.12`; protoc-gen-go-grpc `1.6.2` |
| Database tools | sqlc `1.31.1`; golang-migrate `4.20.1` |
| Secret scan | gitleaks `8.30.1` |
| Libraries | Pinned in `go.mod` |
| Test database | Locally `cas_test`; in CI the service container. `TEST_DATABASE_URL`: owner role, runs the migrations and cleans tables. `TEST_DATABASE_URL_SERVER`: role `cas_server`, used by the code under test |
| Runs | `go test -race -p 1 ./...`: the integration tests share one database |
| Sources | The fake connector and the EVM address check. No row calls a real exchange or chain |
| Secrets | Keys, tokens and the master key of the tests are generated at run time. No real key anywhere |

## 2. Conventions

- Row format: `ID | Description | Preconditions | Steps | Expected | Req | Status`. `Req` names the requirement IDs and sections the row covers; a section without a document name is of SRS — Core.
- ID `C1-Tnnn`; the hundreds digit is the phase. A Go test names its row: `TestT603_RepeatedSyncAddsNoEntries`.
- `Status`: `—` not run, `Pass`, `Fail <attempt>`, `Blocked`. Filled in the QA stage (§5).
- Severity of a failure: P0 a secret or another tenant's data is exposed, or a ledger entry is lost or duplicated; P1 a requirement of the scope is not met; P2 defect of a metric, the CLI or a message; P3 cosmetic.
- Rows of phases 1, 2 and 8 check the binaries, the contract and CI. The other rows are Go tests: unit tests without a database, or integration tests with PostgreSQL and the gRPC server in process, with the real interceptors.
- Time in tests is a fake clock: intervals, backoff, cooldown and staleness are tested without waiting.
- **Common** preconditions: migrated test database; tenant A with a valid token; source `fake` of kind `EXCHANGE` available; the fake connector scripted by the row.
- **Exchange connection:** Common, and a connection of tenant A on `fake`. The fake declares the streams `balances` and `ops` unless the row says otherwise.
- **Wallet connection:** Common, and a connection of tenant A on `anvil`.
- **Tenant B:** a second tenant with its own token and its own connection.

## 3. Phases

| Phase | Scope | Stage | Rows |
|---|---|---|---|
| 1 | Configuration and health | st2 | 6 |
| 2 | Contract | st3 | 5 |
| 3 | Schema | st4 | 8 |
| 4 | Tenants and access | st5 | 14 |
| 5 | Connections and vault | st6a, st6b | 37 |
| 6 | Sync engine and ledger | st7a, st7b | 36 |
| 7 | Read API | st8 | 21 |
| 8 | CI and repository | st2, st9 | 6 |

## 4. Test matrix

### Phase 1 — Configuration and health

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T101 | Required configuration | — | Start `server` without `DATABASE_URL`; then without `CAS_MASTER_KEY` | Exit code ≠ 0; the message names the variable and prints no value | §3.1 | — |
| C1-T102 | Defaults | Only the required variables set | Read the parsed configuration | gRPC 50053, health 8091, `failure_threshold` 5, backoff 30 s up to 1 h, cooldown 60 s, key check 24 h, tick 1 s, workers 4, lock retry 10 s, fake source off | §3.1 | — |
| C1-T103 | Invalid configuration | — | Start with a non-numeric port; a master key that is not 32 bytes in base64; a malformed duration | Exit code ≠ 0 in each case | §3.1, ADR-4 | — |
| C1-T104 | `/healthz` | `server` running | `GET /healthz` | 200 | §3.1 | — |
| C1-T105 | `/readyz`, database | `server` running | `GET /readyz`; make the database unreachable; `GET /readyz` | 200; then 503 | §3.1 | — |
| C1-T106 | `/metrics` | `server` running | `GET /metrics` on the health port | 200, Prometheus text format | §2.5.1 | — |

### Phase 2 — Contract

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T201 | Lint | `proto/cas/v1` | `buf lint` | Passes with the `STANDARD` rules; enum values carry the enum name as a prefix | §2.1.1 | — |
| C1-T202 | Method catalogue | Contract | Read the service descriptors | `ConnectionService` with 6 methods, `AccountDataService` with 2; no `CardService` | §2.1.1 | — |
| C1-T203 | Messages against the SRS | Contract | Compare with §2.1.1 – §2.1.4 | Every field of the SRS exists with the stated type; no amount and no `seq` is a float; the selectors of `CreateConnection` and `GetBalances` are `oneof` | §2.1.1 – §2.1.4, handoff §4 | — |
| C1-T204 | Generated code is current | Contract | `buf generate`; `git diff` | No diff | Handoff §3.4 | — |
| C1-T205 | Freeze guard | Frozen contract | On a throw-away copy remove a field; remove a method; add a field. Run the freeze check against `proto/frozen/cas_v1.json` | Fails in all three cases: `buf breaking` for the removals, the comparison with the image for the addition | §2.1.1, QA gate | — |

### Phase 3 — Schema

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T301 | Apply on a clean database | Empty database | Run the migrations up | Ten Core tables: `tenants`, `api_credentials`, `sources`, `connections`, `asset_aliases`, `sync_cursors`, `balance_snapshots`, `snapshot_balances`, `ledger_entries`, `audit_log`. No `cards`, no `reconciliation_runs` | §2.4 | — |
| C1-T302 | Roll back and apply again | T301 done | Run the migrations down to zero; up again | Empty schema after down; the same schema after up | Package st4 | — |
| C1-T303 | Uniqueness | Migrated database | Insert a duplicate of each key | Refused: `tenants.name`; `api_credentials.key_id`; `sources.code`; `connections (tenant_id, source_id, external_account)`; `ledger_entries (connection_id, stream, external_id, leg)` | §2.4, FR-104, FR-106 | — |
| C1-T304 | Value constraints | Migrated database | Insert `amount` 0 and negative; negative `free`, `locked`; an unknown status, kind, mode, leg, type, direction | Refused in each case | §2.4, handoff §4 | — |
| C1-T305 | Rights of `cas_server` | Role `cas_server` | As `cas_server`: DDL; `UPDATE` and `DELETE` on `ledger_entries`, `balance_snapshots`, `snapshot_balances`; `UPDATE`, `DELETE`, `SELECT` on `audit_log`; `INSERT` on `tenants`, `api_credentials`, `sources`, `asset_aliases` | Permission denied in each case | §3.2, FR-111 | — |
| C1-T306 | Cascade under `cas_server` | A connection with cursors, snapshots and entries | As `cas_server` delete the connection | Its cursors, snapshots, balances and entries are gone | §3.2, FR-118 | — |
| C1-T307 | Seed | Migrated database | Read `sources` | `anvil` with chain ID 31337 and `base-sepolia` with 84532: kind `EVM`, enabled, `config` with `chain_id` only. No `fake`, no `binance`. The development seed adds `fake` | SRS — EVM §2.4, §2.4 | — |
| C1-T308 | `/readyz`, schema version | `server` running | `GET /readyz` on the current schema; on a database one migration behind | 200; then 503 | §3.1 | — |

### Phase 4 — Tenants and access

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T401 | Create a tenant | Migrated database | `casctl tenant create` twice with the same name; then with names outside the rule of UC-105 | First: row with status `ACTIVE`. Second: refused, nothing stored. Names outside the rule: refused, nothing stored | UC-105, EC-120 | — |
| C1-T402 | Issue a token | A tenant | `casctl token issue`; read `api_credentials` | The token is printed once; the row holds `key_id` and a hash; the secret is nowhere in the database | UC-105, ADR-7, §3.2 | — |
| C1-T403 | Valid token | Common | Call any method | The request runs in the tenant of the token | §2.1.1 | — |
| C1-T404 | Bad token | Common | Call without metadata; without `Bearer`; with an unknown `key_id`; with a wrong secret | `UNAUTHENTICATED` with the same message in all four cases | §2.1.1 | — |
| C1-T405 | Revoked token | Common | `casctl token revoke`; call a method | `UNAUTHENTICATED` from the next request on | UC-105 | — |
| C1-T406 | Disabled tenant | Common | `casctl tenant disable`; call; `enable`; call | `UNAUTHENTICATED` while disabled, as for an unknown token; served again after enable; data unchanged | UC-105, EC-121 | — |
| C1-T407 | Every method needs a token | Contract | Table test over the service descriptors: call each method without a token | None of the 8 methods answers | FR-105 | — |
| C1-T408 | Isolation of reads | Tenant B; both tenants use the same `owner_ref` | As B: `GetConnection` and `GetBalances(connection_id)` on A's connection; `ListConnections`, `GetBalances(owner_ref)`, `ListLedgerEntries` | `NOT_FOUND` for A's connection; the lists return only B's rows | FR-105, BR-13, G-6 | — |
| C1-T409 | Isolation of writes | Tenant B | As B: `DeleteConnection` and `TriggerSync` on A's connection | `NOT_FOUND`; A's data and schedule unchanged | FR-105, BR-13 | — |
| C1-T410 | Same account in two tenants | Tenant B | A and B connect the same account | Both connections are created | FR-104, BR-13 | — |
| C1-T411 | Token format | — | Issue two tokens | `cas_` + 12 hexadecimal characters + `_` + 64 hexadecimal characters; the tokens differ; `secret_hash` is the SHA-256 of the secret | UC-105 | — |
| C1-T412 | Several valid tokens | A tenant with two tokens | Call with each; revoke the first; call with each | Both work; then only the second | UC-105 | — |
| C1-T413 | Audit of CLI changes | Migrated database | Create a tenant, disable and enable it, issue and revoke a token | One audit row per change: `TENANT_CREATED`, `TENANT_DISABLED`, `TENANT_ENABLED`, `CREDENTIAL_ISSUED`, `CREDENTIAL_REVOKED`; `object_id` set; `credential_id` empty | FR-121 | — |
| C1-T414 | Lists show no secret | A tenant with a token | `casctl tenant list`, `casctl token list` | Tenants and tokens with status and dates; no secret and no hash | UC-105 | — |

### Phase 5 — Connections and vault

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T501 | `ListSources` | Common; one disabled source; one source without a connector | `ListSources` | Available sources only, with `code` and `kind`, ordered by `code` | §2.1.1 | — |
| C1-T502 | Create, exchange | Common | `CreateConnection` with `exchange_key` | Response of §2.1.2: `ACTIVE`, `EXCHANGE`, fingerprint, permissions `["READ"]`; one cursor per declared stream, due at once; audit `CONNECTION_CREATED` with the acting credential | UC-101, FR-101, FR-117 | — |
| C1-T503 | Create, wallet | Common | `CreateConnection` with `wallet` on `anvil` | `EVM_WALLET`; `wallet_address` and the stored identity in EIP-55 form; `credentials_enc` empty, no fingerprint; audit record | UC-101, FR-101, FR-302, FR-117 | — |
| C1-T504 | Validation | Common | Send: no `owner_ref`; `owner_ref` of 129 characters; `label` of 65; no source; no credential; `wallet` for an exchange; `exchange_key` for a network; `api_key` of 15 and of 257 characters; `api_secret` of 15 and of 4097 | `INVALID_ARGUMENT` in each case; nothing stored. 128 and 64 characters of a multi-byte script are accepted; so are keys and secrets at both ends of their ranges | UC-101 step 1, §2.1.1 | — |
| C1-T505 | Source unknown or disabled | Common; one disabled source | Create on an unknown code; on the disabled source | `NOT_FOUND`; `FAILED_PRECONDITION` with `ErrorInfo` reason `SOURCE_DISABLED`, domain `cas` | UC-101 step 2, §2.1.1 | — |
| C1-T506 | Key not read-only | Common; the fake reports a trade permission | `CreateConnection` | `FAILED_PRECONDITION / KEY_NOT_READ_ONLY`; no connection, no ciphertext | EC-101, FR-102 | — |
| C1-T507 | Key rejected by the source | Common; the fake rejects the key | `CreateConnection` | `FAILED_PRECONDITION / KEY_INVALID`; nothing stored | UC-101 step 3 | — |
| C1-T508 | Source unreachable at the key check | Common; the fake is unreachable; then it answers with a rate limit; then its budget is paused longer than the wait; then it fails in another way | `CreateConnection` | `UNAVAILABLE`; `UNAVAILABLE`; `UNAVAILABLE`; `INTERNAL`. Nothing stored | EC-104, UC-101 step 3, FR-110 | — |
| C1-T509 | Permissions not reported | Common; the fake declares that it cannot read permissions | `CreateConnection` | Created with `["UNVERIFIED"]`; the audit record says so | EC-103, FR-117 | — |
| C1-T510 | Same account again | Exchange connection | Create with another key of the same account | `ALREADY_EXISTS` | EC-102, FR-104 | — |
| C1-T511 | Same account, concurrent requests | Common | Send the same `CreateConnection` from several goroutines | Exactly one succeeds; the others get `ALREADY_EXISTS` | FR-104 | — |
| C1-T512 | Atomic insert | Common; a failure injected at step 8 | `CreateConnection` | `INTERNAL`; no connection, no cursor and no audit row | UC-101 step 8 | — |
| C1-T513 | Encrypted at rest | Exchange connection | Read `credentials_enc`; decrypt with the vault | The column holds neither key nor secret in clear; decryption returns the original; `kek_version` is stored | FR-103, ADR-4 | — |
| C1-T514 | Ciphertext bound to its row | Two exchange connections | Copy the ciphertext of one to the other; decrypt | Decryption fails | ADR-4 | — |
| C1-T515 | Fail closed | Exchange connection | Decrypt with another master key; with one changed byte; with another `CAS_MASTER_KEY_VERSION` | Error in each case; no plaintext | ADR-4, §3.1 | — |
| C1-T516 | Fresh key material | — | Encrypt the same key twice | Different ciphertexts | ADR-4 | — |
| C1-T517 | Fingerprint only in the API | Exchange connection; failed creations of T506, T507 | Read the responses and error messages of create, get and list | At most the fingerprint: `…` and the last 4 characters of the API key | FR-103, §2.1.2 | — |
| C1-T518 | Nothing secret in the logs | Log captured | Run create, a sync, a key check and their failures | The log contains no API key, no secret, no service token and no master key | FR-103, handoff §4 | — |
| C1-T519 | `GetConnection` | Exchange connection after one sync | `GetConnection` | The connection of §2.1.2 and, per stream: `stream`, `mode`, `next_run_at`, `last_success_at`, `last_error`, `consecutive_failures`; no cursor | §2.1.1, FR-101 | — |
| C1-T520 | `ListConnections` | 120 connections of two owners | List all; filter by `owner_ref`; walk the pages; send `page_size` 0, 501, −1; send a malformed `page_token` | Order `created_at`, `connection_id`; the filter returns one owner; the pages return every connection once and the last `next_page_token` is empty; 0 → 100; 501 → 500; −1 → `INVALID_ARGUMENT`; malformed token → `INVALID_ARGUMENT` | §2.1.1, FR-101 | — |
| C1-T521 | Delete | Exchange connection with snapshots and entries | `DeleteConnection` | Connection, secret, cursors, snapshots and entries are removed in one transaction; audit `CONNECTION_DELETED` stays | UC-104, FR-101, FR-118, FR-117 | — |
| C1-T522 | Nothing readable after deletion | T521 done | `GetConnection`, `ListConnections`, `GetBalances`, `ListLedgerEntries` | `NOT_FOUND`; absent from the lists and the reads | FR-118 | — |
| C1-T523 | Delete during a sync | Exchange connection; the fake blocks inside a page | `DeleteConnection`; release the fake | The run stops at its next step; no row of the connection reappears | UC-104 step 3 | — |
| C1-T524 | Delete twice, or an unknown ID | Common | Delete a deleted connection; an unknown ID; a malformed ID | `NOT_FOUND`; `NOT_FOUND`; `INVALID_ARGUMENT` | UC-104, §2.1.1 | — |
| C1-T525 | Failed delete | Exchange connection; a failure injected in the transaction | `DeleteConnection` | Rollback: nothing is removed, no audit row | UC-104 step 2 | — |
| C1-T526 | Connect again after deletion | T521 done | Create the same account again | New connection ID, empty history | FR-104 | — |
| C1-T527 | Address format | Common | Send 39 and 41 hexadecimal characters; no prefix; prefix `0X`; a non-hexadecimal character; surrounding whitespace | `INVALID_ARGUMENT` in each case | UC-301, FR-301 | — |
| C1-T528 | Wrong checksum | Common | Send a mixed-case address with one letter in the wrong case | `INVALID_ARGUMENT` | EC-301, FR-301 | — |
| C1-T529 | Valid checksum | Common | Send the reference addresses of EIP-55 | Accepted | FR-301 | — |
| C1-T530 | Single case | Common | Send an address all in lower case; another all in upper case | Accepted; stored and returned in EIP-55 form | FR-301, FR-302 | — |
| C1-T531 | Zero address | Common | Send `0x` + 40 zeros | `INVALID_ARGUMENT` | EC-302 | — |
| C1-T532 | Same address in another case | Wallet connection | Create with the same address in another letter case | `ALREADY_EXISTS` | EC-303, FR-302, FR-104 | — |
| C1-T533 | Same address on two networks | Wallet connection | Create with the same address on `base-sepolia` | Second connection created | FR-104 | — |
| C1-T534 | No network call | — | Run the address check with no network | Passes: the connector of C1 has no RPC client | UC-301 | — |
| C1-T535 | Chain outside the allow-list | `EVM_ALLOWED_CHAIN_IDS` without 84532 | Start; `ListSources`; create on `base-sepolia` | A log line at start; `base-sepolia` is not listed; `FAILED_PRECONDITION / SOURCE_DISABLED` | FR-318, EC-318 | — |
| C1-T536 | Fake source behind its flag | Migrated database | `casctl source add-fake` twice; `ListSources` and create without `ENABLE_FAKE_SOURCE`; then with it | One row `fake`, enabled, kind `EXCHANGE`. Without the flag: not listed, `SOURCE_DISABLED`. With it: listed, creation works | §2.1.1, §2.4, §3.1 | — |
| C1-T537 | Wallet without streams | Wallet connection | Read `sync_cursors`; `GetConnection` | No cursor, no sync; `streams` is empty | UC-101 postcondition, SRS — EVM §2.1.1 | — |

### Phase 6 — Sync engine and ledger

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T601 | First sync | Common; the fake holds balances and three pages of entries | Create the connection; run the engine | Each stream runs once: one snapshot, the entries of all pages; `last_success_at` set, failures 0, `next_run_at` = now + interval | UC-102 | — |
| C1-T602 | Schedule | Exchange connection, synced | Advance the clock to just before, then to `next_run_at` | No run before; one run at the tick after | UC-102 step 1 | — |
| C1-T603 | Repeated sync | Exchange connection, synced | Reset the cursor of the fake; sync again | 0 new entries; the skipped ones are counted | FR-106, EC-105 | — |
| C1-T604 | Overlapping page | Exchange connection, synced | The fake returns a page with known and new records | Known ones skipped, new ones inserted, the cursor moves | EC-105, FR-106 | — |
| C1-T605 | Entries and cursor together | Exchange connection; a failure injected between insert and cursor update | Sync | Neither the entries nor the cursor change | FR-107 | — |
| C1-T606 | Resume after a stop | Common; the fake holds a backfill of five pages | Stop the engine after page 2; start a new engine | It continues from the stored cursor; the ledger equals an uninterrupted run; no duplicates | FR-108, EC-106 | — |
| C1-T607 | Mode switch | Exchange connection in `BACKFILL` | Sync to the end of history | The stream becomes `INCREMENTAL`; the mode is stored | UC-102 Stream modes | — |
| C1-T608 | Legs | Common; the fake returns an operation with base, quote and fee legs | Sync twice | Three entries with one `group_id` = family + `external_id`; no fourth on the second run | ADR-5, §2.1.4 | — |
| C1-T609 | Raw record | Exchange connection, synced | Read `raw` of every entry | Equal to the record the fake delivered | FR-113 | — |
| C1-T610 | Immutable entries | Exchange connection, synced | The fake returns a known record with another amount | The stored entry is unchanged | FR-111 | — |
| C1-T611 | Not final yet | Common; the fake holds one record back as not final | Sync; mark it final; sync | Absent after the first run; imported by the second | EC-110, FR-111 | — |
| C1-T612 | `seq` in commit order | Several connections; a reader pulling by `after_seq` in a loop | Sync all connections concurrently | The reader ends with every entry; `seq` grows in commit order | FR-114 | — |
| C1-T613 | Backfilled operation | Exchange connection with newer entries | Import an older operation | It gets a higher `seq` than the entries already stored | §2.1.4, FR-114 | — |
| C1-T614 | Asset mapping | Common; an alias for one native code | The fake returns one known and one unknown code | Known → canonical asset; unknown kept as it is, counted and logged | EC-109, UC-102 step 4 | — |
| C1-T615 | Snapshot | Exchange connection | Sync the balance stream twice | Two rows in `balance_snapshots` with their balances; the first stays | §2.4 | — |
| C1-T616 | Failed snapshot | Exchange connection, synced | The fake fails the balance call | Nothing written; the previous snapshot stays the latest | UC-102 step 6, FR-109 | — |
| C1-T617 | Failure and backoff | Exchange connection | The fake fails a stream several times, then succeeds | `last_error` stored; counter + 1; next run after 30 s, 60 s, 120 s … at most 1 h; the success zeroes the counter | UC-102 step 8, §3.1 | — |
| C1-T618 | Degraded and recovered | Exchange connection | Fail `ops` five times; let it succeed. Repeat with both streams failing | `DEGRADED` at the fifth failure; the other stream keeps its schedule; reads answer. `ACTIVE` at the first success, or when both recovered. Audit `CONNECTION_DEGRADED` and `CONNECTION_RECOVERED`, `credential_id` empty | EC-111, §2.3.1, FR-117 | — |
| C1-T619 | Key rejected during a sync | Exchange connection | The fake rejects the key | `CREDENTIALS_INVALID`; no further call for the connection; audit record | EC-108, FR-117 | — |
| C1-T620 | Periodic key check | Two exchange connections | Advance 24 h with a read-only key; then with a key that gained a permission. On the second connection: a key that the source rejects | `permissions_checked_at` moves; then `CREDENTIALS_INVALID`, streams stop, audit record. Second connection: `CREDENTIALS_INVALID`, audit record | FR-119, EC-116, EC-108, FR-117 | — |
| C1-T621 | Key check scope | An exchange and a wallet connection | Advance less than 24 h; then 24 h | No check before the interval; never for the wallet; the check passes the limiter | FR-119, UC-102 | — |
| C1-T622 | One limiter per source | Two exchange connections | Sync both | Every call of the fake carried a reservation; both connections draw on one budget | FR-110 | — |
| C1-T623 | Budget spent | Exchange connection; a small budget | Sync more pages than the budget allows | The run waits and continues when the budget allows | UC-102 step 2, FR-110 | — |
| C1-T624 | "Rate limit exceeded" | Two budgets | The fake answers with a pause of 60 s on one budget | No call of that budget for 60 s; the other budget goes on; the run counts as failed; its next run is not before the end of the pause | EC-107, FR-110 | — |
| C1-T625 | Parallel work | Six connections; `SYNC_WORKERS` 4 | Sync; `TriggerSync` during a run | At most four connections at a time; the streams of one connection one after another; no stream twice at once | §3.2, UC-102 | — |
| C1-T626 | One engine | Two `server` processes on one database | Start both; stop the lock holder | Only one runs streams; the other serves the API and takes the lock within `SYNC_LOCK_RETRY` | §3.2 | — |
| C1-T627 | `last_error` without secrets | Exchange connection | The fake returns an error that contains the key and the secret | `last_error` and the log hold the message without them | §2.1.1 Connector contract, FR-103 | — |
| C1-T628 | A source is a connector and seed data | — | Check the imports of the engine, ledger and API packages | No connector package imported; no code of a source named | FR-112, ADR-2 | — |
| C1-T629 | Invalid entry or balance | Exchange connection | The fake returns a page with one invalid entry: amount 0; negative; 19 decimal places; 21 integer digits; empty `external_id`; unknown type, leg or direction. Then a snapshot with one invalid balance: negative; 19 decimal places; 21 integer digits; unknown account type; the same account type and asset twice | The page is rolled back, its valid entries too; the cursor stays; a failure is recorded. The snapshot is refused as a whole; the previous one stays the latest; a failure is recorded | EC-117 | — |
| C1-T630 | Stream declared later | Exchange connection; the fake starts to declare a third stream | Restart the engine | The missing cursor is created and due at once; the other cursors are unchanged | FR-122, EC-119 | — |
| C1-T631 | Key check, source unreachable | Exchange connection | The fake is unreachable at the key check; then it answers with a rate limit; then the engine restarts | The status does not change; the check is repeated with backoff; after the restart it runs at once | EC-118 | — |
| C1-T632 | Empty snapshot | Exchange connection; the fake returns no balances | Sync | One row in `balance_snapshots`, none in `snapshot_balances`; the run is a success | UC-102 step 6, §2.4 | — |
| C1-T633 | Disabled tenant, unavailable source | Exchange connection | Disable the tenant; advance the clock; enable. Then disable the source; advance the clock; enable | No run and no failure counted while the tenant is disabled or the source is not available; the streams run again afterwards, from their cursors | UC-105, UC-102 preconditions | — |
| C1-T634 | Metrics | A scripted scenario: success, failure, duplicate, unknown asset, limit answer | Read `/metrics` | The nine metrics of §2.5.1 with the expected values | §2.5.1 | — |
| C1-T635 | Interval from the connector | The fake declares streams with intervals of 5 and 20 min | Sync | `next_run_at` of each stream uses its own interval | §2.1.1 Connector contract, §3.1 | — |
| C1-T636 | Page limit per run | Exchange connection; the fake holds a backfill of seven pages; `SYNC_MAX_PAGES_PER_RUN` 3 | Run the engine until the backfill ends | Three runs of the stream: 3, 3 and 1 pages. A run that stops at the limit is a success and its stream is due at once; a due balance stream of the connection runs in between; the ledger equals an uninterrupted run | UC-102 step 7, §3.1 | — |

### Phase 7 — Read API

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T701 | Balances by owner | Two synced connections of one owner | `GetBalances(owner_ref)` | The latest snapshot of each connection; fields of §2.1.3 | §2.1.3 | — |
| C1-T702 | Balances by connection | Exchange connection, synced | `GetBalances(connection_id)` | That connection only | §2.1.3 | — |
| C1-T703 | Selector missing | Common | `GetBalances` with neither field | `INVALID_ARGUMENT` | §2.1.3 | — |
| C1-T704 | Never synced | An exchange connection before its first sync; a wallet connection | `GetBalances` | Listed with `stale: true`, no `as_of`, no balances | §2.1.3 | — |
| C1-T705 | Stale flag | Exchange connection, synced; `stale_after` 30 min | Read; fail the sync for 31 min and read; let it succeed with a `taken_at` one hour old and read | `false`; `true` with the same balances; `false` with `as_of` one hour old. Reads succeed throughout | FR-109, §2.1.3 | — |
| C1-T706 | `stale_after` default | A source without `stale_after`; balance interval 15 min | Read after 29 and after 31 min without a sync | `false`; `true` | §3.1 | — |
| C1-T707 | Latest snapshot only | Exchange connection with three snapshots; the last has other assets | `GetBalances` | Only the balances of the latest snapshot | §2.1.3 | — |
| C1-T708 | No call to a source | Exchange connection | `GetBalances`, `ListLedgerEntries` | The fake records 0 calls | §2.1.1 | — |
| C1-T709 | Unknown owner, unknown connection | Common | `GetBalances` with each | Empty response; `NOT_FOUND` | §2.1.3 | — |
| C1-T710 | Ledger order | Exchange connection with entries | `ListLedgerEntries` without and with `after_seq`; with an `after_seq` above the last; with a negative `after_seq` | Ascending `seq`; only entries above `after_seq`; `last_seq`, `has_more`; the empty response returns the `after_seq` of the request; negative → `INVALID_ARGUMENT` | §2.1.4, FR-114 | — |
| C1-T711 | Incremental pull | 250 entries; new ones committed during the reading | Read in pages of 100 by `after_seq` = `last_seq` | Every entry once, also those committed between two pages | FR-114 | — |
| C1-T712 | Filters | Entries of two owners, two connections, several types and times | Filter by `owner_ref`, `connection_id`, `types`, `occurred_from`, `occurred_to`, alone and combined; filter by an unknown `connection_id`; send an unknown value in `types` | Only matching entries; `occurred_from` inclusive, `occurred_to` exclusive; `NOT_FOUND` for the unknown connection; `INVALID_ARGUMENT` for the unknown type | §2.1.4 | — |
| C1-T713 | Page size | 600 entries | `page_size` absent, 0, 500, 501, −1 | 100; 100; 500; 500; `INVALID_ARGUMENT` | §2.1.1 | — |
| C1-T714 | Entry fields | Exchange connection with entries | `ListLedgerEntries` | The fields of §2.1.4, `native_asset` included; no `raw` | §2.1.4 | — |
| C1-T715 | Exact amounts | Entries and balances with `0.000000000000000001`, `99999999999999999999.999999999999999999`, `0.012500`, balance 0 | Read | The first two unchanged; `0.0125`; `0`. No exponent | §2.1.1, §2.4, handoff §4 | — |
| C1-T716 | `TriggerSync` | Two exchange connections, synced, not due | `TriggerSync`. On a second connection: `TriggerSync` while one of its streams is running | Every stream is due now and runs on the next tick; `last_manual_sync_at` set; no audit record. Second connection: the running stream is not run again, its other streams run | FR-120 | — |
| C1-T717 | Cooldown | Two exchange connections | `TriggerSync` twice within 60 s; on the other connection; again after 60 s; again after a restart of `server` inside the cooldown | `RESOURCE_EXHAUSTED` for the second call; the other connection is accepted; accepted after 60 s; the restart does not reset the cooldown | EC-112, FR-120 | — |
| C1-T718 | `TriggerSync` on a stopped connection | Exchange connection in `CREDENTIALS_INVALID` | `TriggerSync` | `FAILED_PRECONDITION / CREDENTIALS_INVALID` | §2.1.1 | — |
| C1-T719 | Read latency | 100 000 entries, 50 connections | Measure `GetBalances` and `ListLedgerEntries` | p95 ≤ 100 ms. A failure is P2 | §3.2 | — |
| C1-T720 | Empty snapshot in a read | T632 done | `GetBalances` | The connection has `as_of`, `stale: false` and no balances. After a snapshot with balances, an empty one hides them | §2.1.3, §2.4 | — |
| C1-T721 | `TriggerSync` without streams | Wallet connection | `TriggerSync` twice | Accepted, nothing runs; the second call inside the cooldown is `RESOURCE_EXHAUSTED` | §2.1.1 | — |

### Phase 8 — CI and repository

| ID | Description | Preconditions | Steps | Expected | Req | Status |
|---|---|---|---|---|---|---|
| C1-T801 | Go job | Workflow in the branch | Push to `feature/v0.2.0`; open the pull request; after the merge look at `main` | Format, vet, build and tests against PostgreSQL 16 run and pass on all three | Package decision 2 | — |
| C1-T802 | Secret scan | Workflow in the branch | Push; then push a planted test secret to a throw-away branch | The whole history is scanned and passes with the allow-list of the documented example values; the planted secret fails the job | Package decision 1 | — |
| C1-T803 | Contract job | Workflow in the branch | Push a change under `proto/` | T201, T204 and T205 run in CI | §2.1.1 | — |
| C1-T804 | Repository hygiene | Branch before the pull request | Read `.gitignore`, `.env.example` and the diff of the branch | `.env` ignored; `.env.example` without values; no private address and no key in the diff | Handoff §4, package §3 | — |
| C1-T805 | `contracts` workflow | — | `git diff v0.1.0 -- .github/workflows/contracts.yml`; its last run | No diff; green | Package 2.5 | — |
| C1-T806 | No real source | Built binaries | List the registered connectors | Only the EVM address check and, with its flag, the fake connector | Package §2 | — |

### Coverage of the requirements

| FR | Rows |
|---|---|
| FR-101 | T502, T503, T519, T520, T521 |
| FR-102 | T506 |
| FR-103 | T513, T517, T518, T627 |
| FR-104 | T303, T410, T510, T511, T526, T532, T533 |
| FR-105 | T407, T408, T409 |
| FR-106 | T303, T603, T604 |
| FR-107 | T605 |
| FR-108 | T606 |
| FR-109 | T616, T705 |
| FR-110 | T622, T623, T624 |
| FR-111 | T305, T610, T611 |
| FR-112 | T628 |
| FR-113 | T609 |
| FR-114 | T612, T613, T710, T711 |
| FR-117, connections | T502, T503, T509, T521, T618, T619, T620 |
| FR-118 | T306, T521, T522 |
| FR-119 | T620, T621 |
| FR-120 | T716, T717 |
| FR-121 | T413 |
| FR-122 | T630 |
| FR-301 | T527, T528, T529, T530 |
| FR-302 | T503, T530, T532 |
| FR-318 | T535 |

| EC | Rows | EC | Rows |
|---|---|---|---|
| EC-101 | T506 | EC-112 | T717 |
| EC-102 | T510 | EC-116 | T620 |
| EC-103 | T509 | EC-117 | T629 |
| EC-104 | T508 | EC-118 | T631 |
| EC-105 | T603, T604 | EC-119 | T630 |
| EC-106 | T606 | EC-120 | T401 |
| EC-107 | T624 | EC-121 | T406 |
| EC-108 | T619, T620 | EC-301 | T528 |
| EC-109 | T614 | EC-302 | T531 |
| EC-110 | T611 | EC-303 | T532 |
| EC-111 | T618 | EC-318 | T535 |

EC-114 is tested in S2: the card registry does not exist in C1.

### Coverage of the exit criteria of C1

| Criterion | Rows |
|---|---|
| A connection of each kind can be created, listed and deleted | T502, T503, T520, T521 |
| A two-tenant test proves isolation | T408, T409 |
| A repeated sync adds no entries | T603 |
| A stream resumes from its cursor after a restart | T606 |
| Secrets are stored encrypted; responses and logs show the fingerprint only | T513, T517, T518 |

## 5. Run log

Batches run in the QA stage in phase order; one line per batch and attempt. Cycle: fix → retest → regression of the earlier phases.

| Date | Batch | Attempt | Pass | Fail | Failed IDs | Notes |
|---|---|---|---|---|---|---|

## 6. Success criteria and sign-off

- Every row of §4 is `Pass`, locally and in CI.
- 0 open P0 and P1. Open P2 and P3 are listed in `known-issues.md` with their row IDs.
- The contract in `proto/cas/v1` equals its frozen image `proto/frozen/cas_v1.json`; the generated code is current.
- `docs/srs/core.md` and `docs/srs/evm-connector.md` match the code.

| Role | Name | Date | Result |
|---|---|---|---|
| Owner, QA | Igor Kudinov | — | — |

## 7. Notes of the QA stage

Filled in the QA stage.
