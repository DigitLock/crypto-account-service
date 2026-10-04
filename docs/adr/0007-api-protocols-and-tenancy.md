# ADR-7 — API protocols, tenancy and authentication

- **Status:** Accepted
- **Date:** 2026-10-03
- **Related:** BR-13, FR-105, ADR-3, SRS — Core §2.1.1, SRS — Card Spend §2.1.1

## Context

- Consumers are services: ET, partner backends. The processor is an external party with its own API contract.
- Lessons from ET: two protocols for the same API double the contract work; one secret shared between services breaks them together.
- CAS must serve several partners on one platform (BR-13) and store no personal data.

## Options

**Protocol**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | gRPC only | One contract | The processor cannot be told which protocol to use |
| 2 | gRPC and REST for every method | Easy access from any client | Two contracts to keep equal |
| 3 | gRPC for consumers; HTTP/JSON only where an external party defines the contract | One contract per audience | Two protocols in the service |

**Identity**

| # | Option | Pros | Cons |
|---|---|---|---|
| 1 | Reuse ET users and its token secret | Nothing new | Ties CAS to ET; repeats the shared-secret problem |
| 2 | Own users inside CAS | Full control | A user store and personal data in CAS |
| 3 | Tenant per consuming system; the end user is an opaque `owner_ref` | No personal data; any consumer fits | The consumer keeps the mapping to its users |

## Decision

Protocol option 3, identity option 3.

- **Consumer API:** gRPC, package `cas.v1`. The `.proto` files are the contract; lint and breaking-change checks run in CI.
- **Processor API:** HTTP/JSON on `card-auth` only, modelled on JIT Funding, described in OpenAPI.
- **Tenant:** one per consuming system. A credential resolves to exactly one tenant; every query is filtered by it.
- **Owner:** `owner_ref`, an opaque string supplied by the tenant. CAS has no user table.
- **Credentials:** a service token per tenant for gRPC; Basic credentials for the processor. Stored as hashes. Never shared with another service.
- **Isolation signal:** another tenant's resource is reported as not found.
- **Network:** internal only. mTLS is a later step.

## Trade-offs

- Two protocols, limited to one endpoint group.
- No direct access from a browser; a demo needs the CLI or a gateway.
- Service tokens are long-lived secrets with manual rotation.
- Inside a tenant there is no per-owner authorization: the tenant is trusted for all its owners.
