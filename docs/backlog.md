# Backlog

Postponed items. One row per item. An item leaves the table when a milestone takes it.

| # | Item | Details | Raised |
|---|---|---|---|
| 1 | Rotation of the master key | Several key versions in the environment and a `casctl` command that re-wraps the data keys ([ADR-4](adr/0004-secrets-encrypted-at-rest.md)). C1 stores `kek_version` and refuses a secret of another version ([SRS — Core](srs/core.md) §3.1) | C1 discovery, 2026-10-04 |
