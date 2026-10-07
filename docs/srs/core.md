# SRS — Core

---

## 1. Introduction

- **Purpose:** specify the `server` binary: what every source and every consumer shares.
- **Scope:** tenants and access, connections, secrets, sync engine, balances, ledger, card registry, gRPC API, data model.
- **Milestones:** C1 (tenants, connections, engine, API), S2 (card registry), S3 (on-chain connector, reconciliation report), X1–X2 (first exchange connector).
- **Out of scope:**
  - exchange-specific behaviour — SRS — Binance;
  - authorization flow and contract — [SRS — Card Spend](card-spend.md);
  - the EVM connector (event indexer) — [SRS — EVM Connector](evm-connector.md);
  - tenant self-onboarding, credential rotation through the API, data retention jobs — §4.
- **Parents:** [BRD](../brd.md) BR-1 … BR-5, BR-13; [PRD — Card Spend](../prd/card-spend.md) US-6, US-7, US-8, US-13; [PRD — Exchange Accounts](../prd/exchange-accounts.md) US-204, US-205, US-208 … US-210, US-213; [ADR](../adr/README.md) 1–7, 11, 13.
- **Version:** 1.4, 2026-10-07. Completed by the discovery of S3 (decisions S3 D-n): `GetReconciliationReport` and its mismatch types (S3 D-10, S3 D-21); the balance checkpoint in the connector contract and `balance_checkpoints` (S3 D-3, S3 D-5); the metric `ledger_gap` (S3 D-19); `reconciliation_runs` per source (S3 D-6, S3 D-7); the reconciliation worker (S3 D-8); `casctl source set`, `source set-treasury`, `reconcile` (S3 D-2, S3 D-8, S3 D-17, S3 D-27, S3 D-32); the alias rows in `Source` (S3 D-31); rights of `cas_server` for S3 (S3 D-7). Version 1.3, 2026-10-05. Completed by the discovery of S2: the methods of `CardService`, processor credentials in `casctl`, rights of the role `cas_card_auth`, EC-114 without card removal. Completed in S2 st10a, 2026-10-06, to match the code: `owner_ref` length of `RegisterCard`, pagination of `ListCards` and `ListAuthorizations`, rights of `cas_server` on the card-auth tables, `ON DELETE RESTRICT` of `cards.connection_id`. Owner's answers of 2026-10-06: leading zeros of `daily_limit`, the validation of `GetCard`, `GetAuthorization` and the filters of `ListAuthorizations`, tombstones under a card filter, `casctl processor list [<tenant>]`, a repeated revoke. Version 1.2, 2026-10-05. Completed while C1 was built: tenant name and key length rules, `wallet_address`, ID and page token rules, connector contract as built, rate limiter on every account check, source availability in the engine, page limit per run, invalid balances, key check cases, format of `sources.config`, metric labels. Version 1.1, 2026-10-04: completed by the discovery of C1. Version 1.0 approved 2026-10-04.

| Term | Meaning |
|---|---|
| Tenant | A consuming system: a partner, ET, the demo. Owns all its data. |
| Owner | The tenant's user, known to CAS only as an opaque `owner_ref`. |
| Source | An exchange or an EVM network that CAS can read. |
| Connection | One account at one source: an exchange API key or a wallet address. |
| Connector | Code adapter of one source behind the common interface (ADR-2). |
| Stream | One kind of data of a connection with its own cursor: balances, deposits, trades of a pair, event logs. Streams of one kind form a family: `trades:BTCUSDT` and `trades:ETHUSDT` belong to `trades`. |
| Snapshot | Balances of a connection at one moment. |
| Ledger entry | One immutable movement of one asset. An operation with several assets has several entries: legs. |
| Canonical asset | Asset code used across sources: `BTC`, `USDC`. The source's own code is the native asset. |

---

## 2. Functional Requirements

### 2.1 SRS for API

#### 2.1.1 Core

##### Components diagram

```mermaid
flowchart LR
    consumer[Consumer<br/>partner, ET, CLI]

    subgraph server[server]
        api[gRPC API]
        registry[Registry<br/>tenants, connections, cards]
        vault[Secret vault]
        engine[Sync engine<br/>scheduler, cursors, health]
        limiter[Rate limiter]
        conn[Connectors<br/>binance, evm]
        ledger[Ledger writer]
        recon[Reconciliation worker]
    end

    db[(PostgreSQL)]
    src[Sources<br/>exchange API, EVM RPC]

    consumer -- "gRPC + service token" --> api
    api --> registry
    api -- "read only" --> db
    registry --> vault
    registry --> db
    engine --> limiter
    engine --> conn
    conn -- "reserve cost" --> limiter
    conn -- "HTTPS, JSON-RPC" --> src
    conn --> vault
    engine --> ledger
    ledger --> db
    engine --> db
    recon -- "read cursors, entries, card tables, write runs" --> db
```

| Component | Responsibility |
|---|---|
| gRPC API | Authentication, tenant scoping, validation, reads from the database |
| Registry | Tenants, credentials, connections, cards |
| Secret vault | Encrypts and decrypts exchange secrets (ADR-4) |
| Sync engine | Schedules streams, keeps cursors and health, drives connectors |
| Rate limiter | One limiter per source. Its budgets are defined by the connector and shared by the connections they cover. The connector reserves the cost of every request in it |
| Connectors | Source-specific requests, signing, pagination, normalization |
| Ledger writer | Single writer of ledger entries and snapshots; computes the gap of a balance checkpoint |
| Reconciliation worker | From S3: runs SRS — Card Spend UC-4 per source when the treasury connection has new final data; stores the run (S3 D-8) |

##### Sequence diagram

One sync step of one stream.

```mermaid
sequenceDiagram
    autonumber
    participant E as Sync engine
    participant C as Connector
    participant L as Rate limiter
    participant S as Source
    participant D as PostgreSQL

    E->>D: Pick a due stream, read its cursor
    E->>C: Fetch page(cursor)
    C->>L: Reserve request cost
    L-->>C: Granted
    C->>S: Signed request
    S-->>C: Page of native records
    C-->>E: Normalized entries + next cursor
    E->>D: One transaction: insert entries, skip known ones, move the cursor
    E->>D: Health: last_success_at, failures = 0
```

##### Common rules

- **Protocol:** gRPC, package `cas.v1`. The `.proto` files in `proto/cas/v1/` are the contract: `connection_service.proto`, `account_data_service.proto` and, from S2, `card_service.proto`; `buf lint` and `buf breaking` run in CI. The contract is frozen as the image `proto/frozen/cas_v1.json`: CI compares every build with it, and a change of the contract updates the image by the owner's decision.
- **Transport:** plain gRPC inside the internal network, no TLS (ADR-7). Server reflection is on and needs no token.
- **Authorization:** metadata `authorization: Bearer <service token>`. The token resolves to one tenant. Format and handling of the token: UC-105.
- **Tenant scope:** every request reads and writes only the caller's tenant. A resource of another tenant is reported as `NOT_FOUND`.
- **Reads never call a source.** Balances and ledger come from the database; freshness is reported, not forced.
- **Amounts:** decimal strings in asset units, without exponent and without trailing zeros: `0.0125`, `0`. Token base units where stated.
- **Pagination:** `page_size`: 100 when absent or 0, maximum 500; a larger value is cut to 500; a negative one is `INVALID_ARGUMENT`. `ListConnections`, `ListCards` and `ListAuthorizations` continue with `page_token` → `next_page_token`; the token is opaque and belongs to its list, and a malformed one, or one of another list, is `INVALID_ARGUMENT`. `ListLedgerEntries` continues with `after_seq` (§2.1.4).
- **Lengths** of strings count characters, not bytes.
- **IDs:** `connection_id` is a UUID in its 36-character form. Any other value is `INVALID_ARGUMENT`; a well-formed ID that matches nothing in the tenant is `NOT_FOUND`.
- **Enum values** carry the name of their enum as a prefix on the wire: `CONNECTION_STATUS_ACTIVE`. The examples of this document show the short form.
- **Available source:** a source is offered when it is enabled, its connector is registered and, for an EVM network, its chain ID is in the allow-list (SRS — EVM Connector §2.1.1). Any other source is treated as disabled. Availability is evaluated on every request.
- **Errors:**

| gRPC status | Meaning |
|---|---|
| `UNAUTHENTICATED` | Missing, unknown or revoked token; token of a disabled tenant |
| `INVALID_ARGUMENT` | Validation failed |
| `NOT_FOUND` | No such resource in the caller's tenant |
| `ALREADY_EXISTS` | The account is already connected, or the card is already registered with different data |
| `FAILED_PRECONDITION` | The request is valid but not allowed in the current state; the reason is in the error detail |
| `RESOURCE_EXHAUSTED` | Manual sync requested inside the cooldown |
| `UNAVAILABLE` | During key verification the source cannot be reached or answers with a rate limit |
| `INTERNAL` | Unexpected failure; nothing was stored |

- **Error detail:** `google.rpc.ErrorInfo` with `domain = "cas"` and one of these reasons:

| Reason | Status | Meaning |
|---|---|---|
| `KEY_NOT_READ_ONLY` | `FAILED_PRECONDITION` | The key allows trading, withdrawal or transfer |
| `KEY_INVALID` | `FAILED_PRECONDITION` | The source rejects the key |
| `SOURCE_DISABLED` | `FAILED_PRECONDITION` | The source is not available |
| `CREDENTIALS_INVALID` | `FAILED_PRECONDITION` | The connection is stopped: its key was rejected or is no longer read-only |
| `CONNECTION_NOT_USABLE` | `FAILED_PRECONDITION` | `RegisterCard`: the connection is not a wallet, not `ACTIVE` or `DEGRADED`, or belongs to another owner (EC-113). From S2 |
| `CONNECTION_HAS_CARDS` | `FAILED_PRECONDITION` | `DeleteConnection`: cards are bound to the connection (EC-114). From S2 |

##### Method catalogue

| Service | Method | Purpose | Milestone |
|---|---|---|---|
| `ConnectionService` | `ListSources` | Enabled exchanges and networks | C1 |
| | `CreateConnection` | Register an exchange key or a wallet address | C1 |
| | `ListConnections` | Connections of the tenant, filter by `owner_ref` | C1 |
| | `GetConnection` | One connection with the health of its streams | C1 |
| | `DeleteConnection` | Remove a connection and its data | C1 |
| | `TriggerSync` | Run the streams of a connection now | C1 |
| `AccountDataService` | `GetBalances` | Latest balances with freshness | C1 |
| | `ListLedgerEntries` | Operation history | C1 |
| `CardService` | `RegisterCard` | Bind a `card_ref` to a wallet connection | S2 |
| | `UpdateCard` | Change the daily limit; freeze or unfreeze | S2 |
| | `GetCard`, `ListCards` | Read cards | S2 |
| | `GetAuthorization`, `ListAuthorizations` | Status and history of authorizations | S2 |
| | `GetReconciliationReport` | Result of a reconciliation run | S3 |

Methods with non-obvious rules are specified below. The others follow the common rules, the table below and the `.proto` definitions.

| Method | Request | Response | Rules |
|---|---|---|---|
| `ListSources` | — | `sources[]`: `code`, `kind` | Available sources only, ordered by `code`. No pagination |
| `ListConnections` | `owner_ref`, optional, ≤ 128; `page_size`, `page_token` | `connections[]`: the connection of §2.1.2; `next_page_token` | Order: `created_at`, `connection_id`. No stream health. `next_page_token` is empty on the last page |
| `GetConnection` | `connection_id` | The connection of §2.1.2 and `streams[]`: `stream`, `mode`, `next_run_at`, `last_success_at`, `last_error`, `consecutive_failures` | Streams ordered by `stream`. The cursor is not returned |
| `DeleteConnection` | `connection_id` | Empty | UC-104. A second call: `NOT_FOUND`. Audit `CONNECTION_DELETED` with the acting credential |
| `TriggerSync` | `connection_id` | Empty | Every stream of the connection becomes due now; the engine runs them on its next tick. Inside `trigger_sync_cooldown` after the last accepted call: `RESOURCE_EXHAUSTED`; the time of that call is `connections.last_manual_sync_at`. Connection in `CREDENTIALS_INVALID`: `FAILED_PRECONDITION / CREDENTIALS_INVALID`; this is checked before the cooldown. Connection without streams: accepted, nothing runs. A stream that is running when the call arrives is not run again. No audit record |
| `UpdateCard` | `card_ref`; optional `daily_limit`; optional `status` | The card of §2.1.5 | S2. At least one of the two optional fields; otherwise `INVALID_ARGUMENT`. `daily_limit`: a base-unit integer string ≥ 0. `status`: `ACTIVE` or `FROZEN`. A value equal to the stored one changes nothing and writes no audit row; a change sets `updated_at` and writes `CARD_UPDATED` with the changed fields. The change applies to the next authorization (EC-115) |
| `GetCard` | `card_ref`, 1 to 64 characters | The card of §2.1.5 | S2. `card_ref` empty or longer: `INVALID_ARGUMENT`. Unknown in the tenant: `NOT_FOUND` |
| `ListCards` | `owner_ref`, optional, ≤ 128; `page_size`, `page_token` | `cards[]`: the card of §2.1.5; `next_page_token` | S2. Order: `created_at`, `card_ref`. Pagination as `ListConnections` |
| `GetAuthorization` | `auth_id`, 1 to 64 characters | The authorization of SRS — Card Spend §2.1.4: the same fields, `returns` and `history` included, plus `card_ref`, `received_at`, `decided_at`; timestamps as `Timestamp`; `tx_hash` as SRS — Card Spend §2.1.4 | S2. `auth_id` empty or longer: `INVALID_ARGUMENT`; the length is counted in characters, the character rule of the processor API is not applied. Unknown in the tenant: `NOT_FOUND`. A tombstone is returned as in SRS — Card Spend §2.1.4 |
| `GetReconciliationReport` | `source`, required; `run_id`, optional | `run`: the reconciliation run below | S3 (S3 D-10). `source` empty: `INVALID_ARGUMENT`; unknown: `NOT_FOUND`. `run_id` malformed: `INVALID_ARGUMENT`. Empty `run_id`: the newest run of the caller's tenant for the source. A `run_id` of another tenant or another source, or no run at all: `NOT_FOUND`. No list method |
| `ListAuthorizations` | optional `card_ref` ≤ 64, `owner_ref` ≤ 128, `status`, `received_from`, `received_to`; `page_size`, `page_token` | `authorizations[]`: the authorization without `returns` and `history`; `next_page_token` | S2. Filters combine with AND; `received_from` inclusive, `received_to` exclusive; an unknown `status`, a longer `card_ref` or `owner_ref`, and an invalid `received_from` or `received_to` are `INVALID_ARGUMENT`. Order: `received_at` descending, `auth_id`. Tombstones are included without these two filters: a tombstone has no card, so a `card_ref` or `owner_ref` filter, which is the owner of the card, does not return it |

###### Reconciliation run

Returned by `GetReconciliationReport`; stored in `reconciliation_runs` (§2.4). Rules: SRS — Card Spend UC-4.

| Field | Type | Description |
|---|---|---|
| `run_id` | String, UUID | ID of the run |
| `source` | String | Source code: `base-sepolia` |
| `period_from`, `period_to` | Timestamp | Checked period (SRS — Card Spend UC-4) |
| `to_block` | String, integer | Last block whose logs the run read: `next_block − 1` of the treasury's `logs` cursor |
| `totals` | Object | `authorizations_checked`, `debits_count`, `debits_amount`, `returns_checked`, `refunds_count`, `refunds_amount`. Amounts in token base units |
| `mismatches[]` | Array | One object per mismatch, fields below |
| `created_at` | Timestamp | When the run was stored |

| Mismatch field | Type | Present for |
|---|---|---|
| `type` | Enum | Always: `MISSING_DEBIT`, `AMOUNT_MISMATCH`, `UNKNOWN_DEBIT`, `UNEXPECTED_DEBIT`, `MISSING_REFUND`, `UNKNOWN_REFUND`, `TREASURY_MISMATCH` (S3 D-21) |
| `auth_id`, `return_id` | String | Mismatches of the caller's own authorizations and returns. Never an ID of another tenant |
| `chain_auth_id`, `chain_refund_id` | String, 0x + 64 hex | Every mismatch of a debit or a refund |
| `tx_hash` | String | The transaction of the event; for `MISSING_*`, the transaction of `card-auth` by the rule of SRS — Card Spend §2.1.4, when there is one |
| `expected_amount`, `actual_amount` | String, integer | Token base units: the amount of CAS and the amount of the event |
| `asset`, `checkpoint_balance`, `ledger_total` | String; decimals in asset units | `TREASURY_MISMATCH` only |

##### Connector contract

What the engine expects from every connector (ADR-2). The Go interface below is as built in C1, package `internal/connector`; the table gives the rules.

```go
type Connector interface {
    Capabilities() Capabilities
    CheckAccount(ctx context.Context, src Source, cred Credentials, lim Limiter) (AccountInfo, error)
    Streams(ctx context.Context, src Source, account AccountInfo) ([]Stream, error)
    Budgets(src Source) []Budget
    FetchPage(ctx context.Context, conn Connection, stream string, mode Mode, cursor json.RawMessage) (Page, error)
    FetchSnapshot(ctx context.Context, conn Connection) (Snapshot, error)
}

type Limiter interface {
    Reserve(ctx context.Context, budget string, cost int) error
    Pause(budget string, d time.Duration)
}
```

- `Connection` carries the connection ID, the source, the account identity, the decrypted key when there is one and the limiter of the source.
- `Source` carries the code, the kind, the enabled flag, `config` and, from S3, the alias rows of the source: native asset, canonical asset, decimals. The engine and `CreateConnection` load them with the source; a connector takes the assets it tracks from them and never reads the database (S3 D-31).
- `Page`: entries, next cursor, next mode, more pages and, from S3, an optional balance checkpoint: block number, block hash, block time and balances per native asset (S3 D-3). `Entry`: `external_id`, leg, type, direction, native asset, amount, `occurred_at`, raw record. `Snapshot`: time and balances. `Balance`: account type, native asset, free, locked.
- Typed errors: `ErrKeyRejected`, `ErrUnreachable`, `KeyNotReadOnlyError`, `RateLimitError`, `InvalidInputError`.
- ADR-2 shows the first sketch of this interface; the names differ, the decision does not.

| Topic | Rule |
|---|---|
| Registration | A connector is registered under the code of its source. The EVM connector is one for every source of kind `EVM`. A source without a connector is not available |
| Capabilities | Flags declared by the connector. C1 reads one: key permissions readable (EC-103). A flag is added when the engine starts to use it |
| Account check | Returns the account identity and, for a key, its permissions. Used by `CreateConnection` and by the periodic key check; both pass the limiter of the source (FR-110) |
| Streams | The connector declares the streams of a connection: name, family, interval, first mode, first cursor. It reads the interval from `sources.config` |
| Page | One call returns entries, the next cursor, the mode and whether more pages follow. Only final records are returned |
| Balance checkpoint | From S3. A page may carry the balances of the account at the point its records end (EVM: the final block, SRS — EVM Connector UC-304). Balances follow the rules of a snapshot balance; an invalid checkpoint refuses the page (EC-117). In the transaction of the page the ledger writer computes per native asset: ledger total = Σ `IN` − Σ `OUT` of the connection, gap = checkpoint balance − ledger total; it stores the result in `balance_checkpoints` and reports `ledger_gap`. The same rule for every source: no branch by source (ADR-2) |
| Snapshot | One call returns all balances of the connection and their time, or fails as a whole. A balance has `free` and `locked` not negative, with at most 18 decimal places and 20 integer digits, a known account type, and appears once per account type and native asset. A snapshot that breaks this is refused as a whole (EC-117) |
| Rate limiter | One limiter per source for the whole process, built on first use from the budgets the connector declares. The engine and `CreateConnection` hand it to the connector. The connector reserves the cost before every request and reports the limit answers of the source. A budget is a number of cost units per time window: a reservation waits until the window allows it. A window starts with the first reservation after the previous window ended. A pause demanded by the source blocks its budget until the pause ends |
| Errors | Typed: key rejected, key not read-only, rate limit with the pause the source demands, source unreachable. Anything else is a plain failure of the run |
| Entry | Positive amount with at most 18 decimal places and 20 integer digits; `external_id` not empty; a known type, leg and direction. An entry that breaks this fails its page (EC-117). Amounts travel as decimal strings and are stored as `NUMERIC`: no float and no exponent on the path |
| Secrets | A connector gets the decrypted key only for the call and never puts it into an error or a log line. The engine removes the key and the secret from `last_error` and from its own log lines |
| Fake connector | Scripted connector for tests. In a running `server` only with `ENABLE_FAKE_SOURCE`: development and demo, fictitious data only. Without a script it accepts any key, derives the account from the API key and reports `READ`; it returns a small fixed history and a snapshot of `BTC` and `USDT` |

#### 2.1.2 CreateConnection

##### Description
Registers one account at one source for an owner and starts its first sync.

##### Endpoint
`cas.v1.ConnectionService/CreateConnection`

##### Authorization
See Common rules.

##### Request parameters
###### Request example
```json
{
  "owner_ref": "user-4821",
  "source": "binance",
  "label": "Main account",
  "exchange_key": { "api_key": "vmPUZE6mv9SD…", "api_secret": "NhqPtmdSJYdK…" }
}
```

```json
{
  "owner_ref": "user-4821",
  "source": "base-sepolia",
  "label": "Card wallet",
  "wallet": { "address": "0x9aF3…41cE" }
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| owner_ref | String, ≤ 128 | Yes | Opaque owner ID of the tenant | `user-4821` |
| source | String | Yes | Source code from `ListSources` | `binance` |
| label | String, ≤ 64 | No | Display name | `Main account` |
| exchange_key | Object | For exchanges | API key and secret. Never returned. | — |
| wallet | Object | For EVM networks | Wallet address | — |

- `exchange_key` and `wallet` are alternatives (`oneof`). The one sent must match the kind of the source; otherwise `INVALID_ARGUMENT`.
- `api_key`: 16 to 256 characters. `api_secret`: 16 to 4096 characters. Neither is trimmed or changed; another length is `INVALID_ARGUMENT`.

##### Response parameters
###### Response body example
```json
{
  "connection_id": "0c9e6a1e-7f41-4f0a-9d5e-1b2f3c4d5e6f",
  "source": "binance",
  "kind": "EXCHANGE",
  "owner_ref": "user-4821",
  "label": "Main account",
  "status": "ACTIVE",
  "key_fingerprint": "…9SDq",
  "permissions": ["READ"],
  "created_at": "2026-10-03T10:00:00Z"
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| connection_id | String, UUID | Yes | Connection ID | — |
| kind | Enum | Yes | `EXCHANGE` or `EVM_WALLET` | `EXCHANGE` |
| status | Enum | Yes | Connection state, §2.3.1 | `ACTIVE` |
| key_fingerprint | String | For exchanges | Last 4 characters of the API key, after `…` | `…9SDq` |
| permissions | Array of strings | For exchanges | Permissions reported by the source | `["READ"]` |
| wallet_address | String | For EVM wallets | The address in EIP-55 form, whatever letter case was sent | `0x9aF3…41cE` |

- On the wire the response carries this object in its field `connection`. `GetConnection` and `ListConnections` return the same object.
- `FAILED_PRECONDITION / KEY_NOT_READ_ONLY`: the key allows trading, withdrawal or transfer.
- `FAILED_PRECONDITION / KEY_INVALID`: the source rejects the key.
- `ALREADY_EXISTS`: this exchange account or wallet address is already connected in the tenant.
- `NOT_FOUND`: no source with this code.
- `FAILED_PRECONDITION / SOURCE_DISABLED`: the source is not available.

#### 2.1.3 GetBalances

##### Description
Returns the latest snapshot of each selected connection. Never calls the source.

##### Endpoint
`cas.v1.AccountDataService/GetBalances`

##### Authorization
See Common rules.

##### Request parameters
###### Request example
```json
{ "owner_ref": "user-4821" }
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| owner_ref | String | One of the two | All connections of the owner | `user-4821` |
| connection_id | String, UUID | One of the two | One connection | — |

- `owner_ref` and `connection_id` are alternatives (`oneof`). A request with neither: `INVALID_ARGUMENT`.
- Unknown `owner_ref`: empty response. Unknown `connection_id`: `NOT_FOUND`.
- `owner_ref` empty or longer than 128 characters: `INVALID_ARGUMENT`.

##### Response parameters
###### Response body example
```json
{
  "connections": [
    { "connection_id": "0c9e6a1e-…", "source": "binance", "as_of": "2026-10-03T10:15:00Z", "stale": false },
    { "connection_id": "5b7d2c90-…", "source": "base-sepolia", "as_of": "2026-10-03T09:40:00Z", "stale": true }
  ],
  "balances": [
    { "connection_id": "0c9e6a1e-…", "account_type": "SPOT", "asset": "BTC", "native_asset": "BTC", "free": "0.0125", "locked": "0" },
    { "connection_id": "0c9e6a1e-…", "account_type": "EARN_FLEXIBLE", "asset": "USDT", "native_asset": "USDT", "free": "150", "locked": "0" },
    { "connection_id": "5b7d2c90-…", "account_type": "WALLET", "asset": "USDC", "native_asset": "0x7a1c…e9b2", "free": "70.133613", "locked": "0" }
  ]
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| connections[].as_of | Timestamp | After the first sync | `taken_at` of the returned snapshot | — |
| connections[].stale | Boolean | Yes | `true` when the last successful run of the balance stream is older than `stale_after`, or when there was none | `false` |
| balances[].account_type | Enum | Yes | `SPOT`, `FUNDING`, `EARN_FLEXIBLE`, `EARN_LOCKED`, `WALLET` | `SPOT` |
| balances[].asset | String | Yes | Canonical asset | `BTC` |
| balances[].native_asset | String | Yes | Code or token address at the source | `BTC` |
| balances[].free | String, decimal | Yes | Available amount | `0.0125` |
| balances[].locked | String, decimal | Yes | Amount in orders or lock-up | `0` |

- An asset absent from the snapshot has a zero balance.
- The snapshot returned for a connection is its newest by import time. Order: connections as in `ListConnections`; balances by connection, account type, native asset.
- A connection that has never synced appears in `connections` with `stale: true`, no `as_of` and no balances.
- A snapshot may be empty: the connection then has `as_of` and no balances.
- `stale` measures the age of the sync; `as_of` shows the age of the data.
- CAS returns no fiat valuation (ADR-1).

#### 2.1.4 ListLedgerEntries

##### Description
Returns ledger entries of the tenant. Built for incremental pull: the consumer keeps the last `seq` and asks for what came after it.

##### Endpoint
`cas.v1.AccountDataService/ListLedgerEntries`

##### Authorization
See Common rules.

##### Request parameters
###### Request example
```json
{ "owner_ref": "user-4821", "after_seq": "18220", "page_size": 100 }
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| owner_ref | String | No | Filter by owner | `user-4821` |
| connection_id | String, UUID | No | Filter by connection. Unknown in the tenant: `NOT_FOUND` | — |
| after_seq | String, integer | No | Return entries with `seq` above this value. Default 0. | `18220` |
| types | Array of enums | No | Filter by entry type | `["TRADE"]` |
| occurred_from, occurred_to | Timestamp | No | Filter by operation time: from inclusive, to exclusive | — |
| page_size | Integer | No | Default 100, maximum 500 | `100` |

##### Response parameters
###### Response body example
```json
{
  "entries": [
    {
      "seq": "18221", "connection_id": "0c9e6a1e-…", "type": "TRADE", "leg": "BASE",
      "direction": "IN", "asset": "BTC", "native_asset": "BTC", "amount": "0.0125",
      "group_id": "trades:BTCUSDT:3491027", "external_id": "BTCUSDT:3491027",
      "occurred_at": "2026-10-01T08:12:44Z"
    },
    {
      "seq": "18222", "connection_id": "0c9e6a1e-…", "type": "TRADE", "leg": "QUOTE",
      "direction": "OUT", "asset": "USDT", "native_asset": "USDT", "amount": "1493.75",
      "group_id": "trades:BTCUSDT:3491027", "external_id": "BTCUSDT:3491027",
      "occurred_at": "2026-10-01T08:12:44Z"
    },
    {
      "seq": "18223", "connection_id": "5b7d2c90-…", "type": "CARD_DEBIT", "leg": "SINGLE",
      "direction": "OUT", "asset": "USDC", "native_asset": "0x7a1c…e9b2", "amount": "29.866387",
      "group_id": "logs:0x4be1…c7a2:3", "external_id": "0x4be1…c7a2:3",
      "occurred_at": "2026-10-02T12:00:00Z"
    }
  ],
  "last_seq": "18223",
  "has_more": false
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| entries[].seq | String, integer | Yes | Position in the order of arrival. Grows only. | `18221` |
| entries[].type | Enum | Yes | `DEPOSIT`, `WITHDRAWAL`, `TRADE`, `FEE`, `CONVERT`, `REWARD`, `CARD_DEBIT`, `CARD_REFUND` | `TRADE` |
| entries[].leg | Enum | Yes | `SINGLE`, `BASE`, `QUOTE`, `FEE` | `BASE` |
| entries[].direction | Enum | Yes | `IN` or `OUT`, from the account's point of view | `IN` |
| entries[].asset | String | Yes | Canonical asset | `BTC` |
| entries[].native_asset | String | Yes | Code or token address at the source | `BTC` |
| entries[].amount | String, decimal | Yes | Always positive | `0.0125` |
| entries[].group_id | String | Yes | Same for all legs of one operation: stream family + `external_id` | — |
| entries[].external_id | String | Yes | ID at the source | — |
| entries[].occurred_at | Timestamp | Yes | Time at the source | — |
| last_seq | String, integer | Yes | Highest `seq` in the response; next `after_seq`. Empty response: the `after_seq` of the request | `18223` |
| has_more | Boolean | Yes | More entries exist after `last_seq` | `false` |

- Order: ascending `seq`. A backfilled old operation gets a new, higher `seq`, so an incremental consumer never misses it.
- Entries are immutable. Only final operations of the source are imported (ADR-5).
- The raw source payload is stored but not returned by this method.
- No `page_token`: the consumer continues with `after_seq` = `last_seq`.
- Filters combine with AND. A negative `after_seq`, an unknown value in `types`, or an `owner_ref` longer than 128 characters: `INVALID_ARGUMENT`.

#### 2.1.5 RegisterCard

##### Description
Binds a processor card reference to a wallet connection. `card-auth` reads this registry on every authorization.

##### Endpoint
`cas.v1.CardService/RegisterCard`

##### Authorization
See Common rules.

##### Request parameters
###### Request example
```json
{
  "card_ref": "card_7Q2M",
  "owner_ref": "user-4821",
  "connection_id": "5b7d2c90-…",
  "daily_limit": "200000000"
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| card_ref | String, ≤ 64 | Yes | Opaque card reference from the processor. Unique per tenant. No card number. | `card_7Q2M` |
| owner_ref | String | Yes | 1 to 128 characters; must equal the owner of the connection | `user-4821` |
| connection_id | String, UUID | Yes | An `ACTIVE` or `DEGRADED` connection of kind `EVM_WALLET` | — |
| daily_limit | String, integer | Yes | Card daily limit in base units of the funding token | `200000000` |

##### Response parameters
###### Response body example
```json
{
  "card_ref": "card_7Q2M",
  "owner_ref": "user-4821",
  "connection_id": "5b7d2c90-…",
  "wallet_address": "0x9aF3…41cE",
  "status": "ACTIVE",
  "daily_limit": "200000000"
}
```

| Parameter | Type | Required | Description | Example |
|---|---|---|---|---|
| status | Enum | Yes | `ACTIVE` or `FROZEN` | `ACTIVE` |
| wallet_address | String | Yes | Address of the bound wallet | — |

- The same request repeated returns the existing card. A different body for a known `card_ref` → `ALREADY_EXISTS`.
- `card_ref`: 1 to 64 characters. `daily_limit`: a base-unit integer string ≥ 0, at most 78 digits; no sign, no decimal point, no exponent. Otherwise `INVALID_ARGUMENT`. Leading zeros are accepted and removed: `"0200"` is stored and returned as `"200"`, and a repeated `RegisterCard` compares the stored value.
- The response also carries `created_at` and `updated_at`.
- `FAILED_PRECONDITION / CONNECTION_NOT_USABLE`: the connection is not a wallet, is not `ACTIVE` or `DEGRADED`, or its owner is not `owner_ref` (EC-113).
- Several cards may share one wallet. The wallet daily limit in the contract caps them together (SRS — Card Spend §2.1.5).
- The wallet daily limit is set on-chain by the admin, not by this method.
- There is no method that removes a card: a card is frozen instead, and a connection with cards cannot be deleted (EC-114). Audit `CARD_REGISTERED` with the acting credential.
- Proof of wallet ownership and the KYC status check are design only (US-1).

---

### 2.2 User Interface

N/A — no UI. Administration goes through the CLI and the gRPC API.

---

### 2.3 Use Case

#### 2.3.1 UC-101 Create a connection (C1)

##### Sequence diagram
N/A — one request, one optional call to the source; the steps are in the algorithm.

##### Algorithm

| # | Step | On failure |
|---|---|---|
| 1 | Authenticate, resolve the tenant, validate | `UNAUTHENTICATED`, `INVALID_ARGUMENT` |
| 2 | Check that the source exists and is available (Common rules) | `NOT_FOUND`, `FAILED_PRECONDITION / SOURCE_DISABLED` |
| 3 | Exchange: call the connector's key check. It returns the account ID and the permissions. The call waits for the rate limiter of the source at most 10 s | Key rejected: `KEY_INVALID`. Source unreachable, rate limit, or no budget within the wait: `UNAVAILABLE`. Another failure: `INTERNAL` |
| 4 | Exchange: reject a key with any permission beyond reading | `KEY_NOT_READ_ONLY` |
| 5 | Wallet: validate the address format and checksum | `INVALID_ARGUMENT` |
| 6 | Check uniqueness of `(tenant, source, account)`: exchange account ID or wallet address | `ALREADY_EXISTS` |
| 7 | Exchange: encrypt key and secret; keep the fingerprint | `INTERNAL` |
| 8 | Insert the connection as `ACTIVE`, create a cursor per stream the connector declares, write the audit record | `INTERNAL` |
| 9 | Schedule the first sync at once; respond | — |

##### Preconditions
- The tenant exists and holds a valid service token.

##### Trigger
`CreateConnection`.

##### Basic Flow
Steps 1–9.

##### Connection states

| State | Meaning | Sync |
|---|---|---|
| `ACTIVE` | Usable | Runs |
| `DEGRADED` | `failure_threshold` consecutive failures on a stream | Runs with backoff |
| `CREDENTIALS_INVALID` | The source rejected the key, or the key is no longer read-only | Stopped until the connection is replaced |

- `ACTIVE` and `DEGRADED` are set after every run of a stream, in the transaction that stores its health: `DEGRADED` while any stream of the connection has `consecutive_failures` ≥ `failure_threshold`, otherwise `ACTIVE`.
- `DEGRADED` is a label. Sync, reads, `TriggerSync` and the key check work as for `ACTIVE`.
- `CREDENTIALS_INVALID` is final: the rule above does not change it.
- Every change of the status is written to the audit log (FR-117).

##### Exception Paths

| EC | Case | Handling |
|---|---|---|
| EC-101 | Key with trade, withdrawal or transfer permission | Step 4: rejected; no connection and no key are stored |
| EC-102 | Same account or address registered again | Step 6: `ALREADY_EXISTS` |
| EC-103 | The source cannot report key permissions | The connector declares it; the connection is created with `permissions: ["UNVERIFIED"]` and the audit record says so |
| EC-104 | Source unreachable, or a rate-limit answer, during the key check | Step 3: `UNAVAILABLE`, nothing stored |

##### Acceptance Criteria

| ID | Requirement | Parent |
|---|---|---|
| FR-101 | A tenant must be able to create, list, read and delete connections of both kinds. | BR-1 |
| FR-102 | An exchange key with any permission beyond reading must be rejected and not stored. | BR-2 |
| FR-103 | Key and secret must be stored only encrypted; responses, logs and errors must contain at most the fingerprint. | BRD §9.2 Security |
| FR-104 | One exchange account or wallet address must be connected at most once per tenant. | BR-1 |
| FR-105 | Every request must be limited to the caller's tenant; another tenant's resource must look non-existent. | BR-13 |

##### Postconditions
- The connection exists with one cursor per declared stream; its first sync is scheduled. A connector that declares no stream leaves the connection without sync: EVM wallets until S3.

#### 2.3.2 UC-102 Sync a stream (C1)

##### Sequence diagram
See §2.1.1.

##### Algorithm

| # | Step | On failure |
|---|---|---|
| 1 | Every `SYNC_TICK` the scheduler picks the streams whose `next_run_at` has come | — |
| 2 | Reserve the request cost in the source's rate limiter; wait if the budget is spent | — |
| 3 | The connector fetches one page from the cursor | Step 8 |
| 4 | The connector maps native records to canonical entries or a snapshot; native asset codes are resolved through `asset_aliases` | An unknown code is kept as the canonical code and counted |
| 5 | Ledger stream: in one transaction insert the entries, skip those whose idempotency key exists, move the cursor; with a balance checkpoint, store its gaps (Connector contract) | Rollback; step 8 |
| 6 | Balance stream: insert a new snapshot with its balances; a snapshot without balances is stored too | Rollback; step 8 |
| 7 | More pages → step 2, up to `max_pages_per_run` pages in one run. Otherwise set `last_success_at`, zero the failure counter, set `next_run_at`. A run that stops at the page limit is a success too, and its stream is due at once, behind the streams that were already due | — |
| 8 | Failure: store `last_error`, increase the failure counter, set `next_run_at` with exponential backoff | — |

- The streams of one connection run one after another. Connections run in parallel, at most `SYNC_WORKERS` at a time.
- A stream gets its interval, first mode and first cursor from the connector (Connector contract).
- A cursor of a stream that the connector no longer declares is kept and not run.
- The periodic key check covers every `ACTIVE` and `DEGRADED` connection that has a key. It passes through the limiter of the source.

##### Preconditions
- The connection is `ACTIVE` or `DEGRADED`; its tenant is `ACTIVE`; its source is available (Common rules). While one of these does not hold, the streams of the connection are not run and no failure is counted; they continue from their cursors afterwards.

##### Trigger
Timer per stream; `TriggerSync`; connection created; an event of the source (ADR-13).

##### Basic Flow
Steps 1–7.

##### Stream modes

| Mode | Cursor moves | Ends when |
|---|---|---|
| `BACKFILL` | Through history, window by window: backwards from now for exchanges, forwards from `backfill_floor` for EVM networks | The other end of history is reached; the stream switches to `INCREMENTAL` |
| `INCREMENTAL` | Forwards from the newest known record | Never |

##### Exception Paths

| EC | Case | Handling |
|---|---|---|
| EC-105 | The same record arrives again (overlapping window, retry, restart) | Step 5: skipped by the idempotency key |
| EC-106 | The service stops in the middle of a backfill | The cursor moved only with committed pages; the next run continues from it |
| EC-107 | The source answers "rate limit exceeded" | The limiter pauses the affected budget for the time the source demands; no retry before that. The run counts as failed (step 8); its next run is not before the end of the pause |
| EC-108 | The source rejects the key, during a sync or the periodic key check | Connection → `CREDENTIALS_INVALID`; all its streams stop; audit record |
| EC-109 | Unknown native asset code | Step 4: imported under its native code; metric and log |
| EC-110 | An operation at the source is not final yet | Not imported; picked up by a later run once final |
| EC-111 | A stream keeps failing | After `failure_threshold` failures the connection is `DEGRADED`; reads keep returning the last snapshot, stale by the age rule of §2.1.3. The first success of the stream returns the connection to `ACTIVE`, unless another stream is over the threshold |
| EC-112 | `TriggerSync` called again inside the cooldown | `RESOURCE_EXHAUSTED` |
| EC-116 | The key gains a trade, withdrawal or transfer permission after the connection was created | Found by the periodic key check; connection → `CREDENTIALS_INVALID`; audit record |
| EC-117 | The connector returns an invalid entry: amount not positive, more than 18 decimal places or 20 integer digits, empty `external_id`, unknown type, leg or direction. Or an invalid balance (Connector contract, Snapshot) | The page or the snapshot is refused as a whole: nothing of it is stored, the previous snapshot stays the latest; step 8 |
| EC-118 | The source cannot be reached, or answers with a rate limit, during the periodic key check | The status does not change; the check is repeated with the backoff of step 8. The count of repeats is kept in memory: after a restart the check runs at once |
| EC-119 | A connector declares a stream that an existing connection does not have | The engine creates the missing cursor when it starts; the stream is due at once |

##### Acceptance Criteria

| ID | Requirement | Parent |
|---|---|---|
| FR-106 | A repeated sync must add no duplicate ledger entries. | BR-4 |
| FR-107 | Ledger entries and the cursor must be committed in the same transaction. | BR-4 |
| FR-108 | After a restart a stream must continue from its stored cursor. | BRD §9.2 Reliability |
| FR-109 | When a sync fails, the last snapshot must still be returned; it must be marked stale once the last successful run of the balance stream is older than `stale_after`. | US-204, BR-3 |
| FR-110 | All requests to one source must pass through one rate limiter; the limiter must exist before the first live call. | BRD §9.2 Rate limits |
| FR-111 | Only final operations must be imported; a ledger entry must never be updated or deleted by a sync. | BR-4 |
| FR-112 | A new source must be added as a connector and seed data, with no change to the engine, the ledger schema or the API. | US-208, US-213, BR-5 |
| FR-113 | The raw source record must be stored with every ledger entry. | BRD §9.2 Audit |
| FR-114 | Ledger entries must receive `seq` in commit order, so a consumer reading `after_seq` misses nothing. | US-209, BR-4 |
| FR-119 | Key permissions must be re-checked every `key_check_interval`; a key that is no longer read-only must stop the connection. | BR-2 |
| FR-120 | A consumer must be able to start the sync of a connection on demand, at most once per `trigger_sync_cooldown`. | US-210, BR-3 |
| FR-122 | A stream that a connector declares for an existing connection must get its cursor when the engine starts, without a change to the connection. | US-213, BR-5 |

##### Postconditions
- New entries or a new snapshot are stored; the cursor and health of the stream are up to date.

#### 2.3.3 UC-103 Manage a card (S2)

##### Sequence diagram
N/A — database operations only.

##### Algorithm

| # | Operation | Rules |
|---|---|---|
| 1 | Register | §2.1.5 |
| 2 | Change the daily limit | New value applies to the next authorization |
| 3 | Freeze, unfreeze | `ACTIVE` ↔ `FROZEN`; a frozen card is declined by `card-auth` |
| 4 | Read authorizations | `GetAuthorization`, `ListAuthorizations` (§2.1.1): data of [SRS — Card Spend](card-spend.md) §2.4, read-only, with the role `cas_server` |

##### Preconditions
- An `ACTIVE` or `DEGRADED` wallet connection of the same tenant and owner.

##### Trigger
`CardService` methods, called by the partner backend or the CLI.

##### Basic Flow
Row 1, then `card-auth` uses the card.

##### Exception Paths

| EC | Case | Handling |
|---|---|---|
| EC-113 | The connection is not a wallet, not `ACTIVE` or `DEGRADED`, or belongs to another owner | `FAILED_PRECONDITION` |
| EC-114 | Delete a connection that has cards | `FAILED_PRECONDITION / CONNECTION_HAS_CARDS`; nothing is deleted. There is no card removal: such a connection stays |
| EC-115 | Change a card while an authorization is in progress | The authorization keeps the values it read; the change applies to the next one |

##### Acceptance Criteria

| ID | Requirement | Parent |
|---|---|---|
| FR-115 | A partner must be able to register a card for a wallet, change its daily limit, and freeze and unfreeze it. | US-6, US-7 |
| FR-116 | A partner must be able to read the status and history of its authorizations. | US-8 |
| FR-117 | Every change of a connection or a card must be written to the audit log with the acting credential. | BRD §9.2 Audit |

FR-117 applies to connections from C1. A change made by the engine or the CLI has no acting credential: `credential_id` is empty.

##### Postconditions
- The card registry reflects the change; an audit record exists.

#### 2.3.4 UC-104 Delete a connection (C1)

##### Sequence diagram
N/A — database operations only.

##### Algorithm

| # | Step | On failure |
|---|---|---|
| 1 | Check that no card references the connection, in the same transaction as step 2. From S2: the card registry does not exist before | `FAILED_PRECONDITION / CONNECTION_HAS_CARDS` (EC-114) |
| 2 | In one transaction: delete the connection, its secret, cursors, snapshots and ledger entries; write the audit record | Rollback |
| 3 | Running sync of the connection stops at its next step | — |

##### Preconditions
- The connection belongs to the caller's tenant.

##### Trigger
`DeleteConnection`.

##### Basic Flow
Steps 1–3.

##### Exception Paths
- EC-114, above.

##### Acceptance Criteria

| ID | Requirement | Parent |
|---|---|---|
| FR-118 | After deletion no secret, balance or ledger entry of the connection must remain readable. | US-205, BR-1 |

##### Postconditions
- Only the audit record of the deletion remains.
- Consumers learn about the removal from `ListConnections`; entries they already pulled are theirs to drop.

#### 2.3.5 UC-105 Manage tenants and credentials (C1)

##### Sequence diagram
N/A — database operations only.

##### Algorithm

| # | Operation | Rules |
|---|---|---|
| 1 | Create a tenant | `casctl tenant create`. The name is unique: 1 to 64 characters of `a-z`, `0-9`, `-` and `_`, the first one a letter or a digit. Another name is refused and nothing is stored. Status `ACTIVE` |
| 2 | Disable, enable a tenant | `casctl tenant disable`, `enable`: `ACTIVE` ↔ `DISABLED`. While disabled: every request is `UNAUTHENTICATED`, the sync of its connections stops, its data stays |
| 3 | Issue a service token | `casctl token issue`. Format `cas_<key_id>_<secret>`: `key_id` is 12 hexadecimal characters, `secret` is 32 random bytes as 64 hexadecimal characters. The token is printed once; `api_credentials` keeps `key_id` and the SHA-256 hash of the 32 secret bytes |
| 4 | Revoke a token | `casctl token revoke`: sets `revoked_at`. The next request with the token is `UNAUTHENTICATED` |
| 5 | List | `casctl tenant list`, `casctl token list`, `casctl processor list [<tenant>]`: the processor credentials of every tenant or of one, with username, tenant, created and revoked times. No secret and no hash is shown |
| 6 | Issue a processor credential, from S2 | `casctl processor issue <tenant>`: a Basic pair for the processor API of `card-auth` (SRS — Card Spend §2.1.1). Username = `key_id`, 12 hexadecimal characters; password = 32 random bytes as 64 hexadecimal characters. Printed once as `username:password`; `api_credentials` keeps `kind = PROCESSOR_BASIC`, `key_id` and the SHA-256 hash of the 32 password bytes. Audit `CREDENTIAL_ISSUED` |
| 7 | Revoke a processor credential, from S2 | `casctl processor revoke <username>`: sets `revoked_at`; the next request with the pair is `401`. Audit `CREDENTIAL_REVOKED`. A revoked pair again: `nothing changed`, no audit row. An unknown username: an error |
| 8 | Set a value of an EVM source, from S3 | `casctl source set <source> <key>=<value>`: keys `controller_address` (an address, UC-301 rules) and `backfill_floor` (a block number ≥ 0) write `sources.config`, other keys of it are kept (S3 D-17); key `token_address` (an address, UC-301 rules) writes the `asset_aliases` row of the tracked token of the source: `asset` `USDC`, `decimals` 6, as the seed data of SRS — EVM Connector §2.4, replacing the row of an earlier token address (S3 D-27). Prints the previous and the new value. No audit row: `sources` and `asset_aliases` are not tenant data |
| 9 | Name the treasury connection, from S3 | `casctl source set-treasury <source> <connection_id>`: the connection must exist, be of kind `EVM_WALLET` and belong to that source; its ID is written to `treasury_connection` and its wallet address, in EIP-55 form, to `treasury_address` of the source, in one transaction; values already there are replaced and printed (S3 D-2, S3 D-32). No audit row, as row 8 |
| 10 | Reconcile on demand, from S3 | `casctl reconcile <source>`: runs SRS — Card Spend UC-4 for the source in the process of `casctl`, with the owner role, and stores the runs as the worker does (S3 D-8). Prints one line per run: tenant, run ID, count of mismatches by type. Exit code 0 when the runs are stored, mismatches included; not 0 when nothing could be stored |

- Authentication of a request: the credential is found by `key_id`; the SHA-256 hash of the presented secret is compared in constant time.
- A tenant may hold several valid tokens at a time: a new one is issued, the consumer switches, the old one is revoked. A token is never shared between tenants or services (ADR-7).
- `casctl` connects with the owner role of the database (§3.2). Its connection string is the variable `CASCTL_DATABASE_URL`.

##### Preconditions
- The operator has the owner role of the database.

##### Trigger
`casctl`, run by the operator.

##### Basic Flow
Rows 1 and 3; the token is handed to the consuming system.

##### Exception Paths

| EC | Case | Handling |
|---|---|---|
| EC-120 | A tenant with this name exists | Refused; nothing is stored |
| EC-121 | A request carries a token of a disabled tenant | `UNAUTHENTICATED`, as for an unknown token |
| EC-122 | `casctl source set` or `set-treasury`: unknown source, source not of kind `EVM`, a key outside the list, a malformed value, an unknown connection, a connection of another kind or source | Refused with a message; nothing is changed |
| EC-123 | `casctl reconcile`: the source has no `treasury_connection` | Refused with a message; no run is stored (SRS — EVM Connector §2.1.1) |

##### Acceptance Criteria

| ID | Requirement | Parent |
|---|---|---|
| FR-121 | Every change of a tenant or a credential must be written to the audit log. | BRD §9.2 Audit |

##### Postconditions
- The tenant and its credentials reflect the change; an audit record exists.

---

### 2.4 Data Model

#### 2.4.1 Core

##### Data model schema

```mermaid
erDiagram
    tenants ||--o{ api_credentials : "authenticates"
    tenants ||--o{ connections : "owns"
    sources ||--o{ connections : "hosts"
    sources ||--o{ asset_aliases : "maps"
    connections ||--o{ sync_cursors : "streams"
    connections ||--o{ balance_snapshots : "balances"
    balance_snapshots ||--o{ snapshot_balances : "holds"
    connections ||--o{ ledger_entries : "history"
    connections ||--o{ cards : "funds"
    tenants ||--o{ audit_log : "records"
    tenants ||--o{ reconciliation_runs : "reports"
    sources ||--o{ reconciliation_runs : "reconciles"
    connections ||--o{ balance_checkpoints : "checks"
```

`cards` is the parent of `authorizations` in SRS — Card Spend §2.4.

##### tenants
###### Description
A consuming system. Managed through the CLI (UC-105).

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| name | TEXT | Yes | Unique. Format: UC-105 |
| status | TEXT | Yes | `ACTIVE`, `DISABLED` |
| created_at | TIMESTAMPTZ | Yes | — |

##### api_credentials
###### Description
Service tokens for gRPC and Basic credentials for the processor API of `card-auth`. Only hashes are stored.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| tenant_id | UUID | Yes | Tenant |
| kind | TEXT | Yes | `SERVICE_TOKEN`, `PROCESSOR_BASIC` |
| key_id | TEXT | Yes | `SERVICE_TOKEN`: the `key_id` part of the token. `PROCESSOR_BASIC`: username. Unique. |
| secret_hash | BYTEA | Yes | `SERVICE_TOKEN`: SHA-256 of the secret part. `PROCESSOR_BASIC`: SHA-256 of the 32 password bytes (UC-105, row 6) |
| created_at | TIMESTAMPTZ | Yes | — |
| revoked_at | TIMESTAMPTZ | No | Set when revoked |

##### sources
###### Description
Exchanges and EVM networks. Business configuration lives here; system configuration lives in the environment. The migrations of C1 add the EVM networks (SRS — EVM Connector §2.4); an exchange is added with its connector. The row `fake` of the fake connector is a development seed, not a migration: `casctl source add-fake` adds it as an enabled `EXCHANGE` source with the aliases of its two assets; a second run changes nothing. From S3, `casctl source set` and `source set-treasury` change single values of `config` of an EVM source (UC-105).

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | SMALLINT | Yes | Primary key |
| code | TEXT | Yes | Unique: `binance`, `base-sepolia` |
| kind | TEXT | Yes | `EXCHANGE`, `EVM` |
| enabled | BOOLEAN | Yes | Offered to tenants |
| config | JSONB | Yes | Base URL or chain ID, sync intervals, rate-limit settings, backfill floor |

##### connections
###### Description
One account at one source.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| tenant_id | UUID | Yes | Tenant |
| owner_ref | TEXT | Yes | Opaque owner ID |
| source_id | SMALLINT | Yes | Source |
| external_account | TEXT | Yes | Exchange account ID or wallet address. Unique with `tenant_id` and `source_id`. |
| label | TEXT | No | Display name |
| status | TEXT | Yes | §2.3.1 |
| credentials_enc | BYTEA | No | Envelope-encrypted key and secret (ADR-4). Null for wallets. |
| kek_version | SMALLINT | No | Master key version used |
| key_fingerprint | TEXT | No | Last 4 characters of the API key |
| permissions | TEXT[] | No | As reported by the source |
| permissions_checked_at | TIMESTAMPTZ | No | Last key check |
| last_manual_sync_at | TIMESTAMPTZ | No | Last accepted `TriggerSync` |
| created_at | TIMESTAMPTZ | Yes | — |

##### asset_aliases
###### Description
Native asset code or token address of a source → canonical asset.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| source_id | SMALLINT | Yes | Primary key, part 1 |
| native_asset | TEXT | Yes | Primary key, part 2. Code or token address. |
| asset | TEXT | Yes | Canonical code |
| decimals | SMALLINT | No | For tokens: base-unit exponent |

##### sync_cursors
###### Description
Position and health of one stream of one connection.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| connection_id | UUID | Yes | Primary key, part 1 |
| stream | TEXT | Yes | Primary key, part 2. Full stream name: `balances`, `deposits`, `trades:BTCUSDT`, `logs` |
| mode | TEXT | Yes | `BACKFILL`, `INCREMENTAL` |
| cursor | JSONB | Yes | Connector-defined: last ID, time window, block number |
| next_run_at | TIMESTAMPTZ | Yes | When the stream is due |
| last_success_at | TIMESTAMPTZ | No | Last successful run |
| last_error | TEXT | No | Last error, without secrets, at most 500 characters |
| consecutive_failures | INTEGER | Yes | Zeroed on success |

##### balance_snapshots
###### Description
Append-only. One row per successful run of the balance stream, also when the account holds nothing. The latest row of a connection is its current snapshot.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| connection_id | UUID | Yes | Connection |
| taken_at | TIMESTAMPTZ | Yes | Snapshot time, reported by the connector |
| created_at | TIMESTAMPTZ | Yes | Import time. Orders the snapshots of a connection |

##### snapshot_balances
###### Description
Append-only. The balances of one snapshot.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| snapshot_id | UUID | Yes | Primary key, part 1. Snapshot |
| account_type | TEXT | Yes | Primary key, part 2 |
| native_asset | TEXT | Yes | Primary key, part 3 |
| asset | TEXT | Yes | Canonical code |
| free | NUMERIC(38,18) | Yes | Available |
| locked | NUMERIC(38,18) | Yes | In orders or lock-up |

##### ledger_entries
###### Description
Append-only canonical ledger. Idempotency key: `(connection_id, stream, external_id, leg)`.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| seq | BIGINT | Yes | Primary key. Assigned by the single ledger writer in commit order. |
| tenant_id | UUID | Yes | Tenant |
| connection_id | UUID | Yes | Connection |
| stream | TEXT | Yes | Stream family that produced the entry, not the full stream name: `trades`, `deposits`, `logs` |
| external_id | TEXT | Yes | ID at the source: `BTCUSDT:3491027`, `tx_hash:log_index` |
| leg | TEXT | Yes | `SINGLE`, `BASE`, `QUOTE`, `FEE` |
| group_id | TEXT | Yes | Same for all legs of one operation |
| type | TEXT | Yes | §2.1.4 |
| direction | TEXT | Yes | `IN`, `OUT` |
| asset | TEXT | Yes | Canonical code |
| native_asset | TEXT | Yes | Code or token address at the source |
| amount | NUMERIC(38,18) | Yes | Positive |
| occurred_at | TIMESTAMPTZ | Yes | Time at the source |
| raw | JSONB | Yes | Source record as received |
| created_at | TIMESTAMPTZ | Yes | Import time |

##### cards
###### Description
Card registry read by `card-auth`.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| tenant_id | UUID | Yes | Tenant |
| card_ref | TEXT | Yes | Unique with `tenant_id` |
| owner_ref | TEXT | Yes | Equals the owner of the connection |
| connection_id | UUID | Yes | Wallet connection. `ON DELETE RESTRICT`: the second line of EC-114; a card registered while its connection is deleted fails the delete with `CONNECTION_HAS_CARDS` |
| status | TEXT | Yes | `ACTIVE`, `FROZEN` |
| daily_limit | NUMERIC(78,0) | Yes | Base units of the funding token |
| created_at | TIMESTAMPTZ | Yes | — |
| updated_at | TIMESTAMPTZ | Yes | — |

##### audit_log
###### Description
Append-only record of changes made through the API or the CLI, and of the status changes the engine makes.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | BIGINT | Yes | Primary key, generated |
| tenant_id | UUID | Yes | Tenant |
| credential_id | UUID | No | Acting credential; null for the CLI and the engine |
| action | TEXT | Yes | Connections: `CONNECTION_CREATED`, `CONNECTION_DELETED`, `CONNECTION_DEGRADED`, `CONNECTION_RECOVERED`, `CREDENTIALS_INVALID`. Tenants and credentials: `TENANT_CREATED`, `TENANT_DISABLED`, `TENANT_ENABLED`, `CREDENTIAL_ISSUED`, `CREDENTIAL_REVOKED`. Cards, from S2: `CARD_REGISTERED`, `CARD_UPDATED` |
| object_id | TEXT | Yes | Connection ID, tenant ID, `key_id` or `card_ref` |
| details | JSONB | No | Changed fields; no secrets |
| created_at | TIMESTAMPTZ | Yes | — |

##### reconciliation_runs
###### Description
One row per reconciliation run of one tenant on one source (SRS — Card Spend UC-4). Milestone S3. Append-only; every run is stored (S3 D-9); retention: `docs/backlog.md`.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| id | UUID | Yes | Primary key |
| tenant_id | UUID | Yes | Tenant |
| source_id | SMALLINT | Yes | Source (S3 D-6) |
| period_from | TIMESTAMPTZ | Yes | Start of the checked period |
| period_to | TIMESTAMPTZ | Yes | End of the checked period: `last_time` of the treasury's `logs` cursor (S3 D-7) |
| to_block | BIGINT | Yes | `next_block − 1` of the treasury's `logs` cursor |
| totals | JSONB | Yes | Counts and sums of debits and refunds: the fields of `totals` of §2.1.1 Reconciliation run |
| mismatches | JSONB | Yes | List of mismatches: the fields of §2.1.1 Reconciliation run |
| created_at | TIMESTAMPTZ | Yes | — |

##### balance_checkpoints
###### Description
The last balance checkpoint per connection and native asset, with its gap (Connector contract; SRS — EVM Connector UC-304). Milestone S3 (S3 D-5). A new checkpoint replaces the row. Removed only by the cascade of a deleted connection.

###### Data model

| Name | Type | Required | Description |
|---|---|---|---|
| connection_id | UUID | Yes | Primary key, part 1 |
| native_asset | TEXT | Yes | Primary key, part 2 |
| asset | TEXT | Yes | Canonical code |
| block_number | BIGINT | No | EVM: the block of the checkpoint |
| block_hash | TEXT | No | EVM: its hash |
| taken_at | TIMESTAMPTZ | Yes | Time of the checkpoint at the source: the block time on EVM |
| balance | NUMERIC(38,18) | Yes | Balance at the source, not negative |
| ledger_total | NUMERIC(38,18) | Yes | Σ `IN` − Σ `OUT` of the connection for the asset. A difference, so it may be negative; not a ledger amount |
| gap | NUMERIC(38,18) | Yes | `balance − ledger_total`; 0 when complete |
| checked_at | TIMESTAMPTZ | Yes | When the row was written |

---

### 2.5 Metrics and Alerts

#### 2.5.1 Metrics

- Exposed in the Prometheus text format at `/metrics` on the health port.
- Every metric of the table exists from C1, except `ledger_gap` from S3.

| Service | Metric name | Value | Alert | Description | Requestor |
|---|---|---|---|---|---|
| server | `sync_runs_total{source,stream,result}` | — | Failure share > 20% for 15 min | Sync runs by outcome: `success` or `failure`. `stream` is the stream family | BR-3 |
| server | `sync_staleness_seconds{source}` | < `stale_after` | Above `stale_after` | Now minus the oldest `last_success_at` among the streams the engine runs. A stream that never succeeded is not counted; 0 without streams | BR-3 |
| server | `connections{status}` | — | Any `CREDENTIALS_INVALID` | Connections by state | BR-1 |
| server | `ledger_entries_inserted_total{source}` | — | — | New entries | BR-4 |
| server | `ledger_duplicates_skipped_total{source}` | — | — | Records skipped by the idempotency key | BR-4 |
| server | `unmapped_assets_total{source}` | 0 | Any | Native codes without an alias | BR-4 |
| server | `rate_limit_wait_seconds{source}` | — | p95 > 30 s | Time spent waiting for the budget | BRD §9.2 |
| server | `rate_limit_rejections_total{source}` | 0 | Any | "Rate limit exceeded" answers from the source | BRD §9.2 |
| server | `grpc_request_seconds{method}` | Reads p95 ≤ 100 ms | p95 > 300 ms for 5 min | API latency. `method` is the service and the method: `ConnectionService/ListSources` | BR-3 |
| server | `ledger_gap{source,connection,asset}` | 0 | Any non-zero | Gap of the last balance checkpoint (Connector contract; SRS — EVM Connector UC-304). From S3 (S3 D-19) | BR-12 |

#### 2.5.2 Alerts
Conditions are in the Alert column above. Delivery channel: N/A — defined with the deployment.

---

## 3. Non-functional Requirements

### 3.1 Configuration

| Parameter | Where | Default | Meaning |
|---|---|---|---|
| `CAS_MASTER_KEY` | Environment | — | Key that wraps the data keys of exchange secrets: 32 bytes in base64. `server` does not start without it |
| `CAS_MASTER_KEY_VERSION` | Environment | 1 | Stored as `kek_version`. A secret of another version is not decrypted. Rotation is not built: `docs/backlog.md` |
| `DATABASE_URL` | Environment | — | PostgreSQL, role `cas_server` |
| Connection pool | Environment: `DB_POOL_MAX_CONNS`, `DB_POOL_MIN_CONNS` | 10, 2 | Database connections |
| gRPC port | Environment: `GRPC_PORT` | 50053 | Consumer API |
| Health port | Environment: `HEALTH_HTTP_PORT` | 8091 | `/healthz`, `/readyz`, `/metrics` |
| Logging | Environment: `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | Level and format of the log |
| Shutdown | Environment: `SHUTDOWN_TIMEOUT` | 15 s | Time for running requests and the current sync step to finish |
| `sync_interval` per stream family | `sources.config` | Balances 15 min, ledger 1 h | Period of a stream. Read by the connector and reported with each stream it declares |
| `stale_after` | `sources.config` | 2 × balance interval | Age of the last successful balance sync after which balances are stale |
| `backfill_floor` | `sources.config` | Per source | Earliest point a backfill reaches: a date or a block number; see the source's SRS |
| Rate-limit settings | `sources.config` | Per source | Defined in the source's SRS: `budget_share` for Binance, `rpc_rate_limit` for EVM networks |
| Scheduler tick | Environment: `SYNC_TICK` | 1 s | How often the scheduler looks for due streams |
| Workers | Environment: `SYNC_WORKERS` | 4 | Connections synced at the same time |
| `max_pages_per_run` | Environment: `SYNC_MAX_PAGES_PER_RUN` | 20 | Pages of a ledger stream fetched in one run (UC-102 step 7) |
| Engine lock retry | Environment: `SYNC_LOCK_RETRY` | 10 s | How often an instance without the engine lock tries to take it |
| `failure_threshold` | Environment: `SYNC_FAILURE_THRESHOLD` | 5 | Failures before `DEGRADED` |
| `backoff` | Environment: `SYNC_BACKOFF_INITIAL`, `SYNC_BACKOFF_MAX` | 30 s, doubling, cap 1 h | Delay after a failed run |
| `trigger_sync_cooldown` | Environment: `TRIGGER_SYNC_COOLDOWN` | 60 s | Minimum gap between manual syncs of a connection |
| `key_check_interval` | Environment: `KEY_CHECK_INTERVAL` | 24 h | Period of the key permission re-check |
| Fake source | Environment: `ENABLE_FAKE_SOURCE` | `false` | Registers the fake connector in a running `server`. Development and demo only |

- Values of `sources.config` are JSON; a duration is a string such as `15m` or `1h`: `{"sync_interval": {"balances": "15m", "ops": "1h"}, "stale_after": "30m"}`. A missing or malformed value takes its default. The default interval of a family other than `balances` is 1 h, unless the SRS of the source sets another: `logs` of an EVM network, 5 min (SRS — EVM Connector §3.1). Connectors and the read API take these values from the same place, so a read never asks a connector.
- `/healthz`: the process runs.
- `/readyz`: the database answers and its schema version is the one the binary expects. The state of the engine is not part of readiness.

### 3.2 General Non-functional Requirements

- **Parallel work:**
  - the sync engine runs in one `server` instance at a time, guarded by a database advisory lock: one process owns the rate budgets of each source. An instance without the lock serves the API and tries to take the lock every `SYNC_LOCK_RETRY`. An instance that loses the lock stops its runs and tries again;
  - API instances are stateless and can be many. C1 runs one instance: §4, issue 5;
  - the ledger has one writer, so `seq` order equals commit order (FR-114): every transaction that writes ledger entries first takes one database lock, so the entries of different connections are committed one after another;
  - connections sync concurrently, at most `SYNC_WORKERS` at a time; the streams of one connection run one after another; a stream never runs twice at once;
  - the reconciliation worker runs in the instance that holds the engine lock. Every `SYNC_TICK` it compares the `logs` cursor of the treasury connection of each source with `to_block` of the source's newest run; a moved cursor starts a run (SRS — Card Spend UC-4, S3 D-8). `casctl reconcile` may run beside it: two runs only store two rows.
- **Audit log:** `audit_log` for changes; `ledger_entries.raw` for imported data; no secrets in either.
- **Performance:** read methods p95 ≤ 100 ms: database only. Sync speed is bounded by the source's rate budget, not by CAS.
- **Reliability:**
  - a failed source never fails a read: last data plus the stale flag;
  - cursors move only with committed data (FR-107);
  - a rate-limit answer pauses the affected budget instead of retrying (EC-107).
- **Security:**
  - envelope encryption of exchange secrets; the master key only in the environment (ADR-4);
  - `credentials_enc` is one block: a format byte, the data key wrapped by the master key, and the key and secret encrypted by the data key. Both use AES-256-GCM with a random nonce; the data key is new for every encryption. The connection ID and `kek_version` are authenticated data. A block that fails a check is not decrypted, and the error says nothing about the cause;
  - service tokens and processor passwords stored as hashes;
  - tenant filter on every query (FR-105), covered by an automated test with two tenants;
  - database roles: `cas_server` for `server`, `cas_card_auth` for `card-auth` (S2). The operator creates a role once per environment; the migrations run under the owner role and grant the rights below;
  - `cas_server` reads `authorizations`, `authorization_events`, `returns` and the columns of `operator_txs` of the table below, and writes none of them; from S3 the reconciliation worker reads them; it has no right on `operator_accounts`; `cas_card_auth` reads `cards` but cannot write them; the operator key is not in the database;
  - `cas_card_auth` has no right on the column `connections.credentials_enc`: its `SELECT` on `connections` is granted per column, every column but that one;
  - `casctl` connects with the owner role: neither runtime role can create a tenant or a credential.

| Tables | Rights of `cas_server` |
|---|---|
| `connections` | `SELECT`, `INSERT`, `UPDATE`, `DELETE` |
| `sync_cursors` | `SELECT`, `INSERT`, `UPDATE`. Rows are removed only by the cascade of a deleted connection |
| `balance_snapshots`, `snapshot_balances`, `ledger_entries` | `SELECT`, `INSERT`. Rows are removed only by the cascade of a deleted connection |
| `cards`, from S2 | `SELECT`, `INSERT`, `UPDATE` |
| `authorizations`, `authorization_events`, `returns`, from S2 | `SELECT` |
| `operator_txs`, from S2 | `SELECT` on `authorization_id`, `return_row_id`, `purpose`, `status`, `tx_hash`, `created_at`; from S3 also `block_number`, for the eligibility of a return in reconciliation (S3 D-7) |
| `balance_checkpoints`, from S3 | `SELECT`, `INSERT`, `UPDATE`. Rows are removed only by the cascade of a deleted connection |
| `reconciliation_runs`, from S3 | `SELECT`, `INSERT`. Append-only |
| `audit_log` | `INSERT` |
| `tenants`, `api_credentials`, `sources`, `asset_aliases` | `SELECT` |
| `schema_migrations` | `SELECT`, for `/readyz` |

| Tables | Rights of `cas_card_auth`, from S2 |
|---|---|
| `authorizations`, `returns`, `operator_txs`, `operator_accounts` | `SELECT`, `INSERT`, `UPDATE` |
| `authorization_events` | `SELECT`, `INSERT`. Append-only: no `UPDATE`, no `DELETE` |
| The sequence of `authorization_events.id` | `USAGE` |
| `audit_log` | `INSERT` |
| `cards`, `tenants`, `api_credentials`, `sources` | `SELECT` |
| `connections` | `SELECT` on every column except `credentials_enc` |
| `schema_migrations` | `SELECT`, for `/readyz` |

- Neither role has `DELETE` on a card-auth table: an authorization and its history are never removed. A connection with cards cannot be deleted (EC-114), so no cascade reaches them.

- **Compliance:** no personal data. `owner_ref`, `card_ref` and labels are supplied by the tenant and must not contain any.

---

## 4. Open Issues

| # | Issue | Proposal |
|---|---|---|
| 1 | Tenant onboarding through the API | CLI only for now; an admin API when a second real tenant appears |
| 2 | Rotation of an exchange key without losing history | `RotateCredentials` method: same connection, new encrypted key; after C1 |
| 3 | Retention of balance snapshots | Keep everything for now; thinning of old snapshots later |
| 4 | Erasing all data of one owner | `DeleteOwnerData` method; after C1 |
| 5 | The key check of `CreateConnection` runs in the instance that serves the request. With several instances it does not pass the limiter of the engine instance (FR-110) | C1 runs one instance. Later: the engine instance makes the call for the API instance |
