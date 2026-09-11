---
title: Platform services
description: "The capability-face group: storage, notification, pki, integration and metering — the five Go modules a consumer wires directly when its product stores media, sends messages, manages key material, exposes an API or meters usage."
weight: 2
bookCollapseSection: true
---

# Platform services

These five Go modules are the platform's high-level capability face.
Where the module reference's core group supplies the assembly contract
and infrastructure floor (`pkgcore`, `dbkit`, `tenancy`,
`observability`, `config`, `jobs`, `ratelimit`) — surfaces a consumer
project builds *on*, rarely ships *as* — the services group is what a
product actually turns into features: media objects, outbound
messages, signing keys and certificates, a tenant's outward-facing
API, and usage measurement. Each module ships real tables with
dual-dialect migrations and its declared permissions, audit actions,
events and job handlers, and every one but metering mounts an HTTP
fragment at `/api/v1/*` (metering is a Go-level API business modules
call in-process, deliberately without an HTTP surface); the reference
app is the mandatory first consumer of every one.

The pages here are usage guides for a consumer team — what each module
is for, when you choose it, how to wire it, its core concepts and its
honest limitations. They are not a substitute for each module's own
`AGENTS.md`, which remains the authoritative, always-current document;
every page links it under *Source*.

The group is ordered by dependency, as the left navigation shows:
`storage`, `notification` and `pki` sit directly on the core group's
`jobs`/`tenancy` tier; `integration` and `metering` build on top. All
five expect a working core group underneath — a `ComponentRegistry` with the
infrastructure modules resolved, a `jobs.Queue` for asynchronous work —
so read the core group's pages first if you are assembling a host from
scratch, then the [Quickstart](/docs/user-guide/quickstart/), which generates a
starter project that already wires most of the floor.

## Pages in this group

| Page | What it gives you | Typical first use |
|---|---|---|
| [storage](storage/) | Media-object metadata in the database, bytes in your object store, a three-step upload protocol with server-side revalidation | User uploads of images/media that must be served back sanitized |
| [notification](notification/) | Outbound messaging: in-app inbox, email and SMS, with consent-verified external recipients | Every message your product sends, on channels the recipient chose |
| [pki](pki/) | Key material with a lifecycle: Ed25519 signing keys and an internal CA, with rotation, revocation and CRLs | Signing tokens and attesting content with keys that rotate |
| [integration](integration/) | A tenant's outward API: API keys, three-layer rate limiting, and signed outbound webhooks | Machine access for partners and scripts; events delivered to customers |
| [metering](metering/) | Usage recording with two reliability tiers, aggregated into per-tenant summaries | Counting what your product measures — analytics now, quotas later |

Each page follows the same shape: *What it is for* (including what the
module deliberately does **not** do), *When to choose it*, *Wiring it
in*, *Core concepts and API surface*, and *Limitations and links*.
Coded errors the modules answer with are listed in the [error code
index](../../error-codes/), one row per code; the pages link to their
module's rows. The higher-level domain guides — [Jobs and
notifications](../../domains/jobs-and-notifications/), [Storage,
sharing and AI](../../domains/storage-sharing-and-ai/) and
[Billing and metering](../../domains/billing-metering/) — cover these
modules from the product side; these pages cover them from the wiring
side. The remaining platform modules (identity, tenancy and
organisation, commerce and compliance) live in this reference's other
groups, and the frontend npm packages that consume these modules'
generated APIs are listed on the [module index](../../../modules/).
