# dbkit/audit

`audit` is the M1 audit-infrastructure round's persistence and declarative-collection home (docs/internal/10-compliance-and-audit.md; docs/internal/15-roadmap.md's M1 row): the `AuditEvent` model, its dual-dialect migrations, `Repository` (the append-only accessor that stores and reads events back), `Emit` (doc 10's declarative-secondary collection mechanism), and `Module`, the `pkgcore.Module` persister that subscribes to every collection mechanism's published events and calls `Repository.Insert`. The complementary automatic-collection mechanism — the GORM write-capture plugin — lives one level up, in `go/dbkit` itself (`audit_capture.go`), since it has to be wired into `dbkit.Open`; see `go/dbkit/AGENTS.md`'s own "Audit trail persistence" section for that half.

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
| `func (*Repository) Insert(ctx, *AuditEvent) error` | Appends one event; generates `ID` (UUID) when empty. A caller-supplied duplicate `ID` is a genuine error — Insert has no notion of "the same event happening again" |
| `func (*Repository) InsertIdempotent(ctx, *AuditEvent) error` | Like `Insert`, except a duplicate `ID` (`gorm.ErrDuplicatedKey`, translated from either dialect by `dbkit.Open`'s `TranslateError: true`) is a silent no-op rather than an error. Used only by `Module`'s three event subscribers, whose deterministically-derived `ID` (see "Multi-replica delivery", below) makes this dedup meaningful |
| `func (*Repository) Get(ctx, id string) (*AuditEvent, error)` | `(nil, nil)` when not found |
| `func (*Repository) ListByTenant(ctx, tenantID string) ([]AuditEvent, error)` | Newest first; the empty string returns platform-level events |
| `migrations.FS embed.FS` (`migrations/fs.go`) | The dual-dialect SQL, for a `dbkit.MigrationRegistry` |

`Repository` exposes no `Update` or `Delete` method at all — append-only for M1 is enforced by that absence (`repository_test.go`'s `TestRepository_HasNoUpdateOrDeleteMethod`, a reflection-based proof), not a runtime guard. The database-role/trigger backstop against a determined operator with raw database access, and the optional hash chain, are both explicitly M4.

### Collection — `emit.go`, `module.go`

| Signature | Purpose |
|---|---|
| `type Input struct { Action string; Resource Resource; Result Result; Changes *Diff }` (`emit.go`) | What a caller passes to `Emit`; identity is read from `ctx`, not `Input`, so every caller populates it the same way |
| `type Diff struct { Before, After map[string]any }` | An optional before/after change set for `Input.Changes`. Deliberately no `json` struct tags — every event payload type in this package and in `dbkit` relies on `encoding/json`'s default per-field-name behavior, so `module.go`'s wire-decode helpers can look every field up under one consistent capitalized key regardless of type |
| `func Emit(ctx, bus pkgcore.EventBus, actions pkgcore.AuditActionRegistrar, in Input) error` | Validates `in.Action` against `actions.Actions()` first — `ErrActionNotRegistered` for an undeclared action, closing the loop `go/config`'s own `AuditActionConfigSet` declaration left open — then reads `Actor`/`OnBehalfOf`/`TenantID` off `ctx` (identically to the automatic capture plugin) and publishes `EventRecorded`. A publish failure is returned to the caller, never swallowed |
| `const EventRecorded = "audit.event.recorded"`, `type RecordedEvent struct { ... }` | `Emit`'s own event type and payload, embedding `Actor`/`OnBehalfOf`/`TenantID` as plain fields — never left for a subscriber to re-derive from `ctx` — because the distributed deployment mode's `EventBus` delivers across a real network hop |
| `var ErrActionNotRegistered` | `Emit`'s rejection sentinel, matched with `errors.Is` |
| `func New(db *gorm.DB) *Module` (`module.go`) | Returns the persister `pkgcore.Module`. `db` must come from `dbkit.Open`, with `migrations.FS` already applied — identical contract to `config.NewModule` |
| `func (*Module) Register(reg *pkgcore.Registry) error` | Declares `dbkit.EventWriteCaptured` and `EventRecorded` on `reg.Events` (on `dbkit`'s behalf for the former, since `dbkit` itself is not a `pkgcore.Module`), declares `AuditActionSystemContextEntered` on `reg.AuditActions` (on `go/tenancy`'s behalf — see "Subscribing to `tenancy.system_context.entered`" below), and subscribes to all three event types |
| `const AuditActionSystemContextEntered` | The audit action `Module.Register` declares for `tenancy.EventSystemContextEntered`, since `go/tenancy` has no `pkgcore.Module` of its own to declare it |

### Subscribing to `tenancy.system_context.entered`

`Module` persists `go/tenancy`'s already-shipped `EventSystemContextEntered` (`tenancy.system_context.entered`) as an `AuditEvent`, closing doc 10's requirement that every use of the system context is itself an audit event. It does this **without importing package `tenancy`**: `go/tenancy`'s `go.mod` requires `go/dbkit` (`tenancy` sits above `dbkit` in the module dependency graph — `pkgcore -> dbkit -> tenancy -> config/jobs -> ...`), and this package is a subpackage of `dbkit` itself, so importing `tenancy` from here would make `dbkit` depend on `tenancy` — the same module-cycle conflict `model_test.go` already documents for `tenancytest.AssertNotTenantScoped`. `module.go`'s `tenancySystemContextEnteredEventType` constant duplicates the event-type string by hand instead, and `decodeSystemContextEntered` reads the payload structurally (by field name, via reflection for the concrete struct the standalone in-memory bus delivers, or via a `map[string]any` for the distributed bus's JSON shape) rather than type-asserting to `tenancy.SystemContextEnteredEvent`. This round's scope-freeze report left the exact wiring open (whether `tenancy` should instead gain its own minimal `pkgcore.Module` to declare this on its own behalf) — this is that decision, made in the implementing round; a future round can still give `tenancy` its own `Module` without changing this package.

## Multi-replica delivery

In distributed deployment mode with more than one replica, `pkgcore.RedisEventBus` delivers every published event to **every replica once each** (its own doc comment: "each event is delivered to every replica exactly once" — once per replica, not once system-wide, since most subscribers hold per-replica state rather than writing into one shared row). `Module`'s three subscribers do the opposite: every replica's `onWriteCaptured`/`onRecorded`/`onSystemContextEntered` writes into the SAME shared `audit_events` table, so a single real action independently reaches every replica's subscriber and, without deduplication, would leave one row per replica instead of one row total.

`Module` deduplicates by giving `AuditEvent.ID` a value **deterministically derived from the event's own content** — `module.go`'s `auditDeterministicEventID(eventType, occurredAt, parts...)`, a `uuid.NewSHA1` hash over the event type, an RFC3339Nano-normalized `occurredAt`, and a handler-specific tuple of fields (tenant, resource, actor, operation for `onWriteCaptured`; tenant, action, resource, actor for `onRecorded`; tenant, actor, purpose, ticket for `onSystemContextEntered`) — instead of leaving `ID` for `Repository.Insert` to generate randomly. Every replica computes the identical `ID` for the identical underlying event (the wire-decode helpers already normalize the concrete-struct and JSON-`map[string]any` shapes into the same Go values before this runs), so the second and any later replica's insert collides on the primary key; each subscriber calls `Repository.InsertIdempotent` rather than `Insert`, which turns that collision into a silent no-op instead of an error. `occurredAt` is taken at nanosecond precision specifically so two *genuinely different* actions essentially never collide — they would need to share tenant, resource, actor and operation AND land in the same nanosecond, which does not happen outside of the exact redelivery this exists to detect.

This only matters in distributed deployment mode with 2+ replicas; the standalone in-memory bus and a single-replica distributed deployment never redeliver, so the derived `ID` and `InsertIdempotent`'s dedup are inert (but harmless) there. See `repository_test.go`'s `TestRepository_InsertIdempotent_DuplicateID_IsANoOp` (the `Repository`-level proof) and `module_test.go`'s three `*_DeliveredToMultipleReplicas_PersistsExactlyOnce` tests (the end-to-end proof, through the real deterministic-ID derivation, one per subscriber).

## What this package does not (yet) do

- No query/report API beyond `ListByTenant`. The actor/resource/action/time-range/result search doc 10 describes, and its admin-console surface, are M4 (`go/compliance`) scope.
- No immutability enforcement beyond the application layer (no mutating method exists). Database-role revocation and the optional hash chain are M4.
- No retention/archival.

## Reference-app consumer

`examples/reference-app` is this package's mandatory first consumer (root `CLAUDE.md`'s "a module API it does not use is not done" rule): `internal/notes/handler.go`'s `NotesCreateNote` calls `Emit` explicitly (`recordNoteCreatedAudit`) after a note is created, under the already-declared `notes.note.create` audit action; `cmd/server/server.go` wires `audit.New(db)` into the app's `Kernel.Bootstrap` module set, sharing the notes module's own database connection — no new infra dependency. `cmd/server/server_test.go`'s `TestBuildServer_NoteCreate_PersistsAuditEvent` is the end-to-end proof: a real POST `/api/v1/notes` through the composed HTTP stack, then a second `dbkit.Open` connection to the same SQLite file reading the row back through a real `Repository.ListByTenant` call — not a mock, and not merely an in-memory event assertion (that narrower claim is `internal/notes/handler_test.go`'s `TestHandler_Create_ValidText_RecordsAuditEvent`).

`notes.Note` also implements `dbkit.Auditable` (`AuditResourceType() string { return "note" }`), but the reference app's own `db` deliberately does **not** set `dbkit.Options.AuditBus` on it — see `go/dbkit/AGENTS.md`'s "Audit trail collection" section, "Known limitation", for the same-SQLite-file deadlock this discovered, and `server.go`'s own doc comment on its `dbkit.Open` call for the app-specific write-up. The declarative `Emit` path above is what this app actually relies on for persistence; `Auditable` stays declared for when that mechanism is fixed or a host wires a dedicated audit connection.

## Testing

- `model_test.go` — `AuditEvent`'s flattening helpers, table name, and the not-tenant-scoped proof (see above).
- `repository_test.go` — `Repository`'s CRUD-minus-UD surface, including the reflection-based "no mutating method" proof; also carries `fakeAuditModule` and `openAuditTestDB`, the shared test-DB helper used by every test file in this package (both live here rather than duplicated per file).
- `migrations_test.go` — a real `dbkit.MigrationRegistry.Apply` round trip against SQLite (always) and, opportunistically, a local PostgreSQL instance if one is reachable at the conventional local dev endpoint (skipped, not failed, otherwise — mirroring `go/dbkit`'s own `migrations_test.go` precedent).
- `emit_test.go` — action-not-registered rejection, dual-identity and tenant population from `ctx`, the zero-value case when `ctx` carries none, and publish-failure propagation.
- `module_test.go` — `Register`'s declarations; a subscriber round trip for each of the three event types (`dbkit.EventWriteCaptured`, `EventRecorded`, `tenancy.system_context.entered`), including the JSON-`map[string]any` shape a real distributed-bus round trip produces (via an actual `encoding/json` marshal/unmarshal, not a hand-built map) for both `dbkit.WriteCapturedEvent` and the system-context payload; and an undecodable-payload case per handler, proving it is dropped rather than failing the handler chain.
- `example_test.go` — `Example` (direct `Repository` use) and `ExampleEmit` (the full collection path: register an action, wire `Module` into a `Registry`, call `Emit`, read the persisted row back through `Repository.ListByTenant`).

See `go/dbkit/AGENTS.md`'s "Audit trail persistence" section and its own `audit_capture_test.go` for the automatic-collection mechanism's tests (a real `dbkit.Open` with `AuditBus` set, an `Auditable` model write, a non-`Auditable` model proving the plugin leaves it alone, and the publish-failure-fails-the-write-loudly proof).
