# app

app is speed's **application assembly layer**: the structure every
application's boot code is built from. It is the one module in this
repository with **no business domain** — it owns no tables, registers no
routes, config schema, feature flags, permissions, job handlers or audit
actions, and implements no `pkgcore.Module`. It exists so that what every
host would otherwise re-write by hand — the configuration load, the boot
order, the component drive, the HTTP helpers, the shutdown sequence — is
written once, in one place.

`docs/internal/01-architecture.md` draws it as a node above every module it
composes and records the **explicit exception to the module discipline
("modules are divided by domain cohesion")**: an assembly layer is warranted
despite having no domain, because the alternative is the same glue
hand-maintained in every consumer.

## Charter — what app may and may not be

- **Structure belongs here.** The loader, the stage driver, the shutdown, the
  HTTP helpers and the configuration orchestration are the module's subject;
  a piece of every host's boot sequence that is the same in every host is a
  candidate to live here.
- **Policy belongs to the host.** Which components compose the application,
  which values they are configured with, which seeds it writes, which keys
  the host's own configuration target declares, what its routes and rules are
  — all of it stays in the host and enters through a component descriptor, a
  load spec, a code override or a `chain.Config` field.
- **No implicit defaults.** The loader names the target it loads and fails
  the assembly when it is missing; a value the host does not supply stays
  unset. The one documented exception is the builtin composition layer: the
  standalone deployment default and the default-participating observability
  component, both overridable from any higher source.
- **No HTTP assembly, no listening, in the engine core.** `driver.go`,
  `loader.go` and `component_observability.go` contain neither: a host's own
  application component composes routes from the declaration seats and owns
  the listener. The reusable helpers (`chain.Chain`, `chain.Standard`,
  `PreAuthAllowlist`, the serve timeouts) stay in the module for hosts to
  compose with.
- **Never constructs infrastructure implementations.** app drives components
  and ships HTTP helpers; which EventBus/KVStore/Mailer/ObjectStore a
  process runs stays the application assembler's decision — selected as
  implementation components in the composition configuration, or built by a
  host's own component against the pkgcore interfaces — and which SQL
  dialect packages a binary carries stays the host's blank import. This is
  enforced mechanically, not just promised: app sits under `go/`, so
  `.golangci.yml`'s concrete-infrastructure depguard rules (redis, minio,
  asynq and the dialect drivers) apply to it exactly as to any business
  module.
- **A host's own modules never depend on app.** app is a leaf above the
  module graph; an import edge from a business module back into it is the
  point to stop.

## The engine (go/app, root package)

The engine is three pieces: the loader, the driver and the observability
component.

**The loader (`loader.go`, `loader_composition.go`)** is the bootstrap root.
It runs before the first stage, because what it resolves is what the
assembly plans from and what components read. It does three things in order:

1. loads the host's configuration target through a `pkgcore/config`
   Loader (the same options and sources the declared keys resolve on);
2. resolves the composition configuration from its five sources — builtin
   defaults, project file, environment, command line and the host's code
   override — and publishes it, with the configuration target, into the
   registry as a `pkgcore.ComponentConfig` (the code override via a
   `CompositionOverrides` Put, or `LoadSpec.Overrides` when the caller does
   not hold the registry);
3. resolves every registered component's declared `BootstrapKeys` on the
   same loader chain — through `pkgcore/config`'s declaration-driven entry
   (`ResolveDeclarations`), with no host struct field behind a declared key
   — and publishes the results as the assembly's by-purpose
   `pkgcore.BootstrapMaterial` source. A declared key whose value no source
   supplies resolves to nothing, and the consumer that needs it reports the
   missing material itself; a declaration that cannot be resolved as one
   schema (a malformed path, a format outside the closed set, a Sensitive
   key with no Description, two components declaring one path differently)
   fails the load with the stage, component, cause and remedy named. The
   declaring party's documented development defaults — a
   `pkgcore/config` defaults table handed in as a loader option
   (`ConfigDevDefaults`) — stand where a struct default used to: below the
   root-key derivation and the explicit sources.

Composition spelling (the loading implementation's convention; the design's
§6.1 leaves the carrying format to the implementation): the whole tree
travels under the `composition` envelope key — config file
`composition: {…}`, environment `APP_COMPOSITION__…`, flags
`--composition.…`. Component names appear in files and the environment with
their dots spelled as single underscores (`mailer.smtp` reads
`mailer_smtp`), because the double underscore already marks one level of
nesting; on the command line the dots stay literal and the name is matched
against the registered set by longest prefix. A selection value spelled
`false` (or `true`) in a text source reads as its boolean.

**The driver (`driver.go`)** only orchestrates: `Assemble` runs the loader
and walks the registry through Prepare, Construct, Verify, Init and Start;
`Shutdown` performs the two-phase close (the non-blocking Stop notification,
then the reverse-order Close, errors aggregated); `RunAssembly` is the sugar
— it creates the registry from the global registration plus extras, drives,
waits for the context (SIGINT/SIGTERM overlaid), and shuts down. Failure
semantics are the registry's: a failed Prepare leaves nothing to roll back,
and a failure from Construct on closes every constructed component in
reverse order, exactly once, before the error returns.

**The observability component (`component_observability.go`)** is registered
by the engine's `init` and selected by the builtin composition defaults, so
it participates unless a higher layer deselects it. Its `Prepare` (first
among the components' Prepare callbacks) initializes OTel from its resolved
`service_name` / `otlp_endpoint` block; its `Close` shuts the providers down
and flushes. The engine never initializes observability itself.

## Package layout — dependency cost is why it is split

| Package | Concern | Dependency closure |
|---|---|---|
| `go/app` (root) | the engine: the loader, the driver and the `RunAssembly` sugar, the observability component, and the HTTP helpers the engine and hand-composing hosts share (`AuthnAPIPath`, `ReadHeaderTimeout`/`ShutdownTimeout`, `PreAuthAllowlist`) | pkgcore (+ its config subpackage), config, observability, tenancy — and, through config, dbkit and its GORM. Every composition carries the root |
| `go/app/chain` | the fixed middleware chain: `chain.Standard` (the registry-derived derivation, over either registry shape's `RouteSource`: guard the mounted routes through the host's rbac rule table, split the authn and admin subtrees, mount the rest, delegate to `Chain`), `chain.Config`/`chain.Chain` (the direct path for a custom layout) — the order (authn outermost, then the optional impersonation decorator, then tenancy with the pre-auth allowlist), the authn/admin branches dispatched around it, validation (`chain.go`, `standard.go`) | root + authn + rbac + tenancy + pkgcore — bounded by the chain's own participants (the rule table is rbac's, the impersonation decorator stays a `func(http.Handler) http.Handler` the host builds, and no admin import is needed: the admin prefix arrives as `admin.APIPath` through an option) |
| `go/app/bridges` | the no-import seam bridges: `Entitlements`, `UsageRecorder`, `OrgFeatureGate`, `AuthnFeatureGate`, `ShareExpiryReader` (`bridges.go`, `sharing.go`) | ai-gateway, billing, metering, org, sharing, authn, config — paid only by hosts that wire those modules |

Runnable usage documentation (`example_test.go`) ships one example per
package. The split is deliberate, not incidental: a consumer importing only
the root pays the root's closure — measured with a throwaway module under
`GOWORK=off go mod tidy`, a bare consumer of the root package gets 36
`// indirect` entries (the sibling modules the root imports plus the koanf,
go-i18n, OTel and GORM stacks they pull) — so the root must never import
the chain's or the bridges' participants — nor any business module at all.

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
composition from the bootstrapped registry's mounted routes instead of
taking the pieces pre-assembled (its `RouteSource` parameter is answered by
both registry shapes — the module Registry and the component assembly's
`ComponentRegistry`): it admits every mounted route through the host's
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
compositions) both assemble through this module's component surface, both
running their full suites green. `tools/check_host_composition.py` is the
enforcement half — one ban set over both surfaces: neither host tree may
re-declare the kernel's symbols or re-grow its statements (the serve loop,
the liveness/authn path literals) in its own code, nor issue an
engine-owned assembly call — `dbkit.Open`, `dbkit.NewMigrationRegistry`,
`http.NewServeMux`, `jobs.NewStandaloneQueue`/`jobs.Wire`,
`signal.NotifyContext`, `chain.Chain`, `obs.Init`, `pkgcore.NewKernel`,
`.Bootstrap(`, or the component drive's `pkgcore.NewComponentRegistry` and
`Prepare`/`Construct`/`Verify`/`Init` — each with its one named-file
allowance where the host's own component legitimately owns the call. A
change to the engine, the chain, the allowlist or the serve lifecycle must
keep both consumers working; a change that cannot be adopted by the
skeleton's smallest selection (no authn, hence no `chain.Chain` call at
all) is a symptom the change does not belong here.

## Testing

Unit tier only, container-free: `go test -race ./...` from this
directory. The loader's five-source layering, its composition spellings,
the bootstrap material resolution, the declaration-set validations and the
root-key derivation are pinned in `loader_test.go`; the composition
pipeline's edge and refusal paths (the flag walk's skips, the text
spellings, the pass-through of an unregistered selection, the
one-override rule) in `loader_composition_test.go`; the driver — its
stage order, its rollback and its entry refusals — and the `RunAssembly`
sugar in `driver_test.go`; the observability component's refusals and
teardown halves in `component_observability_test.go`; the pre-auth
allowlist's method scoping in `kernel_test.go`. Fixtures live in
`test_support_test.go` (a host configuration target, the loader options a
bare test boot runs with and the marker product, so no business module
enters the test binary). Each package's `example_test.go` compiles and
runs the documented usage. Behavior owned by another module is not
re-pinned here (the route-label seed's mechanism lives with
`go/observability`, the enrollment of the `Entitlements` closure body is
exercised end-to-end by the reference app's consult flow) — this module's
tests pin the composition it adds, not the modules it composes.

## Known limitations

- `chain.Chain` installs `authn.NewPrincipalResolver()` and the platform
  allowlist by construction; a host whose chain needs a different
  resolver or a different pre-auth set assembles `tenancy.Middleware`
  itself rather than bending `chain.Config`. No consumer needs that
  today.
- A route conflict panics at assembly time rather than returning an error
  (`pkgcore.MountRoutes`' documented contract): a wiring error gets the
  loudest available report, and no rollback runs on a panic.
