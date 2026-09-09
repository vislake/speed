---
title: Data and configuration
weight: 3
description: Declaring tenant-scoped models and their repositories on dbkit, and serving runtime configuration through the config module.
---

# Data and configuration

This domain is the persistence floor of your product: how your own
tables are declared, migrated and queried under tenant isolation
(`dbkit`), and how runtime configuration and feature flags are served
to every layer of your product (`config`).

## The dbkit shape

```mermaid
flowchart LR
    M[Tenant-scoped model\nID + TenantID fields] --> R[Your repository\nembeds dbkit.Repository[T]]
    R -->|Create/Find/Update/Delete| DB[(SQLite or PostgreSQL\nsame code, both dialects)]
    MIG[Versioned SQL migrations\none set per dialect] --> DB
    R -.->|auto-injected tenant filter| DB
```

Every tenant-owned model declares `ID` and `TenantID` (the tenant as
the leftmost key column), and every repository embeds
`dbkit.Repository[T]` — you never hold a raw `*gorm.DB` and hand-write
`WHERE tenant_id = ?`. Repository methods take the tenant from the
context, so a caller without tenant context fails closed rather than
leaking rows across tenants.

## Minimal integration steps

1. **Declare the model.** `dbkit.Repository[T]` requires the
   `dbkit.TenantScoped` marker: `ID` and `TenantID` with the exact
   field shapes the repository documents, tenant first in every
   composite index.
2. **Embed the repository.** Your repository type embeds
   `dbkit.Repository[YourModel]`; the generic base supplies the
   tenant-filtered CRUD plus the `dbkit.WithTenantSession` transaction
   shape for multi-statement writes.
3. **Migrate with versioned SQL, never `AutoMigrate`.** Each module
   ships dual-dialect migration sets (SQLite and PostgreSQL) applied by
   `dbkit.MigrationRegistry`; a module exposes them through its
   `pkgcore.Module` `Migrations()`. Both dialects are first-class —
   avoid PostgreSQL-only features (`gen_random_uuid()`, native arrays,
   `NOW()`); generate IDs in the application.
4. **Classify every table before designing it.** Tenant data is
   tenant-scoped and runs `tenancytest.AssertIsolated`; identity data
   (a person who may belong to several tenants) and platform data
   (globally shared, tenants read only) never implement
   `TenantScoped` and run `AssertNotTenantScoped` instead.
5. **Encrypt what must be queryable.** A field that is both sensitive
   and a lookup key (a phone number used as a login identifier) is
   encrypted at rest *and* blind-indexed through
   `dbkit.NewBlindIndexer` — HMAC over the canonical form — never
   queried in plaintext.

## The config shape

`config` serves values and feature flags to every layer of a speed
product: tenant-facing branding and support settings, capability
switches, AI keys. Two decisions shape its use:

- **Register vs Attach.** Modules *declare* their config items and
  flags on the registry during `Register`; `Attach` — exactly once,
  after `Kernel.Bootstrap` returns — folds every module's declarations
  into one schema and refuses a cipher-less startup while any
  `Sensitive` item exists. A host keeps the `*Service` Attach returns.
- **Scope tiers and fallback.** Every value lives at one tier
  (tenant / system); reads fall back narrow-to-wide: tenant row, then
  system row, then the schema default. A tenant-less context never
  sees tenant rows.

Reads go through `Service.Get` (typed variants `GetTyped[string]`,
`GetTyped[bool]`, ...). Writes are attributed to the context's tenant;
a system-tier write requires the audited system context. Sensitive
values are sealed with your `dbkit.Cipher` and redacted everywhere a
boundary would leak them — events, logs, watch deliveries.

The two pre-auth endpoints — `/api/config/public` (public items only)
and `/api/system/features` (the resolved enabled-flag list) — serve the
login page and the frontend's channel visibility. Name them in your
tenant middleware's allowlist via the exported `PathPublic` and
`PathSystemFeatures` constants.

## Boundaries worth knowing

- Configuration is dynamic by design: a write publishes
  `config.item.changed`, every process invalidates its cache on the
  event *and* polls as an anti-loss backstop. Reads are not
  snapshot-consistent across replicas for the write's own duration —
  design pages that tolerate eventual config convergence.
- The `configs` table is platform data by deliberate exception: the
  system tier must stay visible to every tenant's fallback lookup. Do
  not copy the exception for tenant-owned data.

## Next steps

The full API lives in the `dbkit` and `config` module pages of the
module reference; see the tenancy and organizations domain page for
how repositories get their tenant.

## Source

- [dbkit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md)
- [config AGENTS.md](https://github.com/vislake/speed/blob/main/go/config/AGENTS.md)
- [tenancy AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md)
