# Binance Key Guide

- **Purpose:** connect the owner's Binance account to CAS with a key that can only read, check it, and remove it again (X1 st7b).
- **Status:** Confirmed by the owner on 2026-10-10 (X1 st7b, X1 D-46). Facts come from [SRS — Binance](srs/exchange-binance.md) with their dates.
- **Evidence:** option B (P-1): date, verdict and counts only. No amount, asset name, account identifier or key leaves the owner's terminal.
- **Tests:** [Test Plan — X1](test-plan-x1.md) phase 6, X1-T601 … X1-T608.

## 1. Rules

| Rule | Source |
|---|---|
| The key holds reading only. Any other permission is refused, known or unknown | SRS — Binance UC-201 step 3, FR-201; X1 D-1 |
| The key is HMAC ("system-generated" by Binance); with an unrestricted IP it can hold reading only. Check that HMAC is still offered when the key is created (it was on 2026-10-10); if not, stop: Ed25519 is decided then | SRS — Binance §2.1.1 Signature, §4 issue 1 (Binance FAQ updated 2025-03-20; re-checked 2026-10-09) |
| No IP restriction for this run: the key is accepted; `ip_restricted` `false` is written in the audit row | SRS — Binance EC-213; X1 D-2 |
| The key and the secret live only in `.env` as `BINANCE_API_KEY` and `BINANCE_API_SECRET`: never in a command, a file of the repository, a chat or a report | `.env.example`; CLAUDE.md |
| Base URL `https://api.binance.com`. Serbia is not a prohibited country (List of Prohibited Countries, updated 2026-01-05) | SRS — Binance §2.1.1, §4 issue 6 |
| Delete the connection when the run is recorded, then the key (in X1 at the close of the milestone) | Test plan X1-T608; X1 P-7, X1 D-52 |

## 2. Steps

| # | Step | Command | What it prints |
|---|---|---|---|
| 1 | Create the key on Binance: Account → API Management → Create API → System generated (HMAC). Under API restrictions tick only Enable Reading; leave Spot & Margin & Stock Trading, Margin Loan Repay & Transfer, Universal Transfer, Withdrawals, Alpha Withdrawals, Prediction Trading and Symbol Whitelist unticked. IP access restrictions: Unrestricted. Copy the API Key and the Secret Key at once: Binance does not show them in full again | — | — |
| 2 | Read the warning of the form (Default Security Controls): Binance deletes a key with an unrestricted IP once any permission other than Reading is enabled | — | — |
| 3 | Put the key into `.env` | `BINANCE_API_KEY=…`, `BINANCE_API_SECRET=…` in an editor, without quotes | — |
| 4 | Dry run, nothing stored (X1-T601, T605; T607 reads its headers, X1 D-48) | `make binance-real` | The status of each of the six endpoints of X1, the time offset, the key check and `ip_restricted`, unknown and non-boolean permission fields by name, the count of assets per account type, the LD counts of issue 2, the count of assets without an alias row, the used-weight headers of the four `/sapi` endpoints |
| 5 | Start `server` with `KEY_CHECK_INTERVAL=30m` in `.env` | `make run` in a second terminal; on macOS `caffeinate -i make run` keeps the machine awake | Its log |
| 6 | Connect (X1-T602) | `scripts/binance/connect.sh create` (needs and variables: §4) | Tenant `x1-real` created or existing; the service token key ID; `connection_id`, status, key fingerprint, permissions |
| 7 | Check the stored connection | `make casctl ARGS="connection inspect <connection_id>"` | Status, permissions, `ip_restricted`, ciphertext yes, counts of snapshots, balance rows, cursors and ledger entries, audit rows per action; `uid in audit details: no` |
| 8 | Compare the balances with the exchange UI (X1-T603, T604) | `scripts/binance/connect.sh balances <connection_id>`; for the comparison only, `… --amounts` | `as_of`, `stale`, the count per account type; with `--amounts` a table for the local comparison: do not copy or share it |
| 9 | Let `server` run at least 2 hours, then read its metrics (X1-T606) | `curl -s http://127.0.0.1:8091/metrics \| grep -E '^(binance_\|rate_limit_rejections_total\|sync_runs_total\|key_checks_total\|unmapped_assets_total)'` | Expected: `sync_runs_total{source="binance",stream="balances",result="success"}` at least 8, no `failure`; `key_checks_total{source="binance",result="success"}` at least 4, no `invalid` or `failure` (X1 D-50); `binance_rate_limit_responses_total` 0; `rate_limit_rejections_total{source="binance"}` 0; `binance_used_weight` under the share of each budget |
| 10 | Delete the connection (X1-T608) | `scripts/binance/connect.sh delete <connection_id>`, then `connect.sh balances <connection_id>` and `make casctl ARGS="connection inspect <connection_id>"` | `deleted`; `not_found`; `not found`, 0 snapshots, balance rows and cursors, audit rows only, `uid in audit details: no` |
| 11 | Clean up: stop `server`; revoke the token of `x1-real` and remove the credentials file; remove `KEY_CHECK_INTERVAL` from `.env`; delete the key on Binance and remove its two lines from `.env` (in X1 at the close, X1 D-52) | `make casctl ARGS="token revoke <key_id>"`; `rm ~/.config/cas/x1-real.env` (or the file named by `CAS_X1_CREDENTIALS`) | `Token <key_id> revoked` |

The command of step 9, to copy:

```sh
curl -s http://127.0.0.1:8091/metrics | grep -E '^(binance_|rate_limit_rejections_total|sync_runs_total|key_checks_total|unmapped_assets_total)'
```

## 3. What to record for the report

| Item | Form |
|---|---|
| Date of the run | `2026-10-..` |
| Verdict of each row X1-T601 … X1-T608 | Pass or Fail, with the reason in words |
| Counts | Assets per account type; LD counts; assets without an alias row |
| Field names of the dry run | The names of unknown or non-boolean `enable*` / `permits*` fields of `apiRestrictions`: Binance field names, not account data (X1 D-47) |
| Used-weight headers of the dry run | Header names and values of the four `/sapi` endpoints (X1-T607, after T601: X1 D-48) |
| Never | An amount, an asset name, the `uid`, the key, a fingerprint, a URL, a server log line (it may name an asset), the `--amounts` table |

## 4. `connect.sh`

- Needs: bash, `go`, `buf`, `jq` on `PATH`; `server` running (step 5); `CASCTL_DATABASE_URL` (owner role) in `.env`. `create` builds `casctl` into `CAS_BIN_DIR` first.
- The key and the secret are read from `.env` as data and never reach a command line, an environment or a file left behind (X1 D-40).

| Variable | Default | Rule |
|---|---|---|
| `CAS_ENV_FILE` | `.env` of the repository | The file with `BINANCE_API_KEY`, `BINANCE_API_SECRET`, `CASCTL_DATABASE_URL` |
| `CAS_X1_CREDENTIALS` | `~/.config/cas/x1-real.env` | Holds the service token of `x1-real`; outside the repository, mode 600; refused otherwise |
| `CAS_GRPC_ADDR` | `127.0.0.1:50053` | Loopback only; another host is refused |
| `CAS_BIN_DIR` | `bin/` | Where `casctl` is built |
