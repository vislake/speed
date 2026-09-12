---
title: app
weight: 1
description: "Design of go/app — why the one module with no business domain exists, how its three packages are split by dependency cost, why the loader carries no implicit defaults and why the middleware order is fixed the way it is."
---

# app

`go/app` is speed's **application assembly layer**: the structure every
application's boot code is built from, and the one module in the
repository with no business domain. The [app usage
page](/docs/user-guide/modules/app/) shows the calls; this page is why
the module draws its boundaries where it does.

## Responsibility and boundary

The split the module holds itself to is **structure belongs here,
policy belongs to the host**:

- Here: the loader (the configuration load and the composition plan),
  the stage driver, the shutdown sequence, the HTTP helpers the engine
  and its consumers share (`AuthnAPIPath`, the serve timeouts,
  `PreAuthAllowlist`), the fixed middleware chain and the bridges.
- With the host: which components compose the application, which values
  they are configured with, which seeds it writes, which keys the host's
  own configuration target declares, what its routes and rules are.
  All of it enters through a component descriptor, a load spec, a code
  override or a `chain.Config` field.

Three prohibitions sharpen the boundary:

- **No implicit defaults.** The loader names the target it loads and
  fails the assembly when it is missing; a value the host does not
  supply stays unset. The one documented exception is the builtin
  composition layer — the standalone deployment default and the
  default-participating observability component — and both are
  overridable from any higher source.
- **No HTTP assembly, no listening, in the engine core.** `driver.go`,
  `loader.go` and `component_observability.go` contain neither; a
  host's own application component composes routes from the declaration
  seats and owns the listener.
- **Never constructs infrastructure implementations.** Which
  EventBus/KVStore/Mailer/ObjectStore a process runs stays the
  assembling application's decision, and which SQL dialect packages a
  binary carries stays the host's blank import. This is enforced
  mechanically, not just promised: app sits under `go/`, so the
  repository's concrete-infrastructure depguard rules (redis, minio,
  asynq and the dialect drivers) apply to it exactly as to any business
  module. And a host's own modules never depend on app — an import edge
  from a business module back into it is the point to stop.

## Why a domain-less module exists

The module discipline divides modules by domain cohesion, and app has
no domain: it registers no routes, config schema, feature flags,
permissions, job handlers or audit actions, and implements no
module contract. It is the discipline's **one recorded exception**,
and the reasoning is an exchange: an assembly layer is warranted
precisely because the alternative is the same glue — the configuration
load, the boot order, the component drive, the shutdown sequence —
hand-maintained in every consumer, drifting host by host. Two
consumers make the case concrete: the reference app (the full
composition) and the project skeleton `saasctl new` materializes (the
minimal selections) both assemble through this module's surface.

## Three packages, split by dependency cost

The package layout is decided by measured cost, not by taste. A bare
consumer of the root package — measured with a throwaway module under
`GOWORK=off go mod tidy` — pays 36 `// indirect` entries: the sibling
modules the root imports (config, observability, tenancy) plus the
koanf, go-i18n, OTel and GORM stacks they pull. That measurement drives
the split:

| Package | Closure | Consequence |
|---|---|---|
| root | pkgcore (+ its config subpackage), config, observability, tenancy — and through config, dbkit and its GORM | every composition carries it; it must never import the chain's or the bridges' participants — nor any business module |
| `chain` | root + authn + rbac + tenancy + pkgcore — bounded by the chain's own participants | the impersonation decorator stays a plain `func(http.Handler) http.Handler` the host builds, and the admin prefix arrives as a string through an option, so no admin import is needed |
| `bridges` | ai-gateway, billing, metering, org, sharing, authn, config | paid only by hosts that wire those modules |

The guiding rule: the root is the floor every composition carries, so
anything that would widen its closure moves out to the package whose
own participants need it.

## The loader: five sources, one declaration site per key

The loader runs before the first stage, because what it resolves is
what the assembly plans from — nothing can be chosen before it has run,
so no composition can deselect it. It does three things in order: load
the host's configuration target through a `pkgcore/config` Loader (the
same options and sources the declared keys resolve on, so a host key
and a declared key can never resolve differently for one assembly);
resolve the composition configuration from its five sources (builtin
defaults, project file, environment, command line, host code override)
and publish it with the configuration target into the registry; and
resolve every registered component's declared `BootstrapKeys` on the
same chain.

The third step is where the design saves a struct: a declared key has
**no host struct field behind it** — a declaration is resolved where it
is made — so nothing binds it and nothing can fail to bind. A declared
key whose value no source supplies resolves to nothing, and the
consumer that needs it reports the missing material itself. What does
fail the load, before anything is constructed, is a declaration set
that cannot be resolved as one schema: a malformed key path, a format
outside the closed set (string, int, bool, hexkey), a Sensitive key
with no Description, or two components declaring one key path
differently — one key path has one resolution per assembly, and which
declaration won would be undecidable. Every problem names the stage,
the declaring component, the reason and the remedy.

The declaring party's documented development defaults
(`ConfigDevDefaults`) stand where a struct default used to: below the
root-key derivation and the explicit sources, so a deployment taking
its key material from a secret store passes none and a declared key
resolves to nothing until a source supplies it.

## The chain: why this order is the order

`chain.Chain` is the only place the fixed middleware order lives:
`authn.Middleware(verifier)` outermost — the **only order that verifies
a token exactly once**. A `tenancy.Resolver`'s signature cannot hand a
verified JWT's claims to anything downstream, so running tenancy first
would force verifying every token twice over two code paths free to
drift — and the path that ends up deciding would be the one that
authenticated nothing. Verified once, `tenancy.Middleware` with
`authn.NewPrincipalResolver()` reads the already-verified Principal out
of the request context, and remains the only place the tenant context
is set. authn's middleware is *optional* auth (a missing token proceeds
with no Principal; an invalid one 401s immediately), which is what
makes tenancy's fail-closed default — refuse any request whose
(method, path) is not allowlisted AND whose resolver failed — the thing
that protects every mounted route with no per-route wrapping.

Two branches are dispatched from authn's output **by structure** (mount
paths, not allowlist entries), each for a reason an allowlist cannot
express:

- **authn's own subtree**: authn operations resolve the tenant from the
  Principal's own claim per operation, and enterprise SSO's dynamic
  `oidc:<tenant>` login-start path cannot be enumerated as an exact
  (method, path) allowlist at all.
- **admin's route**: its permissions are evaluated in `rbac.SystemDomain`
  against the caller's own real, unsubstituted Principal, so neither
  tenancy resolution nor impersonation substitution may run ahead of
  it; the impersonation decorator itself slots between authn and
  tenancy — its one correct position — because it reads the real,
  already-verified Principal and substitutes a target Principal for
  everything downstream, only when a valid impersonation grant id is
  present.

`chain.Standard` derives the whole composition from the bootstrapped
registry instead of taking the pieces pre-assembled — its `RouteSource`
parameter is answered by both registry shapes (the module Registry and
the component assembly's `ComponentRegistry`) — with the host supplying
the business half through options. `chain.Chain` is the direct path for
a host with a custom layout; a selection with no authn module composes
no chain at all. The choice of entry point is the host's, and that
flexibility is deliberate: `Standard` for the registry's route set as
the face, `Chain` when the layout is the host's own.

## The seven-stage drive and its failure semantics

The driver only orchestrates. `Assemble` runs the loader and walks the
registry through Prepare, Construct, Verify, Init and Start; `Shutdown`
performs the two-phase close (the non-blocking Stop notification, then
the reverse-order Close, errors aggregated); `RunAssembly` is the sugar
that creates the registry from the global registration plus extras,
drives, calls the host's serve step (the optional `ServeFunc`, handed
the signal-overlaid context and the live registry; nil waits the
context out itself) and shuts down — and a serve failure does not skip
the close: the two-beat shutdown runs regardless and the failure joins
its result. Failure semantics are the registry's: a failed Prepare
leaves nothing to roll back, and a failure from Construct on closes
every constructed component in reverse order, exactly once, before the
error returns — so a caller never has to attempt a teardown of its own.
One hole is accepted and documented: a route conflict panics at
assembly time (`pkgcore.MountRoutes`' contract), getting the loudest
available report, and no rollback runs on a panic.

## Enforcement: one kernel, two hosts, a gate

The shared-kernel property is protected by tooling rather than
convention. `tools/check_host_composition.py` scans both consumer trees
(the reference app and the skeleton's embedded project) and refuses:
any re-declaration of the kernel's identifiers in a host's own code;
any re-growth of the statements the kernel owns (the serve loop, the
liveness and authn path literals); and any re-issued engine-owned
assembly call (`dbkit.Open`, `dbkit.NewMigrationRegistry`,
`http.NewServeMux`, `jobs.NewStandaloneQueue`/`jobs.Wire`,
`signal.NotifyContext`, `chain.Chain`, `obs.Init`, `pkgcore.NewKernel`,
`.Bootstrap(`, `pkgcore.NewComponentRegistry`, and the component
stages) — with the one named-file allowance, where one applies, for the
host file that legitimately owns the call; the signal overlay and the
jobs pair carry none, since the engine's `RunAssembly` owns every host's
signal handling and background execution reaches a host through the
`queue.standalone` component. The depguard rules cover the
no-infrastructure-construction half for app as for every other module.

The gate's design point doubles as the module's adoption test: a change
to the engine, the chain, the allowlist or the serve lifecycle must
keep both consumers working, and a change that cannot be adopted by the
skeleton's smallest selection (no authn, hence no `chain.Chain` call at
all) is a symptom that the change does not belong here.

## Trade-offs and the frozen surface

- **A domain-less module over per-host glue.** The exception is the
  trade: one more module in the graph and one more export surface to
  freeze, in exchange for a boot shared by construction instead of by
  imitation.
- **Measured, split closures over convenience imports.** The 36-entry
  root measurement is the reason the chain and the bridges are not in
  the root package even though most hosts end up importing all three.
- **A fixed chain order, with the escape hatch outside it.** The order
  is not parameterizable: `chain.Chain` installs `authn.NewPrincipalResolver()`
  and the platform pre-auth allowlist by construction, and a host whose
  chain needs a different resolver or a different pre-auth set
  assembles `tenancy.Middleware` itself rather than bending the config.
  No consumer needs that today; the known-limitation note is the honest
  boundary.

Frozen for consumers: `Assemble`, `Shutdown`, `RunAssembly`, `ServeFunc`,
`Load`, `LoadSpec`/`CompositionOverrides`, the `Config*` option constructors,
the root helpers (`AuthnAPIPath`, `ReadHeaderTimeout`,
`ShutdownTimeout`, `PreAuthAllowlist`), and the `chain`/`bridges`
packages' entry points. The observability component participates by
default and is configured through the composition block
(`service_name`, `otlp_endpoint`) like any other component.

## Source

- Module discipline: [go/app/AGENTS.md](https://github.com/vislake/speed/blob/main/go/app/AGENTS.md)

## Related pages

- Usage: [app in the user guide](/docs/user-guide/modules/app/)
- [Architecture](/docs/developer-docs/architecture/) — where the
  assembly layer sits above the module graph
- [pkgcore design](/docs/developer-docs/modules/core/pkgcore/) — the
  `Component`/`ComponentRegistry` contract the driver walks
