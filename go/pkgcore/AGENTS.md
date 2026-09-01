# pkgcore

The dependency floor of the monorepo. Every other Go module imports it; it imports no other speed module.

It owns four things and nothing else:

| Concern | Where |
|---|---|
| The `Module` / `Registry` / `Kernel` wiring contract | `registry.go` |
| Tenant context and the audited escape hatch from tenant filtering | `tenant.go` |
| Dual-profile infrastructure interfaces (`KVStore`, `EventBus`) plus their in-memory implementations | `kv.go`, `eventbus.go` |
| The runtime `Profile` enumeration | `profile.go` |

Two subpackages: `apperr` (the structured application error every module returns) and `config` (the bootstrap configuration loader, run once at process startup).

**Out of scope.** Database access (`dbkit`), tenant enforcement in SQL (`tenancy`), logging and tracing (`observability`), runtime and tenant-overridable configuration (the `config` *module*, not this `config` package), job execution (`jobs`). pkgcore declares contracts; it does not implement business behaviour.

## Public API

### `pkgcore`

**Wiring**

| Signature | Purpose |
|---|---|
| `type Module interface { Name() string; DependsOn() []string; Migrations() embed.FS; Locales() embed.FS; OpenAPISpec() []byte; Register(*Registry) error }` | The contract every module implements |
| `type Registry struct { Routes; Config; Features; Permissions; Jobs; Notifications; Events; AuditActions }` | Everything a module can contribute, one field per mechanism |
| `func NewRegistry(bus EventBus) *Registry` | A registry wired to the in-memory registrars and to `bus`. A nil bus panics |
| `func (*Registry) EventBus() EventBus` | The bus behind `Registry.Events`, so the host publishes into what modules subscribed to |
| `func NewKernel(profile Profile, opts ...KernelOption) *Kernel` | A kernel that assembles modules for one profile |
| `func WithEventBus(bus EventBus) KernelOption` | Inject the host's `EventBus`. Required for `ProfileProduction`, which has no built-in one |
| `func (*Kernel) Profile() Profile` | The profile the kernel assembles for |
| `func (*Kernel) Bootstrap(ctx context.Context, modules ...Module) (*Registry, error)` | Dependency-sort, register each module, validate the feature graph |
| `func ValidateFeatureGraph(reg *Registry) error` | Reports feature flags depending on flags nobody registered |

Registrar interfaces: `RouteRegistrar`, `ConfigSchemaRegistrar`, `FeatureRegistrar`, `PermissionRegistrar`, `JobHandlerRegistrar`, `NotificationRegistrar`, `EventRegistrar` (`Publishes` / `Published` / `Subscribe` / `Bus`), `AuditActionRegistrar`.

Declaration types:

| Type | Fields |
|---|---|
| `MountedRoute` | `Path`, `Handler` |
| `ConfigItem` | `Key`, `Type`, `Default`, `Sensitive`, `Description` |
| `FeatureFlag` | `Key`, `Default`, `Description`, `DependsOn` |
| `NotificationType` | `Key`, `Group`, `DefaultChannels`, `Unsubscribable` |
| `EventDecl` | `Type`, `PayloadType`, `Description` |


**Profile**

`type Profile string`, constants `ProfileDemo` / `ProfileProduction`, `func ParseProfile(string) (Profile, error)` (trims and lowercases), `func (Profile) Valid() bool` (exact match, so `"Demo"` is not valid).

**Tenant context**

| Signature | Purpose |
|---|---|
| `func WithTenant(ctx, TenantID) context.Context` | Scope a context to a tenant |
| `func TenantFromContext(ctx) (TenantID, bool)` | Read it; `false` when absent or empty |
| `func MustTenantFromContext(ctx) (TenantID, error)` | Fail-closed read for data access. Never panics despite the name |
| `func RegisterSystemPurpose(SystemPurpose)` | Declare a purpose the escape hatch will accept |
| `func WithSystemContext(ctx, SystemReason) (context.Context, error)` | Grant the escape hatch; `SystemReason{Actor, Purpose, Ticket}` |
| `func SystemReasonFromContext(ctx) (SystemReason, bool)` | Read who bypassed filtering and why |

**Infrastructure interfaces**

| Signature | Purpose |
|---|---|
| `type KVStore interface { Get; Set; Delete; IncrByFloat; CompareAndSwap }` | Key-value contract, designed against the weakest backend |
| `func NewMemoryKVStore() KVStore` | Demo-profile implementation, and the test double |
| `type EventBus interface { Publish(ctx, Event) error; Subscribe(string, EventHandler) }` | Domain event exchange between modules |
| `func NewMemoryEventBus() EventBus` | Demo-profile implementation: synchronous, in registration order. Single-process, so it is not a production bus |
| `type Event struct { Type string; TenantID TenantID; Payload any }` | One domain fact |

### `pkgcore/apperr`

`type Error struct { Code string; Params map[string]any; Status int }` with `Error()`, `Unwrap()`, `WithParam(key, value) *Error`, `WithCause(error) *Error`.

Constructors and the status each suggests: `Invalid` 400, `Unauthorized` 401, `Forbidden` 403, `NotFound` 404, `Conflict` 409, `Internal` 500. `func As(error) (*Error, bool)` recovers one from a wrapped chain.

`WithParam` and `WithCause` return a *derived* `*Error` and leave the receiver untouched, so a package-level error value can be shared and decorated per request.

### `pkgcore/config`

`func New(opts ...Option) *Loader` with `WithConfigFile(path)`, `WithArgs([]string)`, `WithEnviron([]string)`; `func (*Loader) Load(target any) error`.

Priority, highest first: command-line flags, environment variables, config file, the defaults already on the target struct. `Database.DSN` maps to key `database.dsn` (segments joined by `KeyDelimiter`), flag `--database.dsn`, variable `SPEED_DATABASE__DSN` (`EnvPrefix` + `EnvSeparator`). Per-field options come from the `TagName` struct tag: `config:"required"`, `config:"-"`.

## Typical integration

Implementing a module:

```go
func (m *Module) Register(reg *pkgcore.Registry) error {
	reg.Routes.Mount("/api/v1/billing", m.router())

	if err := reg.Config.Add(pkgcore.ConfigItem{
		Key: "billing.invoice_retry_limit", Type: "int", Default: 3,
		Description: "How many times a failed invoice charge is retried.",
	}); err != nil {
		return err
	}
	if err := reg.Permissions.Add("billing:read", "billing:write"); err != nil {
		return err
	}
	if err := reg.Events.Publishes(pkgcore.EventDecl{
		Type: "billing.invoice.paid", PayloadType: "billing.InvoicePaid",
		Description: "An invoice was paid in full.",
	}); err != nil {
		return err
	}
	reg.Events.Subscribe("authn.user_created", m.openCreditLedger)
	return nil
}
```

Booting a host:

```go
profile, err := pkgcore.ParseProfile(os.Getenv("SPEED_PROFILE"))

// ProfileDemo needs no options; ProfileProduction must be given a bus.
var opts []pkgcore.KernelOption
if profile == pkgcore.ProfileProduction {
	opts = append(opts, pkgcore.WithEventBus(broker.NewEventBus(cfg)))
}
reg, err := pkgcore.NewKernel(profile, opts...).Bootstrap(ctx, tenancy.New(), billing.New())
```

Returning an error:

```go
return apperr.NotFound("billing.subscription_not_found").WithParam("id", id)
```

Full runnable versions of all of the above live in `example_test.go`, `apperr/example_test.go` and `config/example_test.go`, and are executed by CI.

## Rules

**Dependencies**

- Do not import any other speed module here, `dbkit` included. pkgcore is the floor; an import from above is a cycle. That is why `Module.Migrations` returns a plain `embed.FS` rather than a `dbkit` type.
- Do not add a third-party dependency to the root package. It lands in every consumer's `go.sum`. The `config` subpackage carries koanf and is the only place a new one may even be argued for.

**Interfaces and profiles**

- Do not expose a capability on an infrastructure interface that only one implementation can satisfy. Design against the weaker side, which is the demo profile: no server-side scripting, no pub/sub, no pipelines on `KVStore`.
- Do not branch on `Profile` outside kernel wiring. Business logic must not contain `if profile == ProfileDemo`.
- Do not write a mock for `KVStore` or `EventBus`. `NewMemoryKVStore` and `NewMemoryEventBus` are the test doubles.
- Do not use `NewMemoryEventBus` as a production fallback. It is single-process, so every replica would get a private bus; `ProfileProduction` fails assembly instead, and the host injects a real bus with `WithEventBus`.
- Do not build a read-modify-write cycle out of `Get` + `Set`. Use `IncrByFloat` or `CompareAndSwap`; they are the only operations every backend can make atomic.
- Do not retain or hand out a caller's byte slice in a `KVStore` implementation, and do not perform an operation on a cancelled context — return the context error instead.

**Tenancy**

- Do not treat a missing tenant as "all tenants". `MustTenantFromContext` returns `ErrNoTenant` so callers fail closed; propagate that error rather than defaulting.
- Do not pass a tenant through a bare string context key. Use `WithTenant`.
- Do not invent a system purpose at the call site. Declare it with `RegisterSystemPurpose` from the module's own registration; an undeclared purpose is refused.
- Do not call `WithSystemContext` without an `Actor`, and record a `Ticket` whenever one exists. Every bypass must be attributable.
- Do not treat a system context as an authorization bypass. It suppresses tenant filtering only.

**Module contract**

- Do not add a method to `Module`. Under lockstep versioning that breaks all 20 modules at once — add a field to `Registry` instead.
- Do not perform I/O in `Register`. It declares; the kernel decides when anything runs.
- Do not depend on module registration order. Declare the dependency in `DependsOn`; `Bootstrap` sorts, and reports a cycle or a missing dependency rather than guessing.
- Do not swallow a registrar error. A duplicate key is a bug across modules, not a merge, and nothing is registered when the call returns an error.
- Do not publish a domain event the module never declared with `Events.Publishes`. The declarations are the catalog `integration` maps onto the versioned public event schema.
- Do not decorate a shared `apperr` value expecting the receiver to change. `WithParam` and `WithCause` derive a new error.

**Errors and configuration**

- Do not put human-readable text in an `apperr` code. The code is machine-readable and stable; the client resolves it through its own i18n catalog.
- Do not change or reuse a released `apperr` code. It is part of the public API contract.
- Do not surface a wrapped cause in an API response. `WithCause` is for logs and `errors.Is`.
- Do not resolve runtime or tenant-overridable settings through `config.Loader`. It handles bootstrap values only: how to reach infrastructure, and which profile is running.
- Do not hand-write the configuration reference. It is generated from the `ConfigItem` declarations.

**Documentation**

- Do not add an exported identifier without a doc comment, an `Example` in the matching `example_test.go`, and an entry in the tables above, in the same pull request.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrInvalidProfile` | `ParseProfile` on anything but demo/production | Abort startup; the profile is misconfigured |
| `ErrNoTenant` | `MustTenantFromContext` on an unscoped context | Fail closed. In a worker, rebuild the context with `WithTenant` |
| `ErrSystemActorRequired` | `WithSystemContext` with an empty `Actor` | Name the actor; the bypass is audited |
| `ErrSystemPurposeNotRegistered` | `WithSystemContext` with an undeclared purpose | Declare it with `RegisterSystemPurpose` |
| `ErrNotNumeric` | `IncrByFloat` on a key holding a non-number | The key is not a counter; the stored value is left untouched |
| `ErrDuplicateModuleName` | Two modules reporting the same `Name()` | Rename one; nothing was registered |
| `ErrDependencyCycle` | `DependsOn` forming a cycle | The error names the cycle; break it |
| `ErrMissingDependency` | Depending on a module absent from the bootstrap set | Add the module, or drop the dependency |
| `ErrDuplicateConfigKey` / `ErrDuplicateFeatureFlag` / `ErrDuplicatePermission` / `ErrDuplicateJobType` / `ErrDuplicateNotificationType` / `ErrDuplicateEventType` / `ErrDuplicateAuditAction` | The same key registered twice | Two modules own one key; decide which does |
| `ErrUnresolvedFeatureDependency` | A flag depending on a flag nobody registered | Register the flag, or drop the dependency |
| `ErrMissingProductionEventBus` | `Bootstrap` on `ProfileProduction` with no bus wired | Inject the host's bus with `WithEventBus` |
| `config.ErrMissingValue` | A `config:"required"` field left zero | The error names the key and every source consulted |
| `config.ErrInvalidValue` | A supplied value not applicable to its field | The error names the offending key and its source |
| `config.ErrSourceUnreadable` | An unparseable config file or malformed flag | Fix the source; startup aborts |
| `config.ErrInvalidTarget` | `Load` given a non-struct-pointer or a bad `config` tag | Programming error; fix the target type |

Design rationale lives in `docs/internal/01-architecture.md` (module graph and the `Registry` decision), `03-runtime-profiles.md` (dual profiles) and `04-data-and-tenancy.md` (tenant isolation).
