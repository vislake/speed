# dbkit/audit

`audit` is the M1 audit-infrastructure round's persistence half (docs/internal/10-compliance-and-audit.md; docs/internal/15-roadmap.md's M1 row): the `AuditEvent` model, its dual-dialect migrations, and `Repository`, the append-only accessor that stores and reads it back.

## Module home

This package lives inside `go/dbkit`, not as its own `go.work` module and not inside `go/compliance` (a stub until M4). In short:

- `docs/internal/15-roadmap.md`'s M1 row lists "audit infrastructure" as a bare capability, unlike every other M1 deliverable in the same sentence, which is not owned by a named module.
- `docs/internal/10-compliance-and-audit.md`'s own delivery-phase correction: collection lands in M1; `go/compliance` (M4) owns the governance layer — retention, archival, search/report, right-to-be-forgotten, data portability.
- `go/pkgcore` cannot host persistence (no GORM, no DB) — but it already has the right declaration surface: `Registry.AuditActions` was already implemented, and already had one real caller (`go/config`) before this round, whose own `go/config/events.go` said outright: "declaring the enumeration is what keeps the future audit-log consumer's vocabulary complete." This package is that consumer.
- `go/tenancy` was ruled out as a database-touching home by its own `AGENTS.md`: its production code never touches a database, and it does not implement `pkgcore.Module`.
- `go/dbkit` already owns the one existing GORM-callback plugin (`tenant_scope.go`), `MigrationRegistry`, and — most directly — the "a real, indexed `tenant_id` column that must NOT implement `dbkit.TenantScoped`" precedent this table needs, already shipped twice over: `go/jobs`'s `jobRecord` and `go/config`'s `row`. `go/dbkit/**` is also already wholesale-allowlisted in `tools/semgrep_rules/raw-gorm-bypass.yml`.
- Net new dependency edges this package adds: none. It depends only on `go/dbkit` (root) and `go/pkgcore`, both of which `dbkit` already depends on or is depended on by.

## Data-domain classification

An audit event is neither purely tenant data nor purely platform data — docs/internal/10-compliance-and-audit.md notes that the audit log carries both tenant-level and platform-level entries. `AuditEvent` is treated as **platform data with a real, non-enforced `tenant_id` column** — the same treatment `go/jobs`'s `jobRecord` and `go/config`'s `row` already get. It runs the reverse-and-equally-important proof (`model_test.go`'s `TestAuditEvent_DoesNotImplementTenantScoped` and `TestAuditEvent_VisibilityDoesNotDependOnTenantContext`), never `tenancytest.AssertIsolated` — `AuditEvent` does not, and must not, implement `dbkit.TenantScoped`.

**Why not literally `tenancytest.AssertNotTenantScoped`**, the way `go/config` and `go/jobs` prove the identical property of their own platform-data models: `go/tenancy/tenancytest` imports `go/dbkit` itself, and `go/tenancy` sits *above* `go/dbkit` in the module dependency graph (`pkgcore -> dbkit -> tenancy -> config/jobs -> ...`). This package is a subpackage of `dbkit`, so importing `tenancytest` from here would make `dbkit` depend on `tenancy` — inverting the direction root `CLAUDE.md`'s "Dependencies flow strictly bottom-up" rule requires, and reintroducing exactly the module cycle (`tenancy` already requires `dbkit`) that rule exists to rule out. `model_test.go` reproduces `AssertNotTenantScoped`'s two checks by hand instead, using only `dbkit` and `pkgcore`.

## Public API

| Signature | Purpose |
|---|---|
| `type AuditEvent struct { ... }` (`model.go`) | The append-only row. `TableName() string` returns `"audit_events"` |
| `type Resource struct { Type, ID, DisplayName string }` | What an action was performed on |
| `type Result struct { Success bool; FailureReason string }` | An action's outcome |
| `func (*AuditEvent) SetActor(pkgcore.Actor)` / `Actor() pkgcore.Actor` | The acting identity |
| `func (*AuditEvent) SetOnBehalfOf(*pkgcore.Actor)` / `OnBehalfOf() (pkgcore.Actor, bool)` | The real administrator behind an impersonated `Actor`, if any — genuinely nullable columns, never an empty-string sentinel |
| `func (*AuditEvent) SetResource(Resource)` / `Resource() Resource` | |
| `func (*AuditEvent) SetResult(Result)` / `Result() Result` | |
| `func NewRepository(db *gorm.DB) *Repository` (`repository.go`) | `db` must come from `dbkit.Open`, with `migrations.FS` already applied |
| `func (*Repository) Insert(ctx, *AuditEvent) error` | Appends one event; generates `ID` (UUID) when empty |
| `func (*Repository) Get(ctx, id string) (*AuditEvent, error)` | `(nil, nil)` when not found |
| `func (*Repository) ListByTenant(ctx, tenantID string) ([]AuditEvent, error)` | Newest first; the empty string returns platform-level events |
| `migrations.FS embed.FS` (`migrations/fs.go`) | The dual-dialect SQL, for a `dbkit.MigrationRegistry` |

`Repository` exposes no `Update` or `Delete` method at all — append-only for M1 is enforced by that absence (`repository_test.go`'s `TestRepository_HasNoUpdateOrDeleteMethod`, a reflection-based proof), not a runtime guard. The database-role/trigger backstop against a determined operator with raw database access, and the optional hash chain, are both explicitly M4.

## What this package does not (yet) do

- No collection mechanism. Nothing calls `Repository.Insert` yet — the automatic GORM write-capture plugin, the explicit `Emit` function, and the `pkgcore.Module` persister that subscribes to their published events land alongside this same round, layered directly on the types above.
- No query/report API beyond `ListByTenant`. The actor/resource/action/time-range/result search doc 10 describes, and its admin-console surface, are M4 (`go/compliance`) scope.
- No immutability enforcement beyond the application layer (no mutating method exists). Database-role revocation and the optional hash chain are M4.
- No retention/archival.

## Testing

- `model_test.go` — `AuditEvent`'s flattening helpers, table name, and the not-tenant-scoped proof (see above).
- `repository_test.go` — `Repository`'s CRUD-minus-UD surface, including the reflection-based "no mutating method" proof; also carries `fakeAuditModule` and `openAuditTestDB`, the shared test-DB helper used by every test file in this package (both live here rather than duplicated per file).
- `migrations_test.go` — a real `dbkit.MigrationRegistry.Apply` round trip against SQLite (always) and, opportunistically, a local PostgreSQL instance if one is reachable at the conventional local dev endpoint (skipped, not failed, otherwise — mirroring `go/dbkit`'s own `migrations_test.go` precedent).
