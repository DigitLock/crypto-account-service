# ADR-1 — Prices in CRS, accounts in CAS

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-3, BR-11, milestone E1, SRS — Core §2.1.3

## Context

- Consumers want a portfolio value in fiat: balance × rate.
- CRS already serves rates with provider polling, failover and a staleness flag. Crypto pairs are in its planned scope.
- CAS holds balances and history, not market data.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | CAS returns balances only; the consumer multiplies them by CRS rates | One owner per concern; no duplicated polling and failover | Two calls for the consumer; CRS must add crypto pairs |
| 2 | CAS calls CRS and returns valued balances | One call for the consumer | CAS depends on CRS for every read; two places decide which rate applies |
| 3 | CAS reads prices from exchange tickers itself | No dependency on CRS | Second rate service inside CAS: polling, failover, staleness built again |

## Decision

Option 1.

- CAS never serves rates and returns no fiat valuation.
- Valuation is done by the consumer: CAS balance × CRS rate.
- CAS consumes a CRS rate in one place only: `card-auth` quotes a non-USD authorization (BR-11). It uses the rate; it does not publish it.

## Trade-offs

- The consumer makes two calls and combines two freshness flags: balance and rate.
- Portfolio value in ET waits for crypto pairs in CRS (milestone E1).
- Two consumers can value the same balance differently if they read rates at different moments.
