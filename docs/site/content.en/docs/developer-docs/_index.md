---
title: Developer docs
weight: 20
---

# Developer docs

Develop speed itself: this section is for **developers working on
speed** — the repository's own engineers and anyone extending its
modules. It explains the architecture, the design principles behind it,
and why each module is designed the way it is.

## Structure

- **Architecture** — the modular monolith, the module dependency
  direction, the deployment mode / implementation composition axes,
  and the module wiring contract.
- **Design principles** — the discipline every module obeys: module
  boundaries, multi-tenant isolation, spec-first API contract,
  asynchronous work, testing tiers — and why.
- **Per-module design** — one page per Go module and npm package: the
  module's responsibility and boundary (including what it deliberately
  does **not** do), the design decisions and trade-offs behind its
  shape, its key mechanisms, and what of it is a frozen public API.
  The left navigation mirrors the module dependency order, so pages
  read top to bottom as the design story.
- **Status** — where implementation genuinely stands today; the
  repository root CLAUDE.md's Repository Status section is the
  authoritative source of truth.

Pages in this section land as the site's content batches complete
them; the left navigation is the always-current map.

The design pages are distilled from the repository's internal design
documents (`docs/internal/`) and each module's `AGENTS.md` — every page
carries a Source section linking back to the originals, so you can
always verify a claim against the underlying document.
