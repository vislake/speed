# app

app is speed's **application assembly layer**: the structure every
application's boot code is built from. It is the one module in this
repository with **no business domain** — it owns no tables, registers no
routes, config schema, feature flags, permissions, job handlers or audit
actions, and implements no `pkgcore.Module`. It exists so that what every
host would otherwise re-write by hand — the boot order, the seam wiring, the
HTTP face, the shutdown sequence — is written once, in one place.

`docs/internal/01-architecture.md` draws it as a node above every module it
composes and records the **explicit exception to the module discipline
("modules are divided by domain cohesion")**: an assembly layer is warranted
despite having no domain, because the alternative is the same glue
hand-maintained in every consumer.

## Charter — what app may and may not be

- **Structure belongs here.** The assembly order, the seam wiring, the HTTP
  face, the lifecycle and the configuration orchestration are the module's
  subject; a piece of every host's boot sequence that is the same in every
  host is a candidate to live here.
- **Policy belongs to the host.** Which modules compose the application,
  which values they are configured with, which seeds it writes, which keys
  the host's own configuration target declares, what its routes and rules are
  — all of it stays in the host and enters through an option, a callback or a
  `chain.Config` field.
- **No implicit defaults.** The engine names what it needs (a configuration
  target, a database) and fails the assembly when it is missing; a value the
  host does not supply stays unset. A future configuration-file-driven
  composition of modules is a host concern that plugs into the engine's stage
  points; the engine itself pre-shapes nothing of it.
- **Never constructs infrastructure implementations.** app composes modules
  and HTTP; which EventBus/KVStore/Mailer/ObjectStore a process runs stays
  the application assembler's decision, wired through pkgcore's kernel
  options, and which SQL dialect packages a binary carries stays the host's
  blank import. This is enforced mechanically, not just promised: app sits
  under `go/`, so `.golangci.yml`'s concrete-infrastructure depguard rules
  (redis, minio, asynq and the dialect drivers) apply to it exactly as to any
  business module.
- **A host's own modules never depend on app.** app is a leaf above the
  module graph; an import edge from a business module back into it is the
  point to stop.

## The engine (`go/app`, root package)

`New(ctx, opts...)` assembles an application and does not listen;
`Run(ctx, opts...)` is `New` plus signal handling, listening and the ordered
drain. Both run the same eight stages, in one fixed order, and a failure at
any stage tears the built resources down (the same shutdown `Close` performs)
before returning the error:

| # | Stage | What runs |
|---|---|---|
| 1 | configuration | one loader fills `ConfigSpec.Host` and `ConfigSpec.Platform` |
| 2 | infrastructure | the platform cipher from `config.cipher_key`, `WithPreDB`, `dbkit.Open` |
| 3 | modules | `WithModules` constructs the module set |
| 4 | kernel | every module's migrations applied, `pkgcore.NewKernel(...).Bootstrap`, the bootstrap-key binding verified |
| 5 | attach | `Hooks.PostBootstrap` (the typed `Attach` calls) |
| 6 | wiring | `Hooks.PostAttach` (seam bridges, boot-time credentials, subscriptions) |
| 7 | HTTP face | the handler composed (see below) |
| 8 | background | `Hooks.PreServe`, then `Worker.Start` |

The option set: `WithConfig` (required) with the loader's own options
(`ConfigFile`, `ConfigArgs`, `ConfigEnvPrefix`, `ConfigRootKey`,
`ConfigRootKeyEnv`, `ConfigKeyDerivation`), `WithDatabase` (required) with
`DatabaseSpec` (`Dialect`, `DSN`, `AuditBus`, `AuditModels`), `WithPreDB`,
`WithModules` with `ModuleDeps{DB, Cipher}`, `WithKernelOptions`,
`WithObservability` with `ObservabilitySpec` (`ServiceName`, `OTLPEndpoint`),
`WithHTTP` with `HTTPSpec`, `WithHooks` with `Hooks`, `WithWorker` with the
`Worker` interface (`Start`/`Close`), and `WithoutBackgroundWorkers`.

**`PlatformConfig`** is the platform's normative declaration of its six
bootstrap keys (`authn.blind_index_key`, `authn.pii_cipher_key`,
`config.cipher_key`, `notification.contact_index_key`,
`org.invitation_email_index_key`, `pki.local_key_cipher_key`). A host holds it
— typically embedded in its own configuration target and tagged
`config:"-"`, because the engine loads it as its own target, which is what
keeps the six declared key paths unprefixed — pre-fills its development
defaults, and reads the material out for whichever module constructor needs
it. The declaration pins no environment names and no defaults: the
environment spelling is the loader's own derivation from the declared path
(with `ConfigEnvPrefix("APP_")`, `config.cipher_key` reads
`APP_CONFIG__CIPHER_KEY`), and a config file entry is spelled
`config.cipher_key` exactly.

**The HTTP face** composes in one fixed order: a `http.ServeMux` carrying
`obs.MountLiveness`, the host's `ExtraRoutes`, and — unless `HTTPSpec.Compose`
composes the protected face — every route `pkgcore.MountRoutes` mounts from
the registry; the registered routes seeded into the observability
middleware's label budget (`RegisterMountedRoutes`, before that middleware is
built); the host's `Middleware` wrapping outside-in; and the `SPA` wrapping
outside that when configured. `Compose` receives the prepared mux and returns
the handler that serves it — the seat for a host whose face is more than a
route list, typically `chain.Standard`'s product, which mounts the registry's
guarded routes onto the mux and wraps the whole composition in the fixed
middleware chain. **`Run` then wraps that composition in `obs.Middleware`
at serve time**, the outermost layer a served request reaches;
`Application.Handler` — what `New` composes and a test drives — stays the
host's own face, un-instrumented, so a host that serves it itself instruments
it itself rather than paying for a second layer. The composed `http.Server`
carries `ReadHeaderTimeout` and a `BaseContext` of the context `Run` received
(never the signal-derived one).

**`Close`** is idempotent and drains in one fixed order: ① the HTTP server
(`ShutdownTimeout`), ② the worker (`ShutdownTimeout`), ③ the kernel and then
the database, the reverse of their construction, ④ observability last, so
buffered spans and metrics are still exported.

## Package layout — dependency cost is why it is split

| Package | Concern | Dependency closure |
|---|---|---|
| `go/app` (root) | the engine: `New`/`Run`/`Application`/`Option`, `PlatformConfig` and the load orchestration, the HTTP face, the lifecycle, the hooks — plus the host-neutral kernel primitives the engine and hand-composing hosts share (`AuthnAPIPath`, `ReadHeaderTimeout`/`ShutdownTimeout`, `PreAuthAllowlist`, `RegisterMountedRoutes`) | pkgcore (+ its config subpackage), dbkit, observability, tenancy, spa — every composition carries the root |
| `go/app/chain` | the fixed middleware chain: `chain.Standard` (the registry-derived derivation: guard the mounted routes through the host's rbac rule table, split the authn and admin subtrees, mount the rest, delegate to `Chain`), `chain.Config`/`chain.Chain` (the direct path for a custom layout) — the order (authn outermost, then the optional impersonation decorator, then tenancy with the pre-auth allowlist), the authn/admin branches dispatched around it, validation (`chain.go`, `standard.go`) | root + authn + rbac + tenancy + pkgcore — bounded by the chain's own participants (the rule table is rbac's, the impersonation decorator stays a `func(http.Handler) http.Handler` the host builds, and no admin import is needed: the admin prefix arrives as `admin.APIPath` through an option) |
| `go/app/bridges` | the no-import seam bridges: `Entitlements`, `UsageRecorder`, `OrgFeatureGate`, `AuthnFeatureGate`, `ShareExpiryReader` (`bridges.go`, `sharing.go`) | ai-gateway, billing, metering, org, sharing, authn, config — paid only by hosts that wire those modules |

Runnable usage documentation (`example_test.go`) ships one example per
package. The split is deliberate, not incidental: a consumer importing only
the root pays the root's closure (measured with a throwaway module under
`GOWORK=off go mod tidy`: 36 `// indirect` entries, six of them
speed-internal — config, dbkit, observability, pkgcore, ratelimit, tenancy;
adding `go/app/chain` raises the count to 50, net +14), so the root must
never import the chain's or the bridges' participants — nor any business
module at all. The serve-and-drain
lifecycle has one shape only now: `Run` (with `obs.Middleware` applied at
serve time and the ordered `Close` drain); the pre-engine hand-composing
helper went away once both consumers assembled through the engine.

## The chain app/chain encodes

`chain.Chain` is the only place the fixed order lives:
`authn.Middleware(verifier)` outermost — the only order that verifies a
token exactly once and lets `tenancy.Middleware(authn.NewPrincipalResolver())`
read the already-verified Principal — then the optional impersonation
decorator, then tenancy with the pre-auth allowlist, then the host's
protected handler. Two branches are dispatched from authn's output ahead
of the tenancy chain entirely, and both are structural (mount paths, not
allowlist entries):

- `AuthnRoutes` — authn's subtree, split out by `authn.ExemptSubtree`.
  authn operations resolve the tenant from the Principal's own claim per
  operation, and enterprise SSO's dynamic `oidc:<tenant>` login-start
  path is not expressible as an exact (method, path) allowlist at all.
- `AdminRoutes` — admin's own route, split out of the mounted set by the
  prefix the host declares (`WithAdminPrefix`, `admin.APIPath`). Its
  permissions are evaluated in `rbac.SystemDomain` against the caller's OWN
  unsubstituted Principal, so neither tenancy resolution nor impersonation
  substitution may run ahead of it (`go/admin/AGENTS.md` states both).

`chain.Standard(reg, verifier, protected, opts...)` derives that whole
composition from the bootstrapped registry instead of taking the pieces
pre-assembled: it admits every mounted route through the host's
route-authorization table (`WithAuthorization` — `rbac.GuardRoutes` over
the host's own rules, so the table's exhaustiveness is checked and every
gated route is wrapped in rbac's fail-closed gate before it is mounted),
splits the authn subtree out with `authn.ExemptSubtree` and the admin
subtree out by prefix, and mounts everything else on the host's protected
mux before delegating the branch structure to `Chain`. The host supplies
the business half through options: the authorizer and rule table, the
admin prefix, the impersonation decorator, the tenant-status resolver and
its extra pre-auth allowlist entries. The billing quota domain does not
weave here: billing's quota mechanism is the entitlements seam checked
inside go/ai-gateway before a provider is reached
(`aigateway.WithEntitlements`, host-wired at module construction), not a
route-level decorator.

Which entry point a host uses is the host's call: `Standard` when the
registry's route set is the face (the reference app, and what the
skeleton's fuller selections compose), `Chain` when the layout is the
host's own (a selection with no authn module composes no chain at all).

## Two mandatory consumers

`examples/reference-app` (the full composition: admin branch,
impersonation, tenant-status resolver, three extra allowlist entries) and
the project skeleton `saasctl new` materializes (the minimal
compositions) both assemble through this module. `tools/check_host_composition.py`
is the enforcement half: neither host tree may re-declare the kernel's
symbols or re-grow its statements (the serve loop, the liveness/authn
path literals) in its own code. A change to the engine, the chain, the
allowlist or the serve lifecycle must keep both consumers working; a change
that cannot be adopted by the skeleton's smallest selection (no authn, hence
no `chain.Chain` call at all) is a symptom the change does not belong
here.

## Testing

Unit tier only, container-free: `go test -race ./...` from this
directory. The engine's stage order, its rollback, the platform
declaration's key-path mapping, the HTTP face's composition and the
shutdown order are pinned in the root package's `*_test.go` files beside
their targets, over fixtures in `internal/testutil` (a module built from
test-provided pieces, so no business module enters the test binary); the
chain's branch structure and decorator position, the allowlist's method
scoping and the bridges' field mapping are pinned the same way. Each
package's `example_test.go` compiles and runs the documented usage.
Behavior owned by another module is not re-pinned here (the route-label
seed's mechanism lives with `go/observability`, the enrollment of the
`Entitlements` closure body is exercised end-to-end by the reference app's
consult flow) — this module's tests pin the composition it adds, not the
modules it composes.

## Known limitations

- `chain.Chain` installs `authn.NewPrincipalResolver()` and the platform
  allowlist by construction; a host whose chain needs a different
  resolver or a different pre-auth set assembles `tenancy.Middleware`
  itself rather than bending `chain.Config`. No consumer needs that
  today.
- The engine's option values are host-computed at call time, so a value that
  derives from the loaded configuration (the DSN, the listen address, the
  kernel's seam composition) is resolved by the host's own loader pass
  before `New`/`Run`, with the same target handed to `WithConfig`; the
  engine's load re-resolves the identical sources and is idempotent, so the
  two passes cannot disagree.
- `WithObservability` is the only path by which the engine initializes
  observability, and it does so under `Run` alone; a host that calls `New`
  and serves the handler itself keeps responsibility for `obs.Init`, exactly
  as it keeps responsibility for its own listener — and, the same way, for
  wrapping what it serves in `obs.Middleware` (`Run` applies that wrap at
  serve time).
- A route conflict panics at assembly time rather than returning an error
  (`pkgcore.MountRoutes`' documented contract): a wiring error gets the
  loudest available report, and no rollback runs on a panic.
