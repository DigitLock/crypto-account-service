# Glossary

Terms as they are used in the CAS documents. One meaning per term.

## Card payments

| Term | Meaning |
|---|---|
| Card scheme | Network that routes card payments between acquirer and issuer: Visa, Mastercard. |
| Acquirer | The merchant's bank or payment provider. |
| Issuer | The party that issues the card and answers for its payments. |
| Issuer processor | Company that holds the scheme connection for the issuer and manages cards. It calls `card-auth` on every authorization. Short: processor. |
| JIT Funding | Pattern where the processor asks the program's backend to approve and fund each authorization in real time. |
| Authorization | Request to approve one card payment. Identified by `auth_id`. |
| Decision deadline | Time `card-auth` has to answer the processor: 2.5 s by default. |
| Reversal | Cancellation of an authorization, full or partial, before clearing. |
| Refund | Return of money after the purchase was completed. |
| Clearing | The final message with the amount actually charged. It can differ from the authorized amount. |
| Incremental authorization | Extra authorization that raises the amount of an existing one: hotels, car rental. |
| Chargeback | Dispute raised by the cardholder through the issuer. Handled off-chain. |
| Stand-in | Approval made on fixed rules when the normal decision path is unavailable. Not used in v1. |
| `card_ref` | Opaque reference to a card, given by the processor. CAS never sees a card number. |
| KYC | Identity check of the cardholder, done by an external provider. CAS consumes only its status. |

## On-chain

| Term | Meaning |
|---|---|
| Wallet (EOA) | Regular account controlled by one private key. |
| Smart account | Account that is a contract and can carry rules: limits, delays, modules. |
| Self-custody | The user holds the keys. Nobody else can move the funds without an on-chain permission from the user. |
| ERC-20 | Standard interface of tokens on EVM networks. |
| Allowance | Permission given by a wallet to another address to move up to a set amount of a token. Set with `approve`, used with `transferFrom`, revocable at any time. |
| `permit` | Allowance granted by a signature instead of a transaction (EIP-2612). |
| Base units | Integer token amount. 1 USDC = 1 000 000 base units. |
| Stablecoin | Token pegged to a fiat currency. The funding token of v1 is a mock of USDC. |
| Operator | Key of `card-auth` allowed to call `debit` and `refund` in the contract. |
| Admin | Separate key allowed to set limits and pause the contract. |
| Controller | The `CardSpendController` contract: moves tokens wallet → treasury for a debit and back for a refund. |
| Treasury | Address that receives debited tokens and funds refunds. CAS watches it as a connection of the platform tenant: the treasury connection. |
| Nonce | Sequence number of a sender's transactions. One operator key has one sequence. |
| Gas | Fee paid for a transaction in the network's native coin. The operator pays it. |
| Preconfirmation | Early signal from the block builder that a transaction will be in the next block. On Base it arrives about every 200 ms. It can still be dropped. A preconfirmed log or receipt carries a zero block hash. |
| Flashblocks | Base's preconfirmation mechanism: a partial block every 200 ms. Reached with the `pending` block tag and the WebSocket subscription `pendingLogs` of a Flashblocks-capable RPC provider. |
| Sealed block | Block that has been produced and published. On Base every 2 s. |
| Finality | Point after which a block cannot change. |
| Block tag | Named block in an RPC request: `latest`, `pending`, `safe`, `finalized`. |
| Final block | The newest block CAS reads on a network, set by the finality rule of that network. |
| L2 | Network that executes transactions itself and posts them in batches to a base network (L1) for security: Base on Ethereum. |
| Sequencer | The node that orders transactions and builds blocks on an L2 network such as Base. |
| Reorg | Replacement of recent blocks by others. A transaction in a replaced block may disappear. |
| Event log | Record emitted by a contract during a transaction: `Transfer`, `Debited`, `Refunded`. Its indexed fields are topics: log queries filter by them. |
| Tracked token | Token that CAS reads on a network: listed in the asset aliases of that network. |
| RPC provider | Service that gives access to the network: reads, transaction submission, subscriptions. |
| SIWE | Sign-In with Ethereum (EIP-4361): a signed message that proves control of a wallet. |
| Testnet | Network with valueless coins for testing. CAS uses Base Sepolia and a local Anvil chain. |

## Exchanges

| Term | Meaning |
|---|---|
| API key permissions | What a key may do at the exchange: read, trade, withdraw, transfer. CAS accepts read only. |
| Weight | Cost of one request in the exchange's rate limit. |
| IP budget | Weight limit counted per calling IP and shared by all connections. |
| UID budget | Weight limit counted per exchange account. |
| Spot, Funding, Earn | Wallets inside one exchange account. Earn holds assets placed in yield products, flexible or locked. |
| Wrapper asset | Code such as `LDUSDT`: a flexible Earn position shown in the spot wallet. |
| Trading pair | Two assets traded against each other, e.g. `BTCUSDT`. Trade history is read per pair. |
| Delisting | Removal of a trading pair by the exchange. Its trade history becomes unreachable through the API. |

## CAS

| Term | Meaning |
|---|---|
| Tenant | A consuming system: a partner, ET, the demo. Owns all its data. |
| Platform tenant | The platform's own tenant, `cas-platform`. Owns the treasury connection of each network and gets the reconciliation of the treasury. An ordinary tenant otherwise. |
| Owner | The tenant's user, known to CAS only as an opaque `owner_ref`. In the PRDs: account owner, cardholder. |
| Source | An exchange or an EVM network that CAS can read. |
| Connection | One account at one source: an exchange API key or a wallet address. |
| Connector | Code adapter of one source behind the common interface. |
| Stream | One kind of data of a connection with its own cursor: balances, deposits, trades of a pair, event logs. Streams of one kind form a family: `trades`. |
| Cursor | Stored position of a stream: last ID, time window or block number. |
| Backfill | Reading the history that existed before the connection was created: backwards from now for exchanges, forwards from the first block for EVM networks. |
| Lookback | Period re-read by incremental runs to catch records that became final late. |
| Snapshot | Balances of a connection at one moment. |
| Canonical asset | Asset code used across sources: `BTC`, `USDC`. The code or token address at the source is the native asset. |
| Stale | Mark on balances whose last successful sync is older than allowed. |
| Ledger entry | One immutable movement of one asset. |
| Leg | One of several entries that belong to the same operation: base, quote, fee. |
| Idempotency key | `(connection, stream, external ID, leg)`. An entry with a known key is not imported again. |
| `seq` | Position of a ledger entry in the order of arrival. Consumers pull "everything after `seq`". |
| Final record | A source record whose outcome and amounts will not change. Only final records are imported. |
| Completeness gap | Balance at the source minus the ledger sum of an asset (SRS — Core §2.4 `balance_checkpoints`). Shows history CAS could not read. |
| Balance checkpoint | Balances of a connection at the point its imported records end (EVM: the final block). Compared with the ledger sum to give the completeness gap; the last one is stored per connection and native asset. |
| Reconciliation run | One check of the authorizations, returns and treasury of one tenant on one network against the on-chain events. Stored with its period, totals and mismatches. |
| Debit | On-chain transfer wallet → treasury for one authorization. |
| Return | On-chain transfer treasury → wallet: reversal, refund, or automatic refund of a late debit. |
| Inclusion signal | Evidence that a debit executed: its `Debited` log or a successful receipt. |
| Late debit | Debit that lands after the processor was already told "declined". Returned automatically. |
| Lost debit | Approved debit that was dropped by the network and could not be repeated. Issuer exposure. |
| Tombstone | Record of a reversal that arrived before its authorization. Makes the late authorization decline. |
| Event listener | Component of `server` that holds a WebSocket connection to an exchange and turns account events into sync triggers. |
| Chain listener | Component of `card-auth` that subscribes to the `Debited` logs of the controller (`pendingLogs` or `logs`), never to new blocks, and reports inclusion signals. |
| Tracker | Background worker of `card-auth`: follows debits and returns to their final status. |
| Event indexer | The EVM connector of `server`: reads contract and token event logs from final blocks into the ledger. |
