---
title: Module designs
weight: 0
description: "The per-module design section of Developer docs — one page per Go module and npm package on why it is shaped the way it is: responsibility and boundary, design reasoning, trade-offs, key mechanisms, and the frozen public surface."
---

# Module designs

The [architecture](/docs/developer-docs/architecture/) page explains
the shape of speed and the [design
principles](/docs/developer-docs/design-principles/) page the rules
that keep it intact. This section lands the third layer: **one design
page per module** — why each module is shaped the way it is, what it
deliberately does not do, and which of its contracts are frozen.

## How this section relates to the user guide

The user guide's [modules](/docs/user-guide/modules/) pages answer
*how*: how to install a module, wire it, call it. These pages answer
*why*: the responsibility and boundary of each module (including the
things it refuses to do), the design reasoning behind its mechanisms,
the trade-offs that were weighed, and the stable surface consumers may
depend on. The two views complement each other and link back and
forth; a design claim that surprises you can be verified against the
internal document named in that page's Source section.

## Reading order

Start with the architecture and design-principles pages, then read a
group's guide before its module pages — each group guide states the
division of labour among the modules in one sentence per module and
the dependency chain between them. The module pages themselves follow
dependency order, so a group reads top to bottom as one design story.

## How the section is organised

The module space is grouped the way the user guide groups it:

- **core** — the dependency floor every binary composes: the assembly
  contract, the data layer, tenant resolution, observability, dynamic
  configuration, the job queue and rate limiting. Landed with this
  batch.
- **services** — the platform's business-shaped services (object
  storage, notifications, and the modules above them).
- **identity** — authentication and authorization: who a caller is,
  never what they may do.
- **capabilities** — metering, billing, sharing and the outward-facing
  surfaces built on them.
- **tools** — the developer- and operator-facing tooling.

Pages land as the site's content batches complete them, exactly as the
[Developer docs](/docs/developer-docs/) hub records for this whole
section; the left navigation is the always-current map.

## The Source convention

Every page in this section is a distillation, never a copy: the raw
material is the repository's internal design documents
(`docs/internal/`, written in Chinese) and each module's own
`AGENTS.md` — the module-level discipline document that ships with the
module to consuming projects. Internal deliberation, unresolved
tracking items and release scheduling stay out of these pages; each
page carries a Source section linking back to the originals, so any
claim can be checked against the underlying document, and a Related
pages section linking its neighbours in this section and the
corresponding user-guide page.
