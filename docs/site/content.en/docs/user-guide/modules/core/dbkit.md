---
title: dbkit
weight: 2
description: "The dual-dialect (PostgreSQL and SQLite) data-access layer — the sanctioned Open, the mandatory tenant-scoped Repository[T], versioned migrations, field-level encryption with blind indexes, and delete semantics from mark-delete to gated hard delete."
---

# dbkit

The dual-dialect data-access layer of a speed-based service: the only
sanctioned way to open a `*gorm.DB`, the mandatory tenant-scoped
`Repository[T]` business repositories embed, versioned SQL migrations,
field-level encryption with HMAC blind indexes, and the delete
semantics running from soft delete to system-gated physical erasure.

Everything dbkit does serves the three-layer tenant-isolation defense:
a GORM plugin (`Open` installs it on every connection) injects
`WHERE tenant_id = ?` and fails closed without a tenant; the generic
`Repository[T]` resolves the tenant from the context itself and
re-verifies it per row; and `WithTenantSession` runs every repository
call inside a transaction that sets the PostgreSQL session variable
(`app.current_tenant`) a deployment-provisioned RLS policy keys on.
Each layer holds even if the layers below it are bypassed.

## When to choose it

Any module that owns database rows uses it — there is no alternative
entry point: `gorm.Open` and the dialect drivers are never called
directly anywhere in the platform. Tenant-owned and link tables model
with `TenantScoped` (embed `TenantModel` or declare `TenantID` +
`GetTenantID()` yourself when the `(tenant_id, id)` primary key
matters) and are accessed through `Repository[T]`; identity and
platform data (a `users` table, platform-wide plans) must **not**
implement `TenantScoped`, so they use the plain `*gorm.DB` from
`Open` — safe, because the plugin ignores non-tenant-scoped models,
and proven by `tenancytest.AssertNotTenantScoped`.

## Wiring and minimal use

```go
import _ "github.com/vislake/speed/go/dbkit/dialect/sqlite" // or /dialect/postgres

type Subscription struct {
    ID       string `gorm:"primaryKey;size:26"`
    TenantID string `gorm:"primaryKey;size:26;not null"`
    PlanID   string `gorm:"size:64;not null"`
    Status   string `gorm:"size:32;not null"`
}

func (s Subscription) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

type Repository struct {
    *dbkit.Repository[Subscription]
}

func NewRepository(db *gorm.DB) *Repository {
    return &Repository{Repository: dbkit.NewRepository[Subscription](db)}
}
```

Opening the connection and applying migrations (each module ships its
own `postgres/*.sql` and `sqlite/*.sql` sets; `Apply` sorts by
`DependsOn` and runs one transaction per module):

```go
db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
// handle err

reg := dbkit.NewMigrationRegistry()
if err := reg.Register(billingModule); err != nil { // billingModule: your pkgcore.Module
    // handle err
}
if err := reg.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
    // handle err
}
```

Repository calls take the tenant from the context — a request context
already carries it once `tenancy.Middleware` ran, and a background job
rebuilds it with `pkgcore.WithTenant`. `Create` overwrites whatever
the struct's `TenantID` field was set to; `FindByID`, `Update`,
`Delete`, `Restore` and `HardDelete` all answer
`dbkit.ErrRecordNotFound` for "no such id" *and* "belongs to another
tenant" — deliberately indistinguishable.

## Core concepts and API essentials

- **`Open`** — validates the dialect (blank-importing the driver
  subpackage registers it; otherwise `dbkit.invalid_dialect` names the
  fix), applies fixed pool limits (25 open / 5 idle), pings, and only
  then installs the plugins. The DSN is never logged or echoed in an
  error. Every SQLite connection carries a fixed 5-second
  `busy_timeout`; a read-then-write lock upgrade still fails fast with
  `SQLITE_BUSY` and is the caller's retry problem.
- **`Repository[T]`** — deliberately minimal: `Create`, `FindByID`,
  `Update` (full-record save, never a partial patch), `Delete`, and
  `List` (every row of the tenant; no filters, no pagination — richer
  queries are built on the plugin-protected `*gorm.DB`, anchored on
  `db.Model(&TenantScoped{})` so the filter still applies).
- **Delete semantics** — a model carrying
  `SoftDeletable` (`DeletedAt`/`DeletedBy` fields) gets a mark-delete
  on `Delete` and a working `Restore`; soft-deleted rows are hidden
  from reads by a plugin but stay plaintext in the table — not
  compliance-grade erasure. `HardDelete` is the physical erase:
  irreversible, and refused with
  `ErrHardDeleteRequiresSystemContext` unless the context carries a
  system context (whose grant holders are the caller-side whitelist —
  admin, compliance, jobs, authn — entering through tenancy's audited
  wrapper). Even then the tenant stays binding: it never deletes
  across tenants.
- **Encryption and lookup** — `dbkit.Cipher` is AES-256-GCM field
  encryption registered once at bootstrap
  (`RegisterEncryptedSerializer`); an encrypted field is unqueryable,
  so equality lookups go through a `BlindIndexer` on a separate
  plain 64-hex column — `NewBlindIndexer("email_index", key,
  dbkit.NormalizeEmail)`, written with `Index(raw)`, queried with
  `Equal(raw)`, both sides normalizing identically. Keys are 32 bytes,
  must never be reused across cipher and index, and may all be derived
  from one root secret with `dbkit.DeriveKey(root, "purpose.v1")`.
- **Audit collection** — optional `Options.AuditBus` (plus
  `AuditModels`) captures every create/update/delete against an
  `Auditable` model as `dbkit.write.captured`, published only after
  the write's transaction committed; `go/dbkit/audit` supplies the
  append-only `AuditEvent` model, its migrations, an `Emit` path and
  the persister `Module`.
- **Test helpers** — `dbkit/dbtest`'s `NewSQLite(t)`/`NewPostgres(t)`
  return plugin-wired connections for module suites; the mandatory
  isolation assertions live in `tenancy`'s `tenancytest`.

## Boundaries and pitfalls

- Business repositories never hold a raw `*gorm.DB` and write
  queries; `Repository[T]` is the base, and the three bypass points
  (`db.Table`/`db.Model`/`db.Raw`) are semgrep-checked in CI.
- No `AutoMigrate` anywhere: migrations are versioned SQL per dialect,
  one set per module. No PostgreSQL-only features
  (`gen_random_uuid()`, JSONB operators, native arrays, `NOW()`):
  IDs are application-generated, JSON rides `datatypes.JSON`,
  timestamps use gorm's `autoCreateTime`/`autoUpdateTime`.
- A tenant-less context fails closed (`pkgcore.ErrNoTenant`), in a
  worker too — rebuild the context inside the job.
- A blind-index key rotation has no retired-key fallback: every row's
  index must be recomputed as a batch job.
- `Repository[T]` structurally excludes identity and platform data —
  that is the point; do not force `TenantScoped` on them.

## Source

- [dbkit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md)
- [dbkit `example_test.go`](https://github.com/vislake/speed/blob/main/go/dbkit/example_test.go)
- [dbkit/audit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/audit/AGENTS.md)
