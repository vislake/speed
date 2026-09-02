# dbkit

dbkit is speed's dual-dialect (PostgreSQL and SQLite) data-access layer: a safety-wrapped way to open a `*gorm.DB` (`Open`), the mandatory generic tenant-scoped `Repository[T]` base every business module's repository embeds instead of holding a raw connection, `MigrationRegistry` for aggregating and applying each module's own versioned SQL migrations in dependency order, and field-level AES-256-GCM encryption with HMAC blind indexes for sensitive columns. It sits directly above `pkgcore` in the module dependency graph (`pkgcore -> dbkit / observability -> tenancy -> ...`) and implements the Go-side wiring for all three of the project's mandatory tenant-isolation layers — including layer 3's session-variable step, `WithTenantSession`, though provisioning the PostgreSQL role and RLS policy that step depends on remains a deployment-side responsibility this package assumes but does not create (see Out of scope).

| Concern | Where |
|---|---|
| Opening a `*gorm.DB` for either dialect, with fixed pool limits and the tenant-scoping plugin pre-installed | `open.go` |
| The tenant-scoping GORM plugin (isolation layer 1) and the `TenantScoped` / `TenantModel` types it and `Repository[T]` both key off | `tenant_scope.go` |
| The generic tenant-scoped `Repository[T]` base (isolation layer 2) | `repository.go` |
| PostgreSQL row-level-security session wiring (isolation layer 3): `WithTenantSession`, which every `Repository[T]` method now routes through | `tenant_session.go` |
| Aggregating and applying every registered `pkgcore.Module`'s versioned SQL migrations | `migrations.go` |
| Field-level AES-256-GCM encryption (`Cipher`) and its GORM serializer wiring | `encryption.go` |
| The HMAC blind-index mechanism for exact-match lookups on encrypted columns (`BlindIndexer`, raw `BlindIndex`, `NormalizeEmail`, `NormalizePhoneE164`) | `blind_index.go` |
| The dual-dialect test-database helpers every module's tests use (backend coding standard §13's mandatory suite) — a publicly importable package, unlike `internal/testutil` | `dbtest/` |

**One dependency, and why there is only one.** dbkit imports exactly one other speed module: `pkgcore` — its root package, for `pkgcore.TenantID` / `pkgcore.WithTenant` / `pkgcore.MustTenantFromContext` and the `pkgcore.Module` interface `MigrationRegistry` aggregates over, plus `pkgcore/apperr` for every error dbkit returns. (`internal/testutil`, used only by dbkit's own tests, also imports `pkgcore`, so its `Widget` fixture can implement `GetTenantID`.) This is architectural, not incidental minimalism:

- **dbkit cannot depend on `tenancy`.** The dependency graph is `pkgcore -> dbkit -> tenancy -> ...`; `tenancy` itself depends on dbkit for `dbkit.Repository[T]` and `dbkit.TenantScoped` (which `tenancytest`'s isolation assertions build on) — the GORM tenant-isolation plugin itself lives entirely in dbkit's own `tenant_scope.go`, not in tenancy — so an import running the other way would be a cycle. This is also why dbkit calls `pkgcore.MustTenantFromContext` directly instead of going through `tenancy`'s eventual audited wrapper — `docs/internal/04-data-and-tenancy.md` documents this explicitly as an implementation correction: anything at or below dbkit's layer uses the raw `pkgcore` primitives; only code built above `tenancy` gets the audited convenience wrapper around them.
- **dbkit cannot depend on `observability` either**, for the identical bottom-up reason (`pkgcore -> dbkit / observability -> ...` — the two sit at the same graph depth, neither above the other). That is why `Open` silences GORM's own query logger (`gormlogger.Default.LogMode(gormlogger.Silent)`) instead of routing it through the structured logger the rest of the codebase uses — see `Open`'s doc comment in `open.go`. A caller that wants query logs takes them from the returned error and logs it through its own context logger instead.

Beyond `pkgcore`, dbkit's dependencies are exactly the third-party pieces the job requires: `gorm.io/gorm`, the two dialect drivers (`gorm.io/driver/postgres` and the pure-Go, CGO-free `github.com/glebarez/sqlite`), and, for the integration tier only, `github.com/jackc/pgx/v5` and `testcontainers-go`.

**Out of scope.** Business queries beyond create / read-by-id / update / delete / list-all (`Repository[T]` is deliberately minimal — see Known limitations); the audited, business-facing wrapper around the system-context escape hatch (now `tenancy.WithSystemContext`); provisioning the PostgreSQL roles and RLS policies isolation layer 3 depends on, which is a deployment-side responsibility this package assumes but does not create.

## The three-layer tenant isolation defense

Multi-tenant isolation is speed's highest-priority rule (backend coding standard §3): "forgot to filter by tenant" is treated as a security defect, not a style issue, so it is never left to one mechanism alone. dbkit implements the Go-side wiring for all three independent layers; each is designed to hold even if the layer(s) below it were absent, misconfigured, or bypassed.

| Layer | Mechanism | File | Status |
|---|---|---|---|
| **1 — GORM plugin** | The unexported `tenantScopePlugin`, installed automatically by `Open` on every connection it returns. For any model implementing `TenantScoped`, it injects `WHERE tenant_id = ?` ahead of every query/update/delete callback (reading the tenant from `pkgcore.TenantFromContext(db.Statement.Context)`) and forces the `tenant_id` column to that tenant on every create, overwriting whatever the caller populated the struct with. Fails the statement closed (`ErrMissingTenantContext`) when the context carries no tenant. | `tenant_scope.go` | Implemented. Unit-tested in `tenant_scope_test.go`; integration-tested against real PostgreSQL in `integration_test/postgres_tenant_isolation_test.go` |
| **2 — `Repository[T]`** | Resolves the tenant from `ctx` itself, before ever touching the database — it does not trust layer 1 to catch a missing tenant. Scopes every query it issues by `tenant_id` explicitly, re-verifies the tenant on every row it hands back (`FindByID`), and fails closed the same way. Its checks are independently complete: it enforces isolation correctly even given a `*gorm.DB` not obtained from `Open` — which is exactly how dbkit's own unit tests exercise it, against a plugin-less SQLite connection. That is a valid way to unit-test a `Repository[T]`; it is not a valid way to run one in production. Every method's actual database call now routes through `WithTenantSession` (layer 3's wiring, below), so every `Repository[T]` call also runs inside an explicit transaction — a real, deliberately accepted overhead, in exchange for layer 3 being reachable through the one sanctioned data-access path instead of only through code that remembers to call `WithTenantSession` itself. | `repository.go` | Implemented. Same test files as layer 1, plus `repository_test.go` |
| **3 — PostgreSQL row-level security** | A restricted, non-`BYPASSRLS` application database role plus a policy keyed on a per-request/transaction session setting (`current_setting('app.current_tenant', true)`), enforced by the database itself, below the Go layer entirely — the backstop meant to hold even if layers 1 and 2 are both bypassed (raw SQL, a `*gorm.DB` not obtained from `Open`, a bug in either layer). | `tenant_session.go` | **Session-variable wiring implemented**: `WithTenantSession` sets the GUC inside an explicit transaction when connected to PostgreSQL. Provisioning the role and the policy themselves remains a deployment-side responsibility (see Out of scope) |

**Layer 3, as of this writing.** `WithTenantSession(ctx, db, fn)` (`tenant_session.go`) resolves the tenant from `ctx` the same fail-closed way everything else in this package does, then runs `fn` inside `db.WithContext(ctx).Transaction(...)`; when `db.Name() == "postgres"`, it first runs `SELECT set_config('app.current_tenant', ?, true)` as the transaction's first statement, before `fn` ever runs. Every `Repository[T]` method now routes its actual database call through it (see `repository.go`), and it is exported so the raw-SQL escape hatch below can opt into the same protection.

The database-side mechanism itself is proven in `integration_test/postgres_rls_test.go` — a restricted role and an RLS policy set up entirely by hand over `database/sql`, with no dbkit code involved. `integration_test/postgres_tenant_session_test.go` goes further and proves `WithTenantSession` itself (not hand-written SQL) is what drives a real RLS policy correctly end to end, including that the GUC it sets does not leak across calls sharing one pooled connection.

**Why `SELECT set_config('app.current_tenant', ?, true)`, not `SET LOCAL app.current_tenant = ?`.** PostgreSQL's `SET`/`SET LOCAL` statements do not accept a bound query parameter for the value being set — `SET LOCAL app.current_tenant = $1` is rejected outright with a syntax error (`SQLSTATE 42601`), confirmed against a live server while this was built, regardless of driver or ORM. `set_config(name, value, is_local)` is an ordinary function call, so its `value` argument is a normal bound parameter (keeping the tenant id out of the SQL text, with no bespoke escaping to get right), and `is_local = true` gives it the identical transaction-scoped, revert-on-`COMMIT`-or-`ROLLBACK` semantics as `SET LOCAL` — also confirmed against a live server. Do not "simplify" `tenant_session.go` back to a literal `SET LOCAL` statement with the tenant id interpolated into the SQL string; see that file's own doc comment for the full detail.

## Public API

### Connections — `open.go`

| Signature | Purpose |
|---|---|
| `type Dialect string`, constants `DialectPostgres`, `DialectSQLite` | The two SQL dialects dbkit supports. Any other value, including the zero value, is rejected |
| `type Options struct { Dialect Dialect; DSN string }` | `Open`'s configuration. `DSN` is driver-specific (a libpq/pgx URL for Postgres; a file path or `file::memory:?cache=shared` for SQLite) and is never logged or included in a returned error |
| `func Open(ctx context.Context, opts Options) (*gorm.DB, error)` | The ONLY sanctioned way to obtain a `*gorm.DB` anywhere in this codebase. Validates `opts.Dialect`, opens the matching driver, applies fixed connection-pool limits (25 max open / 5 max idle / 30 min max lifetime — not configurable through `Options`, on purpose, since every connection in this codebase shares the same pool shape), pings bound to `ctx`, installs the layer-1 tenant-scoping plugin, and only then returns — no caller can end up holding an "unprotected" handle by accident. Returns `apperr.Invalid("dbkit.invalid_dialect")` for a bad dialect, `apperr.Internal("dbkit.connect_failed")` for a driver or connectivity failure |

### Tenant scoping — `tenant_scope.go`

| Signature | Purpose |
|---|---|
| `type TenantScoped interface { GetTenantID() pkgcore.TenantID }` | Marker interface opting a model into both isolation layers. `GetTenantID` is never actually called by the plugin — it is used purely as a marker, and the tenant to filter or populate by always comes from context, never the model. That is what makes `Create`'s tenant-forcing real: a caller cannot get a row inserted under a different tenant by populating the struct differently |
| `type TenantModel struct { TenantID string }` — tag `` `gorm:"column:tenant_id;not null;index:,priority:1"` `` | Embeddable base satisfying `TenantScoped` and giving the plugin its known column, for a model that does not need `tenant_id` in its primary key. Note: the tag does **not** include `primaryKey` (see Known limitations) — do not "fix" that by shadowing the field from the embedding struct; that silently breaks `GetTenantID` instead (see the type's own doc comment and `tenant_scope_tenantmodel_test.go`, which embeds it and proves both the working path and that failure mode). `internal/testutil.Widget` needs the composite key, so it declares `TenantID` directly instead, with its own `primaryKey` tag, rather than embedding `TenantModel` |
| `func (TenantModel) GetTenantID() pkgcore.TenantID` | Satisfies `TenantScoped` |
| `var ErrMissingTenantContext = apperr.Internal("dbkit.tenant_context_required")` | The plugin's own fail-closed error (with the `pkgcore` error attached via `WithCause`) when a tenant-scoped statement runs on a context carrying no tenant |
| `var ErrTenantIDImmutable = apperr.Invalid("dbkit.tenant_id_immutable")` | Returned when an `Update`/`Updates` payload against a tenant-scoped model tries to set `tenant_id` to a different tenant than the context's |

### `Repository[T]` — `repository.go`

| Signature | Purpose |
|---|---|
| `type Repository[T TenantScoped] struct { ... }` | Generic, tenant-scoped data-access base (isolation layer 2). The zero value is not usable — construct with `NewRepository`. `T` must additionally have exported `ID` and `TenantID` string fields by those exact names — a documented convention `TenantScoped` cannot express in the type system, enforced at first use instead of at compile time |
| `func NewRepository[T TenantScoped](db *gorm.DB) *Repository[T]` | `db` is expected to come from `Open` (for layer 1 underneath); `Repository[T]`'s own checks are independently complete regardless of how `db` was obtained |
| `func (r *Repository[T]) Create(ctx context.Context, m *T) error` | Resolves the tenant from `ctx`, overwrites `m`'s `TenantID` field with it regardless of what the caller set, inserts. No tenant in `ctx` → `pkgcore`'s error returned unmodified, before the database is touched |
| `func (r *Repository[T]) FindByID(ctx context.Context, id string) (*T, error)` | Tenant-scoped lookup by `id`. `ErrRecordNotFound` for both "no such id" and "that id belongs to another tenant" — deliberately indistinguishable, so a caller can never learn from the response alone that an id exists under a tenant it cannot access |
| `func (r *Repository[T]) Update(ctx context.Context, m *T) error` | Full-record save (`Select("*")`, never a partial patch) over the row identified by `m`'s `ID`, scoped to `ctx`'s tenant; overwrites `m.TenantID` the same way `Create` does. `ErrMissingID` for an empty id (checked before touching the database); `ErrRecordNotFound` when nothing matches, including a cross-tenant id. For a `T` whose only columns are `id` and `tenant_id` (a pure marker/link record with no other field), a successful call is a verified no-op touch rather than a real write — see the Rules section below |
| `func (r *Repository[T]) Delete(ctx context.Context, id string) error` | Tenant-scoped delete by `id`. `ErrRecordNotFound` when nothing matches |
| `func (r *Repository[T]) List(ctx context.Context) ([]T, error)` | Every row belonging to `ctx`'s tenant. No filter or pagination parameters — see Known limitations |
| `var ErrRecordNotFound = apperr.NotFound("dbkit.record_not_found")` | |
| `var ErrMissingID = apperr.Invalid("dbkit.missing_id")` | |

### `MigrationRegistry` — `migrations.go`

| Signature | Purpose |
|---|---|
| `func NewMigrationRegistry() *MigrationRegistry` | An empty registry, ready for `Register`. Safe for concurrent use |
| `func (r *MigrationRegistry) Register(m pkgcore.Module) error` | Adds `m`. Reads `m.Name()` / `m.DependsOn()` / `m.Migrations()` at `Apply` time — a `pkgcore.Module` is self-describing, so callers never pass those separately. Rejects a nil `m`, an empty `Name()`, or a `Name()` already registered; does **not** validate `DependsOn` here, since a module may legitimately be registered before the modules it depends on |
| `func (r *MigrationRegistry) Apply(ctx context.Context, db *gorm.DB, dialect Dialect) error` | Topologically sorts the registered modules by `DependsOn` (failing on a cycle or a missing dependency before running anything), creates dbkit's own `schema_migrations` bookkeeping table if absent, then for each module, in dependency order, applies its not-yet-applied files from `m.Migrations()`'s `<dialect>/*.sql` subdirectory in filename order — inside one transaction per module, so a module ends up either fully applied or, on any failure, entirely unchanged, and a later module's failure never rolls back an earlier one already committed. Calling `Apply` again once nothing has changed is a no-op |
| `var ErrNilModule`, `ErrEmptyModuleName`, `ErrDuplicateModule` | `Register`-time errors, plain `errors.New` sentinels |
| `var ErrDependencyCycle`, `ErrMissingDependency`, `ErrUnknownDialect` | `Apply`-time errors |

A module's `Migrations()` embed.FS must expose `postgres/*.sql` and `sqlite/*.sql` at its own root. Because a `//go:embed` directive's patterns resolve relative to the directory of the `.go` file that carries it, the embedding file must itself live in the one directory where `postgres/` and `sqlite/` are its immediate children — see `internal/migrationfixture/basemodule/fs.go` and `.../derivedmodule/fs.go` for the pattern every real module's own migration package should copy.

### Encryption — `encryption.go`

| Signature | Purpose |
|---|---|
| `type Cipher struct { ... }` | Authenticated, randomized field-level encryption with AES-256-GCM. Holds one active key and zero or more retired keys; the zero value is not usable. Safe for concurrent use |
| `func NewCipher(activeKey []byte, retiredKeys ...[]byte) (*Cipher, error)` | Every key, active or retired, must be exactly 32 bytes (`ErrInvalidKeySize` otherwise — no lenient fallback). Rotate by constructing a new `Cipher` with the new key as active and the previous active key appended to `retiredKeys` |
| `func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error)` | Seals under the active key with a freshly generated random nonce every call — nonce reuse under AES-GCM is a confidentiality break, not a cosmetic property, which is why the nonce is never a parameter a caller could get wrong |
| `func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error)` | Tries the active key, then each retired key in the order given to `NewCipher`. `ErrDecryptionFailed` if none authenticate — deliberately indistinguishable between "wrong/missing key" and "corrupted or tampered ciphertext" |
| `func RegisterEncryptedSerializer(name string, cipher *Cipher)` | Wires `cipher` into GORM's process-global serializer registry under `name`, so any field tagged `` `gorm:"serializer:<name>"` `` is transparently encrypted on write and decrypted on read. Call once per name at bootstrap (alongside other `Module.Register`-time wiring), never inside a request path; calling it again with the same name replaces the active cipher for every model using it — how a completed key rotation switches the active key for new writes |
| `var ErrInvalidKeySize`, `ErrDecryptionFailed` | Plain `errors.New` sentinels. `ErrInvalidKeySize` is shared: both `NewCipher` and `NewBlindIndexer` (blind_index.go) return it — wrapped — for a key that is not exactly 32 bytes |

### Blind indexes — `blind_index.go`

The companion mechanism for a queryable encrypted field: when a column's values are encrypted at rest (above), the only way to find a row by that value without decrypting the whole table is a deterministic, keyed index of it stored in a separate plain column — `HMAC(secret, normalize(plaintext))` per `docs/internal/10-compliance-and-audit.md`. Equality-only by construction: a deterministic index supports exact matches and nothing else (no prefix, partial, or fuzzy search — building that would leak the structure the encryption exists to hide), and key rotation has no retired-key fallback because a column holds one index per row (unlike `Decrypt`'s chain); both limits are deliberate.

| Signature | Purpose |
|---|---|
| `func BlindIndex(key []byte, normalized string) string` | The raw primitive: deterministic HMAC-SHA256 of `normalized` under `key`, hex-encoded. `normalized` must already be in the column's canonical form — `BlindIndex` performs no normalization of its own, on purpose. Prefer a `BlindIndexer` (below), which bundles normalization, key, and column so the two sides of a lookup cannot drift apart |
| `type NormalizeFunc func(raw string) (string, error)` | A column's canonical form: caller input in, the exact string the index is computed over out, or an error when the input has no canonical form. Must be deterministic and idempotent; erroring — never best-effort "fixing up" — is what keeps a bad value from silently indexing into a value no lookup will ever match. The two built-ins implement the canonical forms the design promises; a different form supplies its own function and documents it next to the model's column declaration |
| `func NormalizeEmail(raw string) (string, error)` | Trims surrounding whitespace and lowercases (`" User@Example.COM "` → `"user@example.com"`). Errors on empty or whitespace-only input: an absent value has no canonical form and must not be indexed — leave the index column NULL |
| `func NormalizePhoneE164(raw string) (string, error)` | Drops the separators space, `-`, `(`, `)`, `.` (`"+1 555-0100"` → `"+15550100"`). Requires a leading `+` and at most 15 digits — E.164's limit — and errors on a bare national number (no default country is ever assumed), extensions, letters, or anything else that cannot be canonicalized |
| `type BlindIndexer struct { ... }` | The per-column contract: one SQL column, its HMAC key, and its `NormalizeFunc` bound together, so write and query sides of a lookup cannot drift apart by accident. No key accessor (mirrors `Cipher`); zero value not usable; safe for concurrent use. Tenancy is deliberately outside it: the column sits in the same (shared) table as the encrypted field and the surrounding data-access layer's tenant scoping applies to queries on it exactly as to any other column |
| `func NewBlindIndexer(column string, key []byte, normalize NormalizeFunc) (*BlindIndexer, error)` | `column` is the index column's exact SQL name (the model field's `column:` tag); `key` must be exactly 32 bytes — `ErrInvalidKeySize`, the sentinel `NewCipher` shares, so both secrets keep one key shape in the secret manager — and is copied at construction. Empty `column` or nil `normalize` is an error. Construct at bootstrap, next to `RegisterEncryptedSerializer`, where secrets are injected |
| `func (b *BlindIndexer) Index(raw string) (string, error)` | The write side: normalizes `raw` under the column's normalizer and returns the 64-hex value to store in the index column — computed over the same plaintext the serializer encrypts, never over ciphertext. Errors on input with no canonical form; it never yields an empty or best-effort index, so an index column holding `""` is always a write-path bug |
| `func (b *BlindIndexer) Equal(raw string) (clause.Eq, error)` | The query side: normalizes `raw` exactly as `Index` did and returns the column's equality condition, composable straight into `db.Where(...)`. Takes raw input, never a precomputed index value, so a filter under different normalization is impossible except through a second, deliberately constructed `BlindIndexer` on the same column (visible in bootstrap code, not an accident of a call site). The returned clause carries no tenant filter — whatever `WHERE tenant_id = ?` the plugin or `Repository[T]` appends applies as usual |

The model declares the index column explicitly, as an ordinary — never a serializer — indexed column next to the encrypted field (a serializer on it would re-encrypt the index; a blind index column holds plain 64-hex text, and the schema must stay visible to the gorm structs versioned migrations are generated from):

```go
type Account struct {
	Email      string `gorm:"serializer:email_enc"`             // encrypted at rest
	EmailIndex string `gorm:"column:email_index;size:64;index"` // blind index
}
```

Both sides of the lookup funnel through the same `BlindIndexer`, constructed at bootstrap where the secrets are injected — write side via `Index` (store its result in `EmailIndex` when creating or updating, alongside the plaintext being encrypted), query side via `Equal`:

```go
emailIndex, err := dbkit.NewBlindIndexer("email_index", blindKey, dbkit.NormalizeEmail)
if err != nil {
	return err // key misconfiguration: fail the bootstrap, never start with a mechanism that would silently write unusable indexes
}

indexValue, err := emailIndex.Index(form.Email) // normalize, then HMAC — the same value the serializer encrypts
account := Account{Email: form.Email, EmailIndex: indexValue}

cond, err := emailIndex.Equal(form.Email) // normalize identically at query time
var got Account
if err := db.Where(cond).First(&got).Error; err != nil { ... }
```

### RLS session state — `tenant_session.go`

| Signature | Purpose |
|---|---|
| `func WithTenantSession(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error` | Runs `fn` inside `db.WithContext(ctx).Transaction(...)` (isolation layer 3's wiring). Resolves the tenant from `ctx` via `pkgcore.MustTenantFromContext` and fails closed — before any transaction is opened — when there is none; every `Repository[T]` method already makes this same check before calling `WithTenantSession`, and the duplication is intentional. When `db.Name() == "postgres"`, runs `SELECT set_config('app.current_tenant', ?, true)` as the transaction's first statement before `fn`, aborting the whole transaction (returning that error, never calling `fn`) if it fails. On SQLite, a plain transaction wrapper with no GUC step. Exported so the raw-SQL escape hatch (Known limitations, below) can opt into the same protection instead of reinventing it |

See "The three-layer tenant isolation defense" above for why the GUC step is a `set_config(...)` function call rather than a literal `SET LOCAL ... = ?` statement (the latter is a PostgreSQL syntax error, not a style choice).

### Test helpers — `dbtest/`

The backend coding standard (§13) names `dbtest.NewPostgres(t)` and `dbtest.NewSQLite(t)` as the mandatory dual-dialect test suite every module's tests should use. `internal/testutil` cannot be that suite — it is unexported, reachable only from within the dbkit module itself — so `dbtest` exists as its own publicly importable package (`github.com/vislake/speed/go/dbkit/dbtest`) specifically so other modules' tests can import it too.

| Signature | Purpose |
|---|---|
| `func NewSQLite(t *testing.T) *gorm.DB` | A `*gorm.DB` (via `Open`) backed by a private, per-call temp-file SQLite database. Cleaned up via `t.Cleanup` |
| `func NewPostgres(t *testing.T) *gorm.DB` | A `*gorm.DB` (via `Open`) backed by a real, per-call PostgreSQL instance started with testcontainers-go — for RLS, real constraint enforcement, and other dialect-specific behavior `NewSQLite` cannot exercise. Calls `t.Skip` when no Docker (or Docker-API-compatible) daemon is reachable, so a caller needs no Docker-availability check of its own. Container and connection torn down via `t.Cleanup` |

Both are thin wrappers around `Open`, so a caller gets dbkit's full mandatory wiring (fixed pool limits, the tenant-scoping plugin) on top of the connection, not a bare one. Neither applies any migration: `dbtest` is imported by modules dbkit has never heard of, each with its own model(s), so it cannot bake in a schema — not even dbkit's own `internal/testutil.Widget`. A caller migrates the returned `*gorm.DB` itself afterward, typically with its own `MigrationRegistry` and `pkgcore.Module` (the same shape the "Typical integration" section and `example_test.go` show), or a plain `db.Exec` of a fixture's migration SQL for a lightweight test.

`NewPostgres`'s Docker-availability check (`dockerAvailable` / `dockerHostAddress` in `dbtest/docker_probe.go`) is a small, self-contained probe, not a call into testcontainers-go's own provider/host-resolution machinery — that machinery caches its resolved host process-wide behind a `sync.Once`, which would make it unsafe to unit-test with a deliberately-wrong host (`dbtest/docker_probe_test.go` does exactly that) in the same binary as a happy-path test that needs the real daemon.

## Dual-dialect constraints for models used with dbkit

Every model passed to `Repository[T]`, embedded under `TenantModel`, or otherwise written through a `*gorm.DB` obtained from `Open` is expected to run correctly against both PostgreSQL and SQLite, per the backend coding standard's dual-dialect rules (`.claude/skills/backend-coding-standards/SKILL.md` §5):

- Generate primary keys (ULID/UUID) in the application. Never `gen_random_uuid()` — SQLite has no equivalent.
- Store JSON with `datatypes.JSON`. Never filter on JSONB operators (`->`, `->>`, `@>`, …) — SQLite cannot execute them.
- No native PostgreSQL array columns. Use `datatypes.JSON` or a join table instead.
- Timestamps use the `autoCreateTime` / `autoUpdateTime` gorm tags. Never write `NOW()` into a query, a default, or a migration.
- Full-text search goes through a `SearchProvider` interface at the business-module layer (tsvector on PostgreSQL, `LIKE` fallback on SQLite) — dbkit itself provides no full-text search primitive.
- `tenant_id` is the leftmost column of every composite index, and a tenant-scoped table's primary key should be `(tenant_id, id)`. `TenantModel`'s own tag does not declare `primaryKey` and cannot safely be made to (see its doc comment) — when the composite key matters to you, skip embedding `TenantModel` and declare `TenantID` directly instead, with the tag you need (as `internal/testutil.Widget` does).
- dbkit adds one convention of its own on top of the standard's list: `T` must have exported `ID` and `TenantID` string fields by those exact Go names (declared directly, or promoted from an embedded `TenantModel`), because `Repository[T].Create`/`Update` read and write them through reflection — `TenantScoped` cannot express this in the type system, since it requires only the `GetTenantID()` accessor.
- Sensitive fields (national ID numbers, phone numbers, health-related free text, …) use `` gorm:"serializer:<name>" `` after `RegisterEncryptedSerializer(name, cipher)`, plus a separate, non-encrypted, indexed blind-index column (`` `gorm:"column:<field>_index;size:64;index"` ``, declared next to the encrypted field — never a serializer itself) whenever equality lookup is needed. Both sides of the lookup go through one `BlindIndexer` bound to that column — `Index(plaintext)` on write, `Equal(raw input)` on query — so the column's canonical form (E.164 for phone numbers, lowercase for emails) is applied identically every time; see the "Blind indexes" section above.

## Typical integration

Defining a tenant-scoped model and the repository a business module embeds it in:

```go
package billing

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// Subscription is billing's tenant-scoped model. ID and TenantID are exactly
// the field names dbkit.Repository[T] requires (see repository.go), and
// tenant_id is the leftmost column of the composite primary key per the
// backend coding standard's data-model rules.
type Subscription struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null;index:idx_sub_tenant_created,priority:1"`
	PlanID   string `gorm:"size:64;not null"`
	Status   string `gorm:"size:32;not null"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s Subscription) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// Repository is billing's data-access type. It embeds dbkit.Repository[T]
// instead of holding a *gorm.DB (backend coding standard, section 3.2).
type Repository struct {
	*dbkit.Repository[Subscription]
}

func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[Subscription](db)}
}

// Activate creates an active subscription under ctx's tenant.
func (r *Repository) Activate(ctx context.Context, id, planID string) (*Subscription, error) {
	sub := &Subscription{ID: id, PlanID: planID, Status: "active"}
	if err := r.Create(ctx, sub); err != nil {
		return nil, err
	}
	return sub, nil
}
```

Using it — a full create / read / update / delete sweep:

```go
db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
if err != nil {
	return err
}

repo := billing.NewRepository(db)

// A real request's ctx already carries the tenant, injected by
// tenancy.Middleware from the access token claims. Building it explicitly
// here is only for illustration.
ctx = pkgcore.WithTenant(ctx, "tenant-acme")

sub, err := repo.Activate(ctx, newULID(), "plan_pro") // however this module generates its ULIDs
if err != nil {
	return err
}

got, err := repo.FindByID(ctx, sub.ID) // promoted from the embedded *dbkit.Repository[Subscription]
if err != nil {
	return err
}

got.Status = "past_due"
if err := repo.Update(ctx, got); err != nil {
	return err
}

if err := repo.Delete(ctx, got.ID); err != nil {
	return err
}
```

This exact pattern — a tenant-scoped model, a repository embedding `dbkit.Repository[T]`, `dbkit.Open`, `Create` → `FindByID` — is compiled and run under CI as `Example()` in [`example_test.go`](example_test.go) (package `dbkit_test`, matching `pkgcore`'s own `example_test.go` convention), with an `// Output:` comment asserted against the real printed output, per the Documentation rule below. `repository_test.go`'s own `TestRepository_FullCRUDLifecycle_SingleTenant` covers the same call sequence in more depth — it also exercises `Update` and `Delete` — against `internal/testutil.Widget`.

## Known limitations

**`Repository[T]`'s generic constraint structurally excludes identity and platform data — this is intentional, not an oversight, but it is easy to miss.** Per `docs/internal/04-data-and-tenancy.md`'s data-domain table, only two of the four domains (tenant data, link data) are *supposed* to implement `TenantScoped`; identity data (`users`, `sessions`) and platform data (platform-level plan definitions, global config) must **not** — that is exactly what `tenancytest.AssertNotTenantScoped` exists to assert (see below). Because `Repository[T TenantScoped]` requires the constraint, a model in either of those two domains cannot be used as `T` at all — the compiler rejects it, not a runtime check. This is correct, not a bug: `Repository[T]`'s entire value proposition (the fail-closed tenant check, the forced tenant_id on Create) is specific to tenant-owned data and doesn't make sense for a table with no tenant.

A module modeling identity or platform data uses the plain `*gorm.DB` returned by `Open()` directly, with no `Repository[T]`-style wrapper — and that is safe, not a hole: the isolation plugin already ignores any model that doesn't implement `TenantScoped` (`TestTenantScopePlugin_NonTenantScopedModel_Unaffected` proves this), so there is nothing for such a module to opt out of. Do not build an ad-hoc "safety wrapper" for this case without first checking whether `tenancytest.AssertNotTenantScoped` (`go/tenancy/tenancytest`) already covers what you're trying to guard against — inventing a second mechanism for the same guarantee is how these things drift apart. If a real need for a shared, non-tenant-scoped repository *pattern* (as opposed to just "use `*gorm.DB` directly") emerges once `authn`/`org` are actually built, design it then, informed by their real query shapes — a speculative generic type built now, with no consumer, is likely to guess the shape wrong.

**`tenancytest.AssertIsolated` / `AssertNotTenantScoped`, which multiple documents (`CLAUDE.md`, this repo's coding standards) describe as mandatory for every new Repository, now exist at `go/tenancy/tenancytest`**, built entirely on dbkit's already-exported material (`Open`, `Repository[T]`, `TenantScoped`) exactly as anticipated below — see `go/tenancy/AGENTS.md` for their full behavior. Every module above `tenancy` in the dependency graph must run one of the two against its own `Repository[T]` usage. dbkit itself is the one exception: sitting below `tenancy`, it cannot import `tenancytest` without the same import cycle described under "One dependency, and why there is only one" above, so dbkit's own isolation guarantee continues to rest on its hand-written suite (`tenant_scope_test.go`, `repository_test.go`) rather than `tenancytest`.

**`Repository[T]`'s query surface is minimal on purpose.** `List` takes no filter or pagination parameters — it returns every row for the caller's tenant, full stop. There is no `FindWhere`, `Count`, ordering, or paging. Per `repository.go`'s own doc comment, this is "deliberately minimal; later modules extend it as their query needs grow," not an oversight. Until `Repository[T]` itself grows a query surface, a module with real filtering/pagination needs today has two options, in order of preference:
1. Build the query on the same `*gorm.DB` layer 1 already protects, still against a `TenantScoped` model, so the plugin's `WHERE tenant_id = ?` still applies as a backstop even though `Repository[T]`'s own re-verification does not run for that call.
2. Use the raw-SQL escape hatch below, when even that is not enough — for example, a query shape the plugin cannot safely rewrite (see the `Or(...)` composition note in Rules).

**The raw-SQL escape hatch, and how to use `WithTenantSession` from it safely.** Per the backend coding standard (§3.2), a reporting or bulk-maintenance query that genuinely cannot go through `Repository[T]` belongs under the business module's own `internal/query/`, must pass the tenant explicitly, and must ship its own isolation test. Two things matter when you reach for it:
- **You are opting out of layers 1 and 2 for that one statement.** `db.Raw` / `db.Exec` / `db.Row` / `db.Rows` never run the plugin's callbacks at all — they bypass GORM's query/create/update/delete processors entirely — and hand-written SQL obviously never runs `Repository[T]`'s own checks either. Every tenant filter in that statement is now hand-written and your responsibility, exactly what every other rule in this document exists to make unnecessary elsewhere. Treat it as a reviewed exception, not a habit, and bind the tenant as a parameter, never build the `WHERE tenant_id = ?` clause with `fmt.Sprintf` or string concatenation.
- **Layer 3 is the intended backstop for exactly this case.** The whole point of isolation layer 3 (PostgreSQL RLS) is that it protects a statement even when layers 1 and 2 were both skipped — which raw SQL always does. The safe pattern is to run the raw statement through `fn` in `WithTenantSession(ctx, db, fn)`, using the `tx` it hands `fn` for the statement itself, so a mistake in the hand-written `WHERE` clause is still caught by the database's RLS policy instead of becoming a silent cross-tenant leak. See `tenant_session.go`'s doc comment and the "RLS session state" entry above for the full contract.

```go
// internal/query/overdue.go — tenant_id is still bound explicitly by hand
// (there is no Repository[T] method for this shape of aggregate query);
// wrapping the call in dbkit.WithTenantSession adds layer 3 as a backstop
// underneath it, so a mistake in this hand-written WHERE clause is still
// caught by the database's RLS policy in production. WithTenantSession
// works the same way on a *gorm.DB not obtained from Open, since it reads
// the dialect from db.Name() rather than any dbkit-specific field.
func OverdueSubscriptions(ctx context.Context, db *gorm.DB, tenant pkgcore.TenantID) ([]Row, error) {
	var rows []Row
	err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Raw(
			`SELECT id, plan_id FROM subscriptions WHERE tenant_id = ? AND status = 'overdue'`,
			string(tenant),
		).Scan(&rows).Error
	})
	return rows, err
}
```

**`pkgcore.WithSystemContext` does not interact with tenant-scoped models at all.** Neither the plugin nor `Repository[T]` special-cases a system context. `WithSystemContext` and `WithTenant` are orthogonal (per `pkgcore/tenant.go`'s own doc comment: "a system context is orthogonal to a tenant context: it sets no tenant"), so a query against a `TenantScoped` model carrying only a `SystemReason` and no tenant fails closed exactly the same way a request with neither would. A system-context caller that needs a specific tenant's data still supplies it with `pkgcore.WithTenant`; a genuinely cross-tenant admin read (e.g. "search every tenant for X") has no path through dbkit today — it would need either per-tenant iteration through `Repository[T]` or a raw, admin-role-driven query outside it.

**The blind index column is populated by the writer, explicitly — nothing in dbkit fills it for you.** GORM serializers are per-field and cannot write a second column, and the index cannot be derived from the encrypted field's tag, so a model's blind-index column starts NULL/empty on every insert unless the creating code computes `Index(plaintext)` and stores the result itself (see the "Blind indexes" section's example). Missing that step is invisible in the row's own ciphertext and surfaces only as an equality lookup that returns nothing. A full-population or backfill path is deliberately not built into dbkit — it belongs to each module's create/update code paths (where the plaintext already exists) — and bulk recomputation after a blind-index key rotation is a separate `jobs` batch task by design (see below), not something this package triggers.

**A blind-index key rotation has no retired-key fallback, and dbkit does not run the recompute for you.** `Cipher` keeps retired keys so old ciphertext keeps decrypting across a rotation; a blind-index column cannot — a single equality comparison matches exactly one index value per row, so once the column holds values under the old key, nothing matches under the new one. Rotating therefore means recomputing every row's index as a jobs batch task (per `docs/internal/10-compliance-and-audit.md`, rotation of a blind-index secret is planned as exactly that). The operator-facing ordering — whether the deployment tolerates a transient mismatch window between "column rewritten under the new key" and "lookups switched to the new key", or carries a second column through the migration — is that jobs task's design decision, not dbkit's, and no dual-key machinery exists here to make either ordering seamless.

**The mechanism is equality-only, and per-column normalization is a caller contract.** A deterministic HMAC index supports exact matches and nothing else: there is no prefix, partial, or fuzzy search over a blind-index column, by design (building one would leak exactly the structure the encryption hides), and no lower-precision variant of the built-in normalizers. The two canonical forms dbkit ships are the ones the design docs promise — E.164 phone numbers, lowercased emails; a column whose data needs any other canonical form supplies its own `NormalizeFunc` and documents the form next to the model declaration, because the index is only as consistent as the normalization both sides of every lookup share.

## Rules

**Dependencies**
- Do not add a second speed-module dependency to this package. dbkit sits directly above `pkgcore`; `tenancy` sits above dbkit and will depend on it, so an import running the other way is a cycle, and `observability` is unreachable for the identical bottom-up reason (see "One dependency, and why there is only one" above).
- Do not add a third-party dependency casually. It lands in the `go.sum` of every module built on speed.

**Connections**
- Do not call `gorm.Open`, `postgres.Open`, or `sqlite.Open` directly anywhere in this codebase. `Open` is the only sanctioned entry point; no code path in dbkit ever returns a `*gorm.DB` before the tenant-scoping plugin is installed on it.
- Do not add fields to `Options` to override the connection-pool defaults. That is deliberate — every `*gorm.DB` in this codebase shares the same pool shape, so `open.go`'s constants are the one place to reconsider them.
- Do not log `Options.DSN` or let it reach a returned error. `Open` never includes it in one, by design, because it often carries credentials.

**Tenancy**
- Do not hold a `*gorm.DB` in a business module and write queries against it. Embed `dbkit.Repository[T]`.
- Do not expect the tenant-scoping plugin to intercept `db.Raw`, `db.Exec`, `db.Row`, or `db.Rows`. It hooks GORM's query/create/update/delete callbacks only; raw SQL bypasses layers 1 and 2 completely (see Known limitations).
- Do not assume the plugin fails to safely tenant-scope a caller-composed `.Or(...)` condition — it groups the caller's existing WHERE into one `AndConditions` block before appending its own filter, so `db.Where("name = ?", x).Or("name = ?", y)` still ends up as `(name = ? OR name = ?) AND tenant_id = ?` (see `groupExistingWhereConditions` in `tenant_scope.go`). But do treat this as evidence that hand-building multi-condition queries directly against a `*gorm.DB` from `Open` carries more responsibility than it looks; prefer `Repository[T]` wherever its surface suffices.
- Do not assume `pkgcore.WithSystemContext` grants a cross-tenant read against a `TenantScoped` model. It does not — see Known limitations.
- Do not construct or install the tenant-scoping plugin yourself; `tenantScopePlugin` is unexported on purpose, so no `*gorm.DB` in this codebase can exist "unprotected" by accident.
- Do not assume `TenantModel` alone gives you the standard's recommended `(tenant_id, id)` composite primary key — its tag does not include `primaryKey`. Do not "fix" that by embedding `TenantModel` and then redeclaring a same-named `TenantID` field on the embedding struct to shadow it: GORM ends up writing and scanning the shadowing field, but the promoted `GetTenantID` method still reads `TenantModel`'s own, never-populated copy — silently breaking `Repository[T].FindByID` for every tenant, including the row's real owner. Declare `TenantID` directly, with no `TenantModel` embedding at all, when the composite key matters (see `tenant_scope.go`'s `TenantModel` doc comment and `tenant_scope_tenantmodel_test.go`).

**`WithTenantSession`**
- Do not write a literal `SET LOCAL app.current_tenant = ?` (or `SET ... = $1`) against PostgreSQL, in `tenant_session.go` or anywhere else. PostgreSQL's `SET`/`SET LOCAL` grammar does not accept a bound query parameter for the value — it is rejected outright with a syntax error (`SQLSTATE 42601`), independent of driver or ORM, confirmed against a live server. Use `SELECT set_config('app.current_tenant', ?, true)` instead — an ordinary function call, so its value argument is a normal bound parameter — which is exactly what `WithTenantSession` does.
- Do not set `app.current_tenant` with plain `SET` (not `SET LOCAL`, and not `set_config`'s `is_local` left `false`), and do not set it outside an explicit transaction that also contains the query or write it is meant to protect. Either mistake lets the setting persist on a pooled connection past the point it was meant to apply, so it can leak into a later, unrelated request for a different tenant that happens to reuse the same physical connection — the exact bug class this whole mechanism exists to prevent.
- Do not issue a statement meant to be RLS-protected against anything other than the `tx` `WithTenantSession` hands `fn`. A statement issued against the outer `db`, a different session, or a connection obtained some other way inside `fn` never sees the GUC `WithTenantSession` just set — it runs on a different transaction (or no transaction at all) entirely.
- Do not assume a failed GUC-setting step is safe to ignore and fall through to `fn` anyway. `WithTenantSession` aborts the whole transaction and returns the error without calling `fn` when it fails, on purpose: proceeding would silently mean RLS is not engaged for that operation.

**`Repository[T]`**
- Do not build a caller-facing message that distinguishes "no such id" from "that id belongs to another tenant." `FindByID` / `Update` / `Delete` all collapse both to `ErrRecordNotFound` on purpose; surfacing the difference is itself a cross-tenant information leak.
- Do not match `ErrRecordNotFound`, `ErrMissingID`, `ErrMissingTenantContext`, or `ErrTenantIDImmutable` with `errors.Is`/`==` against the package-level var once `WithParam`/`WithCause` may have run on it. Every `apperr` builder derives a *new* `*apperr.Error` instead of mutating the receiver, so identity does not survive decoration. Use `apperr.As(err)` and compare `.Code`, the way `repository_test.go`'s and the integration tests' own `isRecordNotFound` helpers do.
- `pkgcore.ErrNoTenant` is the one exception in this list — match it with `errors.Is` directly. `Repository[T]` returns it unmodified, and the plugin's `ErrMissingTenantContext` wraps it as `Unwrap`'s cause, so `errors.Is(err, pkgcore.ErrNoTenant)` sees through both shapes uniformly.
- Do not assume `T`'s "ID"/"TenantID" field convention is compile-time checked. A `T` missing either fails the first `Create`/`Update`/`FindByID` call with a descriptive error, not at `go build` time.
- Do not assume `Update`'s `RowsAffected == 0` always means "no such row for this tenant" at the gorm level — it is this package's job to resolve that, not callers'. For a `T` whose only columns are `id` and `tenant_id` (a pure marker/link record with no other field), gorm's own `Update` callback computes an empty SET clause (it excludes every primary-key column, and here every column is one) and returns without issuing any SQL at all, leaving `RowsAffected` at zero whether or not the row exists. `Update` detects this exact case (`gorm did not build a statement at all`, not just `RowsAffected == 0`) and falls back to an explicit, still id-and-tenant-scoped existence check in the same transaction before deciding between success and `ErrRecordNotFound` — so a caller of `Update` never needs to special-case this shape of `T` itself, and a successful call against such a `T` means exactly "this row exists under your tenant", not "some field actually changed", since there is no field left to change. See `Update`'s own doc comment in `repository.go` and `TestRepository_Update_IDAndTenantIDOnlyModel_SucceedsAsNoOp` / `_DifferentTenant_ReturnsNotFound` / `_NoSuchID_ReturnsNotFound` in `repository_test.go`.

**Migrations**
- Do not use `AutoMigrate` anywhere in this codebase — it is neither auditable nor reversible. GORM structs are the schema source of truth Atlas generates versioned SQL from instead.
- Do not put a module's `//go:embed postgres sqlite` directive anywhere but the one file living in the directory where `postgres/` and `sqlite/` are its immediate children — `//go:embed` patterns resolve relative to that file's own directory.
- Do not assume `Register` validates `DependsOn`. A missing or cyclic dependency is reported only by `Apply`, once the full registered module set is known.

**Encryption and blind indexes**
- Do not reuse an encryption key passed to `NewCipher` as a blind-index key, or vice versa. Mixing an AES-GCM key and an HMAC blind-index key is a real cryptographic weakness, not a style nitpick; keep them as separate entries in the secret manager and inject them separately.
- Do not call `RegisterEncryptedSerializer` from a request path. GORM's serializer registry is process-global; register once per name at bootstrap, alongside other `Module.Register`-time wiring and the matching `NewBlindIndexer` calls.
- Do not skip the column's normalizer on either side of a lookup. Route every write through `(*BlindIndexer).Index` and every query through `(*BlindIndexer).Equal` on the *same* indexer, so both sides apply the column's canonical form automatically; a value stored under one normalization can never be looked up under another. Code calling raw `BlindIndex` directly is responsible for normalizing to canonical form itself, identically at write and query time — `BlindIndex` performs no normalization of its own, deliberately.
- Do not declare a blind-index column as a serializer field, and do not expect the index to be derived from the encrypted field's tag (GORM serializers are per-field and cannot write a second column). Declare it as a plain, indexed column — `` `gorm:"column:<field>_index;size:64;index"` `` — next to the encrypted field, and populate it explicitly on every create/update of that field via `Index(plaintext)`, computed over the same value being encrypted.
- Do not filter on a precomputed index value, and never on one computed under different normalization. `Equal` exists precisely so the query path cannot accept a hand-computed value; hand-building the condition defeats the mechanism and silently returns zero rows for values that are present.
- Do not index an empty or unnormalizable value. `Index` and `Equal` return an error for input their normalizer cannot canonicalize — propagate it, and leave the column NULL for a genuinely absent value; never store `""` or a best-effort guess.
- Do not expect `Decrypt` or an index mismatch to tell you *why* it failed. `ErrDecryptionFailed` covers both a wrong/missing key and a tampered ciphertext by design; a blind-index key rotation has no retired-key fallback and needs every row's index recomputed as a `jobs` batch task (see Known limitations).
- Do not use a blind-index column for anything but exact matches. The mechanism is equality-only by construction; prefix, partial, or fuzzy search over the column would leak the structure the encryption exists to hide.

**Documentation**
- Do not add an exported identifier to this package without a doc comment and an entry in the tables above, in the same pull request (backend coding standard, Documentation section) — and, for a new public entry point into the package (not every incidental exported type or constant), a compiling `Example` alongside the existing ones in `example_test.go`.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrMissingTenantContext` (`dbkit.tenant_context_required`) | The tenant-scoping plugin (layer 1) running a query/create/update/delete for a `TenantScoped` model on a context with no tenant | Rebuild the context with `pkgcore.WithTenant` (a worker that never rebuilt it is the usual culprit). Also matches `errors.Is(err, pkgcore.ErrNoTenant)`, since it wraps that as its cause |
| `ErrTenantIDImmutable` (`dbkit.tenant_id_immutable`) | An `Update`/`Updates` payload against a `TenantScoped` model trying to set `tenant_id` to a different tenant than the context's | Reject the request; a deliberate cross-tenant transfer needs its own explicit, audited operation |
| `ErrRecordNotFound` (`dbkit.record_not_found`) | `Repository[T].FindByID` / `Update` / `Delete` when nothing matches both the id and the caller's tenant | Ordinary not-found handling. Deliberately indistinguishable from "that id belongs to another tenant" |
| `ErrMissingID` (`dbkit.missing_id`) | `Repository[T].Update` given a model with an empty `ID` field | Caller error — fix the caller; there is no row this call could ever have meant |
| `dbkit.invalid_dialect` (no exported Go sentinel — match on `.Code`) | `Open` given an `Options.Dialect` that is neither `DialectPostgres` nor `DialectSQLite` | Startup misconfiguration; fix the configured dialect |
| `dbkit.connect_failed` (no exported Go sentinel — match on `.Code`) | `Open`'s driver setup, connection-pool retrieval, or `ctx`-bound ping failing | Infrastructure/connectivity problem; the DSN is never included, so check it out of band |
| `dbkit.tenant_scope_plugin_failed` (no exported Go sentinel — match on `.Code`) | `Open` failing to install the tenant-scoping plugin on an otherwise-open connection | Should not happen outside a GORM-version mismatch; treat as a startup abort |
| `ErrNilModule` / `ErrEmptyModuleName` / `ErrDuplicateModule` | `MigrationRegistry.Register` given a nil module, an empty `Name()`, or a `Name()` already registered | Fix the registration call; plain `errors.New` sentinels, safe to match with `errors.Is` directly |
| `ErrDependencyCycle` / `ErrMissingDependency` | `MigrationRegistry.Apply`: the registered modules' `DependsOn` graph has a cycle, or names a module never registered | Fix the module's `DependsOn`, or the bootstrap module set; the error names every module involved |
| `ErrUnknownDialect` | `MigrationRegistry.Apply` given a `Dialect` that is neither `DialectPostgres` nor `DialectSQLite` | Same fix as `dbkit.invalid_dialect` above |
| `ErrInvalidKeySize` | `NewCipher` or `NewBlindIndexer` given a key that is not exactly 32 bytes | Configuration error — AES-256-GCM encryption keys and blind-index HMAC keys are both 256-bit secrets; fix the key material |
| `ErrDecryptionFailed` | `Cipher.Decrypt` when no key, active or retired, authenticates the ciphertext | Wrong/missing key or tampered data, indistinguishable by design; never guess which in a caller-facing message |
| `pkgcore.ErrNoTenant` | `Repository[T]`'s methods and `WithTenantSession`, called on a context with no tenant (returned unmodified in both — see `repository.go`'s and `tenant_session.go`'s doc comments) | Fail closed. In a worker, rebuild the context with `pkgcore.WithTenant`; see `pkgcore/AGENTS.md`'s own error index |

Design rationale for the isolation model — internally titled "multi-tenant isolation: triple protection" — lives in `docs/internal/04-data-and-tenancy.md`; the module dependency graph is in `docs/internal/01-architecture.md`. Field-level encryption and blind-index rationale — the canonical forms, the equality-only promise, and blind-index secret rotation as a planned jobs task — lives in `docs/internal/10-compliance-and-audit.md`.
