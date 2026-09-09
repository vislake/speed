---
title: Capability modules design
weight: 0
description: "Design pages for the five capability modules at the top of the dependency graph — billing, ai-gateway, sharing, compliance, admin — why each is shaped the way it is, and the design story that runs through the group."
bookCollapseSection: true
---

# Capability modules design

These five design pages are the developer-docs mirror of the
[user guide's capability-modules group](/docs/user-guide/modules/capabilities/):
where the user guide pages say what each module does and how a host
wires it, these pages say why it is shaped that way — the boundaries,
the rejected alternatives, and the mechanisms that carry the design.

The five modules sit at the top of the module dependency graph. Every
module below them is the floor or the capability face every binary
composes; this group is what a product *chooses* when it sells
something. None of the five is required, and each answers a different
product question:

- **billing** — the commerce domain: `Plan`/`Feature`/`Entitlements`,
  a channel-agnostic `Subscription`/`Invoice` lifecycle, and the
  credits ledger for pay-per-use. The design question: how to make
  "charge for something" safe to build on — database-arbitrated
  balances, idempotent settlement, and payment channels kept strictly
  behind a seam.
- **ai-gateway** — one vendor-agnostic door to LLM and image-generation
  endpoints. The design question: how to abstract every vendor without
  freezing the abstraction, and where to draw the line between the
  gateway and the business logic that pays for it.
- **sharing** — controlled public links to an internal resource. The
  design question: what an unauthenticated viewer must be allowed to
  do — almost nothing, under five mandatory rules enforced in code.
- **compliance** — retention sweeping, right-to-erasure, data export
  and audit querying. The design question: how a governance layer
  deletes and delivers data across other modules' tables without
  owning any table or inventing any mechanism of its own.
- **admin** — the platform operator's console backend. The design
  question: how the topmost module renders, searches and acts across
  every module below it without becoming a second identity system or a
  database-level "god view".

Read together, the five pages carry one continuous design story. The
first two — billing and ai-gateway — sit on the same dependency tier
and share the group's core discipline: same-tier modules never import
each other, so their connections are structurally-typed seams
(`billing`'s `Entitlements.Check` judgment and `metering`'s usage
recording reach ai-gateway's call path as mirror-shaped interfaces,
never as imports — the two pages explain why each side is shaped to
fit). compliance sits directly above them and *may* import what it
orchestrates — including a real `go/sharing` import for export
delivery, the deliberate contrast with the seam discipline below it.
admin sits at the very top and is the group's one sanctioned
exception: it imports the concrete packages of every module below it
directly, because it is the operations face over all of them. The
pages also cross-reference each other where one module's design
consumes another's: sharing's expiry policy serves compliance's export
delivery; compliance's `AuditQuery` serves admin's audit shell; the
dual-identity audit records admin writes are exactly what compliance's
query dimension reads back.

The per-module dependency-order design story continues in this
section's sibling groups (core, services, identity, tools); the
[developer-docs hub](/docs/developer-docs/) is the always-current map.
The [architecture](/docs/developer-docs/architecture/) page is this
group's foundation — the module wiring contract, the data domains and
the deployment axes the five pages assume. Each design page carries a
Source section linking back to the module's own `AGENTS.md`, so every
claim is verifiable against the original.

## The pages

- [billing](/docs/developer-docs/modules/capabilities/billing/) —
  the commerce domain model, the credits ledger's reserve/confirm/
  refund and single-statement arbitration, and the payment-gateway
  seam.
- [ai-gateway](/docs/developer-docs/modules/capabilities/ai-gateway/) —
  vendor-agnostic chat and image providers, the async-only image
  pipeline, and the structural seams that keep it independent of the
  tier it shares with billing.
- [sharing](/docs/developer-docs/modules/capabilities/sharing/) —
  the five mandatory rules of a public share link, token-first tenant
  resolution, and delivery-settled view accounting.
- [compliance](/docs/developer-docs/modules/capabilities/compliance/) —
  orchestration over participants, why the module owns no table, and
  where each deletion and audit mechanism actually lives.
- [admin](/docs/developer-docs/modules/capabilities/admin/) — the
  exception-shaped top module: direct imports, the tenant ledger,
  impersonation as an authorization credential, and audited
  cross-tenant reads.

## Source

- Module discipline: [go/billing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md), [go/ai-gateway/AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md), [go/sharing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md), [go/compliance/AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md), [go/admin/AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md)
