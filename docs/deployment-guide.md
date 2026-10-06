# Deployment Guide — Base Sepolia

- **Version:** 0.1, 2026-10-06, S2 st9a: first version, written with the scripts of `scripts/sepolia/` and their local rehearsal. Values of Base Sepolia are placeholders until S2 st9b.
- **Status:** Draft.
- **Parents:** [SRS — Card Spend](srs/card-spend.md) §3.1, §3.2; [SRS — Core](srs/core.md) UC-105, `CreateConnection`, `RegisterCard`, §3.2; [test plan S2](test-plan-s2.md) phase 8.

---

## 1. What it covers

| Item | Scope |
|---|---|
| Network | Base Sepolia only, chain ID 84532. No mainnet: the scripts refuse any other chain ID, and the deployment script reverts on it (S2-T801) |
| Machine | The development machine of the owner. No VPS (package decision 1) |
| Contracts | `MockUSDC` and `CardSpendController`, deployed by `contracts/script/Deploy.s.sol` |
| Services | `server` (card registry, gRPC 50053) and `card-auth` (processor API 8092, health 8093), run locally |
| Rows | S2-T802 … S2-T809, run by the owner in st9b |

## 2. Accounts

| Role | Key in the keys file | Used for |
|---|---|---|
| `DEPLOYER` | `DEPLOYER_PRIVATE_KEY` | Broadcast of the deployment |
| `ADMIN` | `ADMIN_PRIVATE_KEY` | `setDailyLimit`, `pause`, `unpause`, `grantRole`, `revokeRole` with `cast` (§6) |
| `OPERATOR` | `OPERATOR_PRIVATE_KEY` | Debits and refunds; held only by `card-auth`, in its process environment |
| `TREASURY` | `TREASURY_PRIVATE_KEY` | Receives debits; approves the controller for refunds |
| `USER` | `USER_PRIVATE_KEY` | Cardholder wallet: mints `MockUSDC`, approves the controller |

- Five generated keys. Never a personal wallet, never a key that held anything of value.
- The keys file lies outside the repository; its path is the variable `CAS_SEPOLIA_KEYS`. Mode 600. Format: one `NAME=value` line per key, the value 64 hexadecimal characters, `0x` optional; lines starting with `#` are comments.
- Generation, once, in a directory outside the repository:

```sh
umask 077
mkdir -p ~/cas-sepolia
for role in DEPLOYER ADMIN OPERATOR TREASURY USER; do
  printf '%s_PRIVATE_KEY=0x%s\n' "$role" "$(openssl rand -hex 32)"
done > ~/cas-sepolia/keys.env
chmod 600 ~/cas-sepolia/keys.env
export CAS_SEPOLIA_KEYS=~/cas-sepolia/keys.env
```

- Address of a key: `cast wallet address --private-key "$(sed -n 's/^ADMIN_PRIVATE_KEY=//p' "$CAS_SEPOLIA_KEYS")"`. The shell history keeps the command, not the key.
- Not `cast wallet new`: it prints the private key to the terminal, so the key ends up in the scrollback and in any copy of it.
- `deploy.sh` prints the five addresses and their ETH balances.

## 3. Funding

| Item | Value |
|---|---|
| Source | CDP faucet: 0.0001 ETH per claim |
| Gas price, measured 2026-10-06 | 0.006 gwei |
| One ETH transfer, measured 2026-10-06 | 0.000000132 ETH, of which the L1 fee is about 5 % |
| Need of st9 | About 0.001 ETH in total |
| Who needs ETH | `DEPLOYER` (deployment), `USER` (mint, approve), `ADMIN` (limit), `TREASURY` (approve), `OPERATOR` (debits, refunds; metric `operator_gas_balance`) |

## 4. Database

- Roles are created once per environment by a superuser; `make migrate-up` (owner role) creates the schema and grants the rights of SRS — Core §3.2.

```sql
CREATE ROLE cas_server LOGIN PASSWORD '…';
CREATE ROLE cas_card_auth LOGIN PASSWORD '…';
```

- Every environment has its own passwords. DEV passwords are the owner's choice; demo, staging and production each get their own, never reused.
- Connection strings live in `.env` only (`.env.example` lists the names):

| Variable | Role | Used by |
|---|---|---|
| `DATABASE_URL` | `cas_server` | `server` |
| `CARD_AUTH_DATABASE_URL` | `cas_card_auth` | `card-auth` |
| `CASCTL_DATABASE_URL`, `MIGRATE_DATABASE_URL` | owner | `casctl` (`register.sh`), migrations |

## 5. Steps

### 5.1 Prerequisites

| Item | Check |
|---|---|
| Foundry 1.8.3 (`forge`, `cast`), Go 1.27, `buf` 1.73.0, `jq`, `curl`, `perl`, bash 3.2 or later | `forge --version`, `buf --version` |
| Submodules of `contracts/` | `git submodule update --init --recursive` |
| `.env` of the repository: `CAS_MASTER_KEY`, `DATABASE_URL`, `CARD_AUTH_DATABASE_URL`, `CASCTL_DATABASE_URL`, `CARD_AUTH_RPC_URL` (Alchemy, HTTPS), `CARD_AUTH_RPC_WS_URL` (Alchemy, WSS) | `make migrate-version` answers; no value is printed |
| Keys file and `CAS_SEPOLIA_KEYS` (§2); the five accounts funded (§3) | `deploy.sh` prints the balances |
| Schema | `make migrate-up` |

### 5.2 Rules of every script

- `set -euo pipefail`, never `set -x`.
- Order of the checks: the chain ID of its RPC first — only 84532 is accepted — then the keys file.
- The keys file is refused when `CAS_SEPOLIA_KEYS` is unset, when the file is missing, when it lies inside the repository working tree, and when group or others have any permission on it.
- A local RPC URL (`localhost`, `127.*`, `[::1]`) is refused; only the rehearsal switch `CAS_SEPOLIA_REHEARSAL=1` allows it, and then only local URLs are accepted (§8).
- No key, no URL with an API key, no password and no connection string is printed. `forge` and `cast` take a private key only as the argument `--private-key`: the script passes it from a variable, so it never enters the shell history, but it is visible in the process list of the machine while that command runs.
- Output is safe to paste into a chat: addresses, transaction hashes, explorer links on `sepolia.basescan.org`, amounts, timings, the RPC provider by name.
- Files written next to the keys file:

| File | Content | Mode |
|---|---|---|
| `cas-sepolia-deployment.env` | Addresses of the five roles and of the pair, deployment hashes and blocks. No secret | 644 |
| `cas-sepolia-credentials.env` | Tenant, processor username and password, service token | 600 |
| `cas-sepolia-forge/` | Broadcast record, cache with the RPC URL, log of `forge script` without secrets | 700 |

| Variable | Default | Scripts |
|---|---|---|
| `CAS_SEPOLIA_KEYS` | — (required) | all |
| `CAS_SEPOLIA_RPC_URL` | `https://sepolia.base.org` | `deploy.sh`, `setup.sh`, `register.sh` |
| `CAS_SEPOLIA_FALLBACK_URL` | `https://sepolia.base.org` | `run-card-auth.sh` |
| `CAS_ENV_FILE` | `.env` of the repository | `register.sh`, `run-card-auth.sh`, `measure.sh` |
| `CAS_BIN_DIR` | `bin/` | `register.sh`, `run-card-auth.sh`, `measure.sh` |
| `CAS_GRPC_ADDR` | `127.0.0.1:50053` | `register.sh` |
| `CAS_TENANT`, `CAS_OWNER_REF`, `CAS_CARD_REF`, `CAS_CARD_DAILY_LIMIT` | `sepolia-demo`, `owner-a`, `card_A`, 200 USDC | `register.sh`; `CAS_CARD_REF` also `measure.sh` |
| `CAS_SETUP_MINT`, `CAS_SETUP_ALLOWANCE`, `CAS_SETUP_DAILY_LIMIT`, `CAS_SETUP_REFUND_ALLOWANCE` | 100, 100, 50, 100 USDC | `setup.sh` |
| `CAS_MEASURE_N`, `CAS_MEASURE_AMOUNT` | 30, 1.00 USD | `measure.sh` |
| `CAS_CARD_AUTH_URL`, `CAS_CARD_AUTH_HEALTH_URL` | `http://127.0.0.1:8092`, `http://127.0.0.1:8093` | `measure.sh` |

### 5.3 Order

| # | Command | Does | Check after |
|---|---|---|---|
| 1 | `scripts/sepolia/deploy.sh` | `Deploy.s.sol` without `TOKEN`: `MockUSDC`, then the controller; broadcast by `DEPLOYER`; roles `TREASURY`, `ADMIN`, `OPERATOR`. Refuses when `cas-sepolia-deployment.env` exists | Both addresses and hashes printed; the script's line `Checked:` (`token()`, `treasury()`, `decimals()` 6, both roles); both links open on the explorer |
| 2 | `scripts/sepolia/setup.sh` | `USER` mints up to 100 USDC and approves 100 USDC; `ADMIN` sets the daily limit of `USER` to 50 USDC; `TREASURY` approves 100 USDC for refunds. A rerun sends only what differs; the limit is sent on every run, the contract has no view of it | `Result:` balance 100, allowance 100, remaining daily limit 50, refund allowance 100 |
| 3 | `make run` in a second terminal, then `scripts/sepolia/register.sh` | `casctl tenant create`, `processor issue`, `token issue`; `CreateConnection` (`base-sepolia`, wallet `USER`) and `RegisterCard` (`card_A`, 200 USDC) over gRPC with `buf curl` and the frozen image `proto/frozen/cas_v1.json`. A rerun continues from the credentials file | Card `card_A` `CARD_STATUS_ACTIVE` on the wallet of `USER`; `cas-sepolia-credentials.env` has mode 600 |
| 4 | `scripts/sepolia/run-card-auth.sh` in a third terminal | `card-auth` with the `.env` values of `card-auth`, then: `OPERATOR_PRIVATE_KEY` from the keys file (process environment only), chain ID 84532, controller and token of the deployment, decimals 6, finality `tag` / `finalized`, listener `pendingLogs`, fallback `CAS_SEPOLIA_FALLBACK_URL` | Start checks pass (S2-T803): `/readyz` 200 on 8093; `chain_listener_connected` 1 in `/metrics` |
| 5 | `casctl sim`, S2-T805, commands below | Authorize 5 USD; return 2 USD; wait for `finalized`, about 20 minutes | `APPROVED` with a `tx_hash` on the explorer; return `CONFIRMED`; `DEBIT_CONFIRMED` |
| 6 | `scripts/sepolia/measure.sh` | 30 USD authorizations of 1.00 in sequence, card `card_A` (S2-T806, S2-T807) | `PASS`: p95 of `auth_decision_seconds` ≤ 2 s; signals by source both counted; record the summary in the test plan |
| 7 | Alchemy dashboard | CU of the run (S2-T808) | About 400 CU per authorization |

- Step 5: the pair goes into the environment of the shell from the credentials file, never typed:

```sh
set -a; . "$(dirname "$CAS_SEPOLIA_KEYS")/cas-sepolia-credentials.env"; set +a
export CASCTL_CARD_AUTH_URL=http://127.0.0.1:8092
go run ./cmd/casctl sim authorize --auth-id t805-1 --card-ref card_A --amount 5.00 --currency USD
go run ./cmd/casctl sim return --auth-id t805-1 --return-id t805-r1 --type REFUND --amount 2.00
go run ./cmd/casctl sim get --auth-id t805-1
```

- The daily limit of 50 USDC covers T805 (5 USDC) and T806 (30 × 1 USDC) in one UTC day. A return does not restore the limit. A second measurement on the same day needs a higher limit: `CAS_SETUP_DAILY_LIMIT=100 scripts/sepolia/setup.sh`.

## 6. Admin operations

- Sent with `cast` and the `ADMIN` key of the keys file; no service and no CLI holds that key (SRS — Card Spend §3.2). The commands read the key inside a subshell, so it is not typed and not kept in the shell history.
- `CONTROLLER` and `USER_ADDRESS` are the lines `CONTROLLER=` and `USER=` of `cas-sepolia-deployment.env`.

```sh
( set -euo pipefail
  rpc=https://sepolia.base.org
  [ "$(cast chain-id --rpc-url "$rpc")" = 84532 ] || { echo "not Base Sepolia"; exit 1; }
  key=$(sed -n 's/^ADMIN_PRIVATE_KEY=//p' "$CAS_SEPOLIA_KEYS")
  cast send "$CONTROLLER" 'setDailyLimit(address,uint256)' "$USER_ADDRESS" 50000000 --private-key "$key" --rpc-url "$rpc" )
```

| Operation | Function and arguments | Effect | Check |
|---|---|---|---|
| Wallet daily limit | `'setDailyLimit(address,uint256)' <wallet> <base units>` | New limit; lowering it takes nothing back | `cast call $CONTROLLER 'remainingDailyLimit(address)(uint256)' <wallet>` |
| Pause | `'pause()'` | Blocks debits only; refunds still work | `cast call $CONTROLLER 'paused()(bool)'` is `true`; authorizations decline |
| Unpause | `'unpause()'` | Debits allowed again | `paused()` is `false` |

## 7. Operator key change

- Rule of SRS — Card Spend §3.2, rules of S2 st6b: the tracker handles only the rows of its own operator key. Every authorization and return of the old key must be final before the change.

| # | Step | Check |
|---|---|---|
| 1 | Stop new traffic: no more `casctl sim` or processor calls | — |
| 2 | Wait until every authorization of the old key is final: `DECLINED`, `DEBIT_CONFIRMED`, `LATE_DEBIT_REFUNDED`, `DEBIT_LOST`; and every return is `CONFIRMED` or `NOTHING_TO_RETURN`. On Base Sepolia the tag `finalized` takes about 20 minutes | `ListAuthorizations` by status; `/metrics`: `returns_not_confirmed` 0, `operator_tx_pending_seconds` 0 |
| 3 | Stop `card-auth` | — |
| 4 | Generate the new key as in §2; fund it (§3) | `cast wallet address` |
| 5 | `ADMIN`: `'grantRole(bytes32,address)' $(cast keccak OPERATOR_ROLE) <new operator>`, then `'revokeRole(bytes32,address)' $(cast keccak OPERATOR_ROLE) <old operator>`, as in §6 | `hasRole` true for the new, false for the old |
| 6 | Replace `OPERATOR_PRIVATE_KEY` in the keys file and the line `OPERATOR=` in `cas-sepolia-deployment.env` | `run-card-auth.sh` refuses a key that is not the `OPERATOR` of the deployment |
| 7 | Start `run-card-auth.sh` | Start checks pass; `operator_accounts` gets a row for the new address |

## 8. Alert: a nonce used outside `card-auth`

| Item | Content |
|---|---|
| Log line | Level Error, message `ALERT: the operator key was used outside card-auth: …`, fields `nonce`, `purpose`, `tx_hash` |
| Meaning | A nonce reserved by `card-auth` is used on chain by a transaction that is none of the hashes of its row: something else signed with the operator key. The row is left for manual handling; the alert is written once per row by a process |
| Risk | The operator key may be compromised. The contract bounds it: debits only to the treasury, within allowance and daily limit; refunds only to the debited wallet (SRS — Card Spend §3.2) |

| # | Manual step |
|---|---|
| 1 | `ADMIN`: `pause()` (§6). Refunds keep working |
| 2 | Find the transaction of that nonce: the operator address on the explorer, or `cast nonce` and the operator's transaction list; note sender, target and data |
| 3 | Find who used the key: any `cast` or script run with `OPERATOR_PRIVATE_KEY`; a second `card-auth` on the same key and chain (S2 runs one instance, SRS §4 issue 6) |
| 4 | Treat the key as compromised unless step 3 explains it: change it (§7) |
| 5 | The row itself: no tool and no rule yet; `docs/backlog.md` item 10 |
| 6 | `unpause()` when the cause is removed |

## 9. TLS

- `card-auth` listens on plain HTTP (SRS — Card Spend §2.1.1). Any access other than from the same machine goes through a TLS terminator in front of it: a reverse proxy holding the certificate, forwarding to `127.0.0.1:8092`.
- The health port 8093 (`/metrics`) is never exposed.
- S2 runs everything on the development machine; no terminator is set up in S2.

## 10. Troubleshooting

| Symptom | Cause | Action |
|---|---|---|
| `answers chain ID …: only 84532 (Base Sepolia) is allowed` | The RPC of the script serves another chain | Check `CAS_SEPOLIA_RPC_URL`, `CARD_AUTH_RPC_URL`, `CAS_SEPOLIA_FALLBACK_URL` |
| `is a local URL` / `is not a local URL` | A local URL without the rehearsal switch, or a public one with it | Unset `CAS_SEPOLIA_REHEARSAL` for Base Sepolia |
| `card-auth` stops at start, a message names `CARD_AUTH_CHAIN_ID` or the RPC | Start check of SRS §3.2: `eth_chainId` of the primary or the fallback, `token()`, `decimals()` | The message names the variable; no value is printed |
| `insufficient funds`, a transaction of `setup.sh` failed | The sender has no ETH | Faucet (§3); rerun the script: it continues |
| Declines `INSUFFICIENT_FUNDS`, `INSUFFICIENT_ALLOWANCE` or `LIMIT_EXCEEDED` | Wallet state, or the daily limit of the wallet or the card | `setup.sh` with higher amounts; the card limit with `UpdateCard` |
| `chain_listener_connected` 0, signals only from `polling` | Listener down: WebSocket closed, chain ID of `CARD_AUTH_RPC_WS_URL` wrong (`ALERT:` line) | Polling continues, decisions go on (FR-23); the listener reconnects with backoff up to 30 s |
| Log: `chain reads moved to the fallback endpoint` | `CARD_AUTH_RPC_FALLBACK_AFTER` consecutive failures of the primary read | Reads and sends use `https://sepolia.base.org`; every tracker cycle probes the primary; `chain reads back on the primary endpoint` when it answers. The listener stays on the primary |
| p95 above 2 s in `measure.sh` | Provider latency, listener off, network of the machine | Record the run anyway (S2-T806); repeat at another time of day |

## 11. Rehearsal

- `internal/rehearsal`, part of `make check`: every script end to end on Anvil started with `--chain-id 84532`, a throwaway keys file in a temporary directory, server and `card-auth` on the test database, `CAS_SEPOLIA_REHEARSAL=1`. No request leaves the machine.
- Covered: deploy; setup twice; register twice; `card-auth` without the listener (Anvil has no `pendingLogs`); authorize 5 USD, return 2 USD, finality by the tag `finalized` (Anvil answers it as the latest block − 64); `measure.sh` with N 10; every refusal of §5.2 on every script.
- Needs: `anvil`, `forge`, `cast`, `buf`, `jq`, `curl`, `perl`; the three test database URLs.

## 12. Evidence of st9b

| Item | Value |
|---|---|
| `MockUSDC` | `<address>`, `<explorer link of the deployment transaction>` |
| `CardSpendController` | `<address>`, `<explorer link of the deployment transaction>` |
| `DEPLOYER`, `ADMIN`, `OPERATOR`, `TREASURY`, `USER` | `<addresses>` |
| T805 debit, return | `<explorer links>` |
| T806 | `<date and time UTC, provider, N, p95, signals by source>` |
| T808 | `<CU of the run>` |
