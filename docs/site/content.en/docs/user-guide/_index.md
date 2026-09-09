---
title: User guides
weight: 10
bookCollapseSection: true
---

# User guides

Build your SaaS on speed: pick the modules you need, compose them into
one binary, and own the result. These guides are for teams **using**
speed's Go modules and npm packages in their own product.

## Two ways to read this section

- **By domain** — the fastest way in. Each domain page walks one
  product need end to end (identity and access, tenancy and
  organizations, notifications, billing, AI, ...): which modules are
  involved, the minimal integration steps, an example, and where to go
  next. Start with the [Quickstart](/docs/user-guide/quickstart/), then follow the
  domains your product needs.
- **By module** — the complete reference. Every Go module and npm
  package has its own page: what it is for, when to choose it, how to
  wire it, its core concepts and API surface, runnable examples, and
  its known limitations.

The section's left navigation mirrors the module dependency order:
infrastructure first (`pkgcore`, `dbkit`, ...), then platform services,
then identity and organization, then the capability modules on top.

## Reference pages

- [API reference](api-reference/) — the complete platform HTTP API,
  rendered from the merged OpenAPI contract: every operation,
  parameter, schema and error, grouped by module.
- [Error code index](error-codes/) — every structured error code a
  speed-based API can answer with, with status, locale message,
  triggering condition and source.
