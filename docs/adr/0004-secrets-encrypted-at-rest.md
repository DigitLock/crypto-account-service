# ADR-4 — Secrets encrypted at rest

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-2, FR-103, ADR-3, SRS — Core §2.4, BRD §9.2 Security

## Context

- CAS stores exchange API keys and secrets of its users. A leak exposes their balances and history.
- Database dumps, backups and logs are the usual leak paths.
- The service is self-hosted: no cloud key management is available by default.

## Options

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Plain text, protected by database access rules | Nothing to build | Every dump or backup contains usable secrets |
| 2 | One master key encrypts every secret directly | Simple | Rotation re-encrypts all secrets; one key touches all data |
| 3 | Envelope encryption: a data key per connection, wrapped by a master key from the environment | Rotation re-wraps small keys only; a ciphertext is bound to its row | Master key still lives on the host |
| 4 | External key management (KMS, Vault) | Key never on the application host; audit of key use | Extra infrastructure for a self-hosted project |

## Decision

Option 3.

- **Cipher:** AES-256-GCM.
- **Data key:** random, one per connection, wrapped by the master key.
- **Binding:** the connection ID is authenticated data, so a ciphertext cannot be moved to another row.
- **Master key:** from the environment, versioned (`kek_version`). Rotation re-wraps the data keys; secrets are not re-encrypted.
- **Use:** a secret is decrypted in memory only to sign a request. It is never logged and never returned; the API shows a fingerprint.
- **Interface:** encryption sits behind a vault interface, so option 4 can replace the environment key later.
- **Other credentials:** service tokens and processor passwords are stored as hashes.

## Trade-offs

- A compromised host exposes the master key and with it all secrets. Accepted: the keys are read-only (ADR-3), which bounds the damage to data exposure.
- Losing the master key means every connection must be created again.
- Secrets exist in process memory while a request is signed.

## Production path

Not built in the MVP. Recorded so the step is known.

- **Exchange secrets:** the vault interface gets a second implementation backed by an external KMS or Vault. Only wrapping and unwrapping of data keys moves out; the data model stays.
- **Operator key (ADR-10):** signing moves into a KMS or HSM that supports the secp256k1 curve. Vault Transit does not support it.
- **Cost:** one more stateful service to run; it must be unsealed after a restart; the service still needs a credential to reach it; sync stops while it is unavailable.
- **Trigger:** the first real tenant, or any use beyond test networks.
