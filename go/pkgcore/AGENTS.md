# pkgcore

The dependency floor of the monorepo. Every other Go module imports it; it imports no other speed module.

It owns four things and nothing else:

| Concern | Where |
|---|---|
| The `Module` / `Registry` / `Kernel` wiring contract | `registry.go` |
| Tenant context and the audited escape hatch from tenant filtering | `tenant.go` |
| Dual-deployment-mode infrastructure interfaces (`KVStore`, `EventBus`, `Mailer`, `ObjectStore`) plus each one's standalone implementation and the Redis-, SMTP- or S3-backed distributed one | `kv.go`, `eventbus.go`, `mailer.go`, `objectstore.go`, `redis_kv.go`, `redis_eventbus.go`, `smtp_mailer.go`, `s3_objectstore.go` |
| The `DeploymentMode` enumeration | `deployment_mode.go` |

Two subpackages: `apperr` (the structured application error every module returns) and `config` (the bootstrap configuration loader, run once at process startup).

**Out of scope.** Database access (`dbkit`), tenant enforcement in SQL (`tenancy`), logging and tracing (`observability`), runtime and tenant-overridable configuration (the `config` *module*, not this `config` package), job execution (`jobs`). pkgcore declares contracts; it does not implement business behaviour.

## Public API

### `pkgcore`

**Wiring**

| Signature | Purpose |
|---|---|
| `type Module interface { Name() string; DependsOn() []string; Migrations() embed.FS; Locales() embed.FS; OpenAPISpec() []byte; Register(*Registry) error }` | The contract every module implements |
| `type Registry struct { Routes; Config; Features; Permissions; Jobs; Notifications; Events; AuditActions }` | Everything a module can contribute, one field per mechanism |
| `func NewRegistry(bus EventBus, kv KVStore, mailer Mailer) *Registry` | A registry wired to the in-memory registrars, to `bus`, `kv` and `mailer`. A nil argument panics |
| `func (*Registry) EventBus() EventBus` | The bus behind `Registry.Events`, so the host publishes into what modules subscribed to |
| `func (*Registry) KVStore() KVStore` | The key-value store the registry was built with |
| `func (*Registry) Mailer() Mailer` | The mailer the registry was built with |
| `func (*Registry) ObjectStore() ObjectStore` | The object store the registry was wired to. Not a `NewRegistry` parameter — the seam post-dates that frozen three-argument signature — so a hand-built registry has none until `Bootstrap` installs one, and only a bootstrapped `Registry` may be used for the store |
| `func NewKernel(mode DeploymentMode, opts ...KernelOption) *Kernel` | A kernel that assembles modules for one deployment mode |
| `func WithEventBus(bus EventBus) KernelOption` | Inject the host's `EventBus`. Required for `DeploymentModeDistributed`, which has no built-in one |
| `func WithKVStore(store KVStore) KernelOption` | Inject the host's `KVStore`. Required for `DeploymentModeDistributed`, which has no built-in one |
| `func WithMailer(mailer Mailer) KernelOption` | Inject the host's `Mailer`. Required for `DeploymentModeDistributed`, which has no built-in one |
| `func WithObjectStore(store ObjectStore) KernelOption` | Wire the host's `ObjectStore` in place of the deployment mode's default. Required for `DeploymentModeDistributed`, which has no built-in one; a standalone host that must keep objects across restarts injects its own local store too, because the standalone default is a throwaway directory. A nil store leaves the default in place |
| `func (*Kernel) DeploymentMode() DeploymentMode` | The deployment mode the kernel assembles for |
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


**Deployment mode**

`type DeploymentMode string`, constants `DeploymentModeStandalone` / `DeploymentModeDistributed`, `func ParseDeploymentMode(string) (DeploymentMode, error)` (trims and lowercases), `func (DeploymentMode) Valid() bool` (exact match, so `"Standalone"` is not valid).

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
| `func NewMemoryKVStore() KVStore` | Standalone-mode implementation, and the test double |
| `func NewRedisKVStore(client *redis.Client) KVStore` | Distributed-mode implementation on Redis, preserving the in-memory store's observable semantics: TTL expiry, a non-positive ttl clearing an existing expiry, `IncrByFloat` keeping a live key's expiry, `CompareAndSwap` never changing one. The host owns the client and closes it |
| `type EventBus interface { Publish(ctx, Event) error; Subscribe(string, EventHandler) }` | Domain event exchange between modules |
| `func NewMemoryEventBus() EventBus` | Standalone-mode implementation: synchronous, in registration order. Single-process, so it is not a distributed-mode bus |
| `func NewRedisEventBus(client *redis.Client) *RedisEventBus` | Distributed-mode implementation on Redis Streams — one stream per event type, one consumer group per bus instance. Local handlers run synchronously with the original payload, exactly as on the in-memory bus; handlers on other instances sharing the client run once, asynchronously, on the bus's reader goroutines, receiving the JSON shape of the payload (never the concrete type), and their failures are not observable to any publisher. There is no catch-up: a group starts at the live end of its stream, and a replica disconnected longer than the stream's trim window loses what scrolled out. A reader whose stream or group is removed out from under it (operator cleanup, FLUSHALL, a failover) recreates both at the live end and keeps reading instead of wedging. Close stops the readers and destroys this instance's consumer groups, deleting a stream once no group is left on it -- a graceful shutdown leaves nothing behind, while a crashed instance's groups linger until the XGROUP DESTROY / DEL recipe the Close docs spell out removes them. The delivery-semantics note on the type spells the full contract out |
| `type Event struct { Type string; TenantID TenantID; Payload any }` | One domain fact |
| `type Mail struct { From string; To []string; Subject, Text, HTML string }` | One rendered outbound message; Text and HTML are the two renderings of one body, at least one non-empty |
| `type Mailer interface { Send(ctx, Mail) error }` | Outbound-mail contract, designed against the weakest backend: Send takes one already-rendered Mail and reports success or failure. Rendering, consent checks and retry policy are the caller's business |
| `func NewConsoleMailer() Mailer` | Standalone-mode implementation: prints every message to stdout as one self-delimiting, greppable record. Doubles as the test double |
| `func NewSMTPMailer(cfg SMTPConfig) Mailer` | Distributed-mode implementation speaking SMTP directly over `net/smtp` from the standard library. Lazy: the relay is dialed on the first Send. An unusable cfg (empty Host, Port outside 1..65535, unknown TLSMode) panics at construction. `SMTPConfig{Host, Port, Username, Password, TLSMode, InsecureSkipVerify}`; `SMTPTLSModeAuto` (port 465 = TLS from the first byte, otherwise STARTTLS when advertised) / `SMTPTLSModeStartTLS` / `SMTPTLSModeImplicitTLS`. An auth-enabled mailer never sends credentials over a connection that did not become TLS: SMTP AUTH fails the Send outright on such a relay -- net/smtp's PLAIN excepting a loopback relay host, the MailHog-style local case -- rather than falling back to anonymous mail. Send honours its context throughout; a cancelled or timed-out Send interrupts a relay that stopped answering |
| `type ObjectStore interface { PutObject(ctx, key, r io.Reader) error; GetObject(ctx, key) (io.ReadCloser, error); DeleteObject(ctx, key) error }` | Object-storage contract, designed against the weakest backend: a local directory. Objects are streams of bytes with no metadata, listing or presigned access; keys are a single tree under a shared grammar (a key must not be a proper prefix of another stored key), every operation streams and honours ctx, and the caller owns and closes a GetObject reader. Each deployment-mode role and the grammar's rationale are documented on the interface |
| `func NewLocalObjectStore(dir string) ObjectStore` | Standalone-mode implementation: one file per object below `dir`, created if missing and resolved to its canonical path, objects surviving across restarts. Writes are atomic (temporary sibling + rename); symbolic links anywhere on a key's path are never followed; an unusable directory panics at construction. Doubles as the test double |
| `func NewS3ObjectStore(cfg S3Config) ObjectStore` | Distributed-mode implementation on any S3-compatible service (MinIO, Aliyun OSS, AWS S3), the wrapper speaking their common dialect through a client it builds itself -- the host never imports minio-go, and no minio type crosses the seam. Lazy: the service is contacted on the first operation; an unusable cfg (empty Endpoint, Bucket, AccessKey or SecretKey) panics at construction. `S3Config{Endpoint, Bucket, AccessKey, SecretKey, Region, UseSSL}`: Bucket must already exist (provisioning is a hosting operation), Region is the signing region AWS and OSS require while MinIO ignores it, and the service's NoSuchKey surfaces as `ErrObjectNotFound` |

**M2 boundary.** The seam deliberately ends at raw bytes. The media service designed to sit on it -- presigned direct upload, the `Object` domain record with its `Derivatives` and `ExpiresAt`, EXIF stripping, server-side MIME checks, derivative generation through the jobs queue, and tenant-level lifecycle and retention, the whole storage-module feature list of `docs/internal/07-platform-services.md` -- is the M2 `storage` module's deliverable, built against this contract when that milestone lands and never added to the contract itself: presigned access is a capability only the S3-backed store could satisfy, and an interface designed against the weaker side, the local directory, must not grow one. The `storage` module is also the seam's first consumer. The reference app has no file-upload flow yet, so it correctly uses nothing here -- a store no module reads or writes would be a fake use, worse than no use.

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
mode, err := pkgcore.ParseDeploymentMode(os.Getenv("SPEED_DEPLOYMENT_MODE"))

// DeploymentModeStandalone needs no options; DeploymentModeDistributed must
// be given a bus, a key-value store, a mailer and an object store. A real
// host wires the broker/Redis/SMTP/S3-backed implementations here; the
// in-memory, console and local-directory constructors of the API tables are
// the standalone-mode counterparts and the test doubles.
var opts []pkgcore.KernelOption
if mode == pkgcore.DeploymentModeDistributed {
	opts = append(opts,
		pkgcore.WithEventBus(broker.NewEventBus(cfg)),
		pkgcore.WithKVStore(redis.NewKVStore(cfg)),
		pkgcore.WithMailer(smtp.NewMailer(cfg)),
		pkgcore.WithObjectStore(s3.NewObjectStore(cfg)))
}
reg, err := pkgcore.NewKernel(mode, opts...).Bootstrap(ctx, tenancy.New(), billing.New())
```

Returning an error:

```go
return apperr.NotFound("billing.subscription_not_found").WithParam("id", id)
```

Full runnable versions of all of the above live in `example_test.go` (the shared wiring and standalone-mode examples, the object stores' constructors, and the SMTP mailer's construction) and `example_redis_test.go` (the Redis-backed distributed-mode ones), plus `apperr/example_test.go` and `config/example_test.go`, and are executed by CI.

## Rules

**Dependencies**

- Do not import any other speed module here, `dbkit` included. pkgcore is the floor; an import from above is a cycle. That is why `Module.Migrations` returns a plain `embed.FS` rather than a `dbkit` type.
- Do not add a third-party dependency to the root package without arguing for it in the pull request: it lands in every consumer's `go.sum`. Three are in today, and all three earned their place the same way — a deployment-mode implementation cannot be written against a weaker third party: koanf in the `config` subpackage, and, in the root package itself, go-redis v9 backing the distributed-mode `KVStore` and `EventBus`, and minio-go v7 speaking the S3 dialect every supported object service accepts for the distributed-mode `ObjectStore`. minio-go was chosen over the AWS SDK for the leaner dependency graph and for covering MinIO, Aliyun OSS and AWS S3 with one client; consumers still never import it, because `NewS3ObjectStore` builds its own client and no minio type crosses the seam.

**Interfaces and deployment modes**

- Do not expose a capability on an infrastructure interface that only one implementation can satisfy. Design against the weaker side, which is the standalone deployment mode: no server-side scripting, no pub/sub, no pipelines on `KVStore`.
- Do not branch on `DeploymentMode` outside kernel wiring. Business logic must not contain `if mode == DeploymentModeStandalone`.
- Do not write a mock for `KVStore`, `EventBus`, `Mailer` or `ObjectStore`. `NewMemoryKVStore`, `NewMemoryEventBus`, `NewConsoleMailer` and `NewLocalObjectStore` are the test doubles.
- Do not use `NewMemoryEventBus`, `NewMemoryKVStore`, `NewConsoleMailer` or `NewLocalObjectStore` as a distributed-mode fallback. The first three are single-process, so every replica would get a private instance (and a console mailer prints where nobody reads); a local directory is the standalone mode's own private root, which a distributed host has no business sharing. `DeploymentModeDistributed` fails assembly instead, and the host injects real ones with `WithEventBus`, `WithKVStore`, `WithMailer` and `WithObjectStore`.
- Do not silently change an interface's semantics when adding an implementation. `NewRedisKVStore` preserves the in-memory store's observable behaviour; where a Redis-backed bus cannot (asynchronous cross-process delivery, JSON payload shape, failures of remote handlers unobservable, no catch-up), the difference is documented on the constructor and spelled out to consumers before they choose it.
- Do not build a read-modify-write cycle out of `Get` + `Set`. Use `IncrByFloat` or `CompareAndSwap`; they are the only operations every backend can make atomic.
- Do not retain or hand out a caller's byte slice in a `KVStore` implementation, and do not perform an operation on a cancelled context — return the context error instead.

**Tenancy**

- Do not treat a missing tenant as "all tenants". `MustTenantFromContext` returns `ErrNoTenant` so callers fail closed; propagate that error rather than defaulting.
- Do not pass a tenant through a bare string context key. Use `WithTenant`.
- Do not invent a system purpose at the call site. Declare it with `RegisterSystemPurpose` from the module's own registration; an undeclared purpose is refused.
- Do not call `WithSystemContext` without an `Actor`, and record a `Ticket` whenever one exists. Every bypass must be attributable.
- Do not treat a system context as an authorization bypass. It suppresses tenant filtering only.

**Module contract**

- Do not add a method to `Module`. Under lockstep versioning that breaks all modules at once — add a field to `Registry` instead.
- Do not perform I/O in `Register`. It declares; the kernel decides when anything runs.
- Do not depend on module registration order. Declare the dependency in `DependsOn`; `Bootstrap` sorts, and reports a cycle or a missing dependency rather than guessing.
- Do not swallow a registrar error. A duplicate key is a bug across modules, not a merge, and nothing is registered when the call returns an error.
- Do not publish a domain event the module never declared with `Events.Publishes`. The declarations are the catalog `integration` maps onto the versioned public event schema.
- Do not decorate a shared `apperr` value expecting the receiver to change. `WithParam` and `WithCause` derive a new error.

**Errors and configuration**

- Do not put human-readable text in an `apperr` code. The code is machine-readable and stable; the client resolves it through its own i18n catalog.
- Do not change or reuse a released `apperr` code. It is part of the public API contract.
- Do not surface a wrapped cause in an API response. `WithCause` is for logs and `errors.Is`.
- Do not resolve runtime or tenant-overridable settings through `config.Loader`. It handles bootstrap values only: how to reach infrastructure, and which deployment mode is running.
- Do not hand-write the configuration reference. It is generated from the `ConfigItem` declarations.

**Documentation**

- Do not add an exported identifier without a doc comment, an `Example` in the matching `example_test.go`, and an entry in the tables above, in the same pull request.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrInvalidDeploymentMode` | `ParseDeploymentMode` on anything but standalone/distributed | Abort startup; the deployment mode is misconfigured |
| `ErrNoTenant` | `MustTenantFromContext` on an unscoped context | Fail closed. In a worker, rebuild the context with `WithTenant` |
| `ErrSystemActorRequired` | `WithSystemContext` with an empty `Actor` | Name the actor; the bypass is audited |
| `ErrSystemPurposeNotRegistered` | `WithSystemContext` with an undeclared purpose | Declare it with `RegisterSystemPurpose` |
| `ErrNotNumeric` | `IncrByFloat` on a key holding a non-number | The key is not a counter; the stored value is left untouched |
| `ErrInvalidMail` | `Mailer.Send` on a message with an empty From, no recipients, an empty recipient, a line break in a header, or no body | Validate the message; nothing reached the wire |
| `ErrInvalidObjectKey` | Any `ObjectStore` operation with a key that breaks the shared grammar (empty, overlong, an empty or `.`/`..` segment, NUL or backslash) | Validate the key before the call; nothing reached the backend. The error names the broken rule, never the key |
| `ErrObjectNotFound` | `ObjectStore.GetObject` on a key no object is stored under, every backend reporting the same sentinel | Handle absence as a normal outcome. `DeleteObject` never returns it: deleting a missing key is a success |
| `ErrDuplicateModuleName` | Two modules reporting the same `Name()` | Rename one; nothing was registered |
| `ErrDependencyCycle` | `DependsOn` forming a cycle | The error names the cycle; break it |
| `ErrMissingDependency` | Depending on a module absent from the bootstrap set | Add the module, or drop the dependency |
| `ErrDuplicateConfigKey` / `ErrDuplicateFeatureFlag` / `ErrDuplicatePermission` / `ErrDuplicateJobType` / `ErrDuplicateNotificationType` / `ErrDuplicateEventType` / `ErrDuplicateAuditAction` | The same key registered twice | Two modules own one key; decide which does |
| `ErrUnresolvedFeatureDependency` | A flag depending on a flag nobody registered | Register the flag, or drop the dependency |
| `ErrMissingDistributedEventBus` | `Bootstrap` on `DeploymentModeDistributed` with no bus wired | Inject the host's bus with `WithEventBus` |
| `ErrMissingDistributedKVStore` | `Bootstrap` on `DeploymentModeDistributed` with no store wired | Inject the host's store with `WithKVStore` |
| `ErrMissingDistributedMailer` | `Bootstrap` on `DeploymentModeDistributed` with no mailer wired | Inject the host's mailer with `WithMailer` |
| `ErrMissingDistributedObjectStore` | `Bootstrap` on `DeploymentModeDistributed` with no object store wired | Inject the host's object store with `WithObjectStore` |
| `ErrEventBusClosed` | `Publish` on a `RedisEventBus` after `Close` | Wiring error: a closed bus is out of the deployment; build a fresh one |
| `config.ErrMissingValue` | A `config:"required"` field left zero | The error names the key and every source consulted |
| `config.ErrInvalidValue` | A supplied value not applicable to its field | The error names the offending key and its source |
| `config.ErrSourceUnreadable` | An unparseable config file or malformed flag | Fix the source; startup aborts |
| `config.ErrInvalidTarget` | `Load` given a non-struct-pointer or a bad `config` tag | Programming error; fix the target type |

Design rationale lives in `docs/internal/01-architecture.md` (module graph and the `Registry` decision), `03-deployment-modes.md` (dual deployment modes) and `04-data-and-tenancy.md` (tenant isolation).
