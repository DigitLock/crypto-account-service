# Backlog

Postponed items. One row per item. An item leaves the table when a milestone takes it.

| # | Item | Details | Raised |
|---|---|---|---|
| 1 | Rotation of the master key | Several key versions in the environment and a `casctl` command that re-wraps the data keys ([ADR-4](adr/0004-secrets-encrypted-at-rest.md)). C1 stores `kek_version` and refuses a secret of another version ([SRS — Core](srs/core.md) §3.1) | C1 discovery, 2026-10-04 |
| 2 | Rate budgets per account | The limiter of C1 holds the budgets of a source by name, shared by all its connections. Binance counts some limits per account ([SRS — Binance](srs/exchange-binance.md)): X1 decides how a connector declares a budget per account | C1, 2026-10-05 |
| 3 | Warning for a malformed value of `sources.config` | A missing or malformed interval or `stale_after` takes its default silently ([SRS — Core](srs/core.md) §3.1). A log line at start would show a typo | C1, 2026-10-05 |
| 4 | Test hardening | T534 and T628 start `go list` and read `GOFLAGS` of the environment: clear it for the child process. No test covers a failed run reported with the family `unknown`: the fake connector cannot fail the declaration of streams ([test plan C1](test-plan-c1.md) §7) | C1 QA, 2026-10-05 |
