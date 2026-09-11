---
title: Capability modules
weight: 4
description: "The product-facing capability modules at the top of the dependency graph — billing, ai-gateway, sharing, compliance and admin — optional by design, chosen by what the product sells rather than by what every binary needs."
bookCollapseSection: true
---

# Capability modules

These five Go modules sit at the top of the module dependency graph.
Where the module reference's core group is the floor every speed binary
composes (`pkgcore`, `dbkit`, `tenancy`, `observability`, `config`,
`jobs`, `ratelimit`), and the services group is the capability face a
product turns into features (`storage`, `notification`, `pki`,
`integration`, `metering`), this group is what a product chooses when it
sells something: commerce and pay-per-use gating, AI calls against
vendor endpoints, controlled public sharing of an internal resource,
governance over data the platform holds, and the platform operator's
own console.

None of the five is required. Every module below them in the graph
boots without them, and a host that wants none of these surfaces simply
leaves them out of its `Kernel.Bootstrap` set. They are picked by
product decision, and they are picked on top of the groups below: each
page's "Wiring it in" section names the services and core modules it
composes with (billing judges usage `go/metering` records, compliance
delivers exports through `go/sharing`, admin fans in on every module
below it).

## Pages in this group

| Page | What it gives you | Typical first use |
|---|---|---|
| [billing](billing/) | The Plan/Feature/Entitlement domain model, a channel-agnostic subscription and invoice lifecycle, and the credits ledger with a payment-gateway layer behind it | Charging for your product: subscriptions, pay-per-use credits, and "can this tenant use this" gates |
| [ai-gateway](ai-gateway/) | A vendor-agnostic chat and image-generation gateway: provider registries, BYOK credentials, async image jobs | Calling an LLM or image vendor through one facade, tenants bringing their own keys |
| [sharing](sharing/) | Public share links to internal resources, with five mandatory security rules and full access logging | An anonymous, single-use view of a resource its owner chose to share |
| [compliance](compliance/) | Retention-window sweeps, right-to-erasure orchestration, export gathering-and-delivery and read-only audit querying | Making retention and erasure real against the data your modules already store |
| [admin](admin/) | The platform-staff operations console: tenant ledger and suspension, impersonation, cross-tenant search, audit query and export, role management, usage dashboard | Operator surfaces over capabilities every other module already provides |

Each page follows the same shape as the other groups: *What it is
for* (including what the module deliberately does **not** do), *When to
choose it*, *Wiring it in*, *Core concepts and API surface*, and
*Limitations and links*. Coded errors the modules answer with are
listed in the [error code index](/docs/user-guide/error-codes/), one
row per code. The module pages in the groups below — the [core
group](/docs/user-guide/modules/core/) and the [services
group](/docs/user-guide/modules/services/) — cover the floors these
five stand on, and the [domain guides](/docs/user-guide/domains/) cover
the product side: [Billing and
metering](/docs/user-guide/domains/billing-metering/) and [Storage,
sharing and AI](/docs/user-guide/domains/storage-sharing-and-ai/) are
the two closest to this group's pages.

The platform's other groups sit beside this one in the same reference —
identity (authn, rbac, org), tools (saasctl) and the web packages. This
group's pages name those modules where a composition needs them, and
link only the group pages that already exist (core and services); the
remaining groups' pages land alongside theirs.
