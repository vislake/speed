---
title: Documentation
weight: 1
---

# Documentation

speed ships as independently released Go modules and npm packages, not
an application. Its documentation is split into two main sections,
plus a small set of site-level pages.

## [User guides](user-guide/) — build your SaaS on speed

For teams **using** speed modules in their own product: the overall
introduction, domain-by-domain fast starts, and a complete per-module
usage reference — Go modules and npm packages alike — with runnable
examples. Includes the [error code index](user-guide/error-codes/), the
complete list of codes a speed-based API can answer with.

## [Developer docs](developer-docs/) — develop speed itself

For **developers working on speed**: the overall architecture, the
design principles behind it, and per-module design deep dives that
explain the design rationale — why each module is shaped the way it is —
with diagrams where they help. Includes the implementation status
snapshot.

## Site-level pages

- [For AI Agents](ai-agents/) — what to read first as a coding agent,
  the architecture rules that most often matter, and where the
  authoritative implementation status lives.
- [About](about/) — what speed is, how its documentation is
  distributed.
- [Status](status/) — a coarse implementation snapshot; the repository
  root CLAUDE.md's Repository Status section is the authoritative
  source of truth and always wins over any status claim on this site.

A machine-readable index of every page lives at [/llms.txt](/llms.txt).
