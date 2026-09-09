---
title: Developer docs
weight: 20
bookCollapseSection: true
---

# Developer docs

Develop speed itself: this section is for **developers working on
speed** — the repository's own engineers and anyone extending its
modules. It explains the architecture, the design principles behind it,
and why each module is designed the way it is.

## Structure

- [**Architecture**](architecture/) — the modular monolith, the module
  dependency direction, the deployment mode / implementation
  composition axes, and the module wiring contract.
- [**Design principles**](design-principles/) — the discipline every
  module obeys: module boundaries, multi-tenant isolation, spec-first
  API contract, asynchronous work, testing tiers — and why.
- [**Per-module design**](modules/) — one page per Go module and npm
  package: the module's responsibility and boundary (including what it
  deliberately does **not** do), the design decisions and trade-offs
  behind its shape, its key mechanisms, and what of it is a frozen
  public API. The section mirrors the module dependency order, so
  pages read top to bottom as the design story.
The design pages are distilled from each module's own `AGENTS.md` —
every page carries a Source section linking back to it, so you can
verify a claim against the module's shipped documentation.
