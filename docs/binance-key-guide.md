# Binance Key Guide

- **Purpose:** connect the owner's Binance account to CAS with a key that can only read, check it, and remove it again (X1 st7b).
- **Status:** Draft, X1 st7a, 2026-10-09 (X1 D-46). Facts come from [SRS — Binance](srs/exchange-binance.md) with their dates. A step marked **to be confirmed by the owner in st7b** is not in the documents yet.
- **Evidence:** option B (P-1): date, verdict and counts only. No amount, asset name, account identifier or key leaves the owner's terminal.
- **Tests:** [Test Plan — X1](test-plan-x1.md) phase 6, X1-T601 … X1-T608.

## 1. Rules

| Rule | Source |
|---|---|
| The key holds reading only. Any other permission is refused, known or unknown | SRS — Binance UC-201 step 3, FR-201; X1 D-1 |
| The key is HMAC ("system-generated" by Binance); with an unrestricted IP it can hold reading only. Check that HMAC is still offered when the key is created; if not, stop: Ed25519 is decided then | SRS — Binance §2.1.1 Signature, §4 issue 1 (Binance FAQ updated 2025-03-20; re-checked 2026-10-09) |
| No IP restriction for this run: the key is accepted; `ip_restricted` `false` is written in the audit row | SRS — Binance EC-213; X1 D-2 |
| The key and the secret live only in `.env` as `BINANCE_API_KEY` and `BINANCE_API_SECRET`: never in a command, a file of the repository, a chat or a report | `.env.example`; CLAUDE.md |
| Base URL `https://api.binance.com`. Serbia is not a prohibited country (List of Prohibited Countries, updated 2026-01-05) | SRS — Binance §2.1.1, §4 issue 6 |
| Delete the connection, then the key, when the run is recorded | Test plan X1-T608; X1 P-7 |

## 2. Steps

| # | Step | Command | What it prints |
|---|---|---|---|
| 1 | Create the key on Binance: a system-generated (HMAC) key; permissions: reading only; IP access: unrestricted. The names of the menus and buttons: **to be confirmed by the owner in st7b** | — | — |
| 2 | "Default Security Controls" of the key: **to be confirmed by the owner in st7b** | — | — |
| 3 | Put the key into `.env` | `BINANCE_API_KEY=…`, `BINANCE_API_SECRET=…` in an editor | — |
| 4 | Dry run, nothing stored (X1-T601, T605; T607 reads its headers, X1 D-48) | `make binance-real` | The status of each of the six endpoints of X1, the time offset, the key check and `ip_restricted`, unknown and non-boolean permission fields by name, the count of assets per account type, the LD counts of issue 2, the count of assets without an alias row, the used-weight headers of the four `/sapi` endpoints |
| 5 | Start `server` with `KEY_CHECK_INTERVAL=30m` in `.env` | `make run` in a second terminal | Its log |
| 6 | Connect (X1-T602) | `scripts/binance/connect.sh create` | Tenant `x1-real` created or existing; the service token key ID; `connection_id`, status, key fingerprint, permissions |
| 7 | Check the stored connection | `make casctl ARGS="connection inspect <connection_id>"` | Status, permissions, `ip_restricted`, ciphertext yes, counts of snapshots, balance rows, cursors, audit rows; `uid in audit details: no` |
| 8 | Compare the balances with the exchange UI (X1-T603, T604) | `scripts/binance/connect.sh balances <connection_id>`; for the comparison only, `… --amounts` | `as_of`, `stale`, the count per account type; with `--amounts` a table for the local comparison: do not copy or share it |
| 9 | Let `server` run at least 2 hours, then read its metrics (X1-T606) | `curl -s http://127.0.0.1:8091/metrics \| grep -E '^(binance_\|rate_limit_rejections_total\|sync_runs_total\|key_checks_total\|unmapped_assets_total)'` | Expected: `sync_runs_total{source="binance",stream="balances",result="success"}` at least 8, no `failure`; `key_checks_total{source="binance",result="success"}` at least 4, no `invalid` or `failure` (X1 D-50); `binance_rate_limit_responses_total` 0; `rate_limit_rejections_total{source="binance"}` 0; `binance_used_weight` under the share of each budget |
| 10 | Delete the connection (X1-T608) | `scripts/binance/connect.sh delete <connection_id>`, then `connect.sh balances <connection_id>` and `make casctl ARGS="connection inspect <connection_id>"` | `deleted`; `not_found`; `not found`, 0 snapshots, balance rows and cursors, audit rows only, `uid in audit details: no` |
| 11 | Delete the key on Binance and remove both lines from `.env` | — | — |

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
| Never | An amount, an asset name, the `uid`, the key, a fingerprint, a URL |
