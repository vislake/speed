# app

app is speed's composition toolkit: the host-neutral assembly an
application's own boot code is built from. It is the one module in this
repository with **no business domain** — it owns no tables, registers no
routes, config schema, feature flags, permissions, job handlers or audit
actions, and implements no `pkgcore.Module`. It exists so the composition
knowledge every host recomposes (the middleware chain's order, which
paths must work before a Principal exists, where authn's subtree is
mounted, which seam pairs are one shape) is written once, in one place,
instead of per host.

`docs/internal/01-architecture.md` draws it as a node above every module
it composes and records the **explicit exception to the module discipline
("modules are divided by domain cohesion")**: a composition-tools module
is warranted here despite having no domain, because the alternative is
the same glue hand-maintained in every consumer. The exception is narrow
by construction, and the charter below is the first gate against it
growing into a framework.

## Charter — what app may and may not be

- **Pure composition assembly.** Every exported function is a step a
  host calls from its own boot sequence: no state of its own, no
  lifecycle wrapper, no required entry point. A host that wants none of
  it loses nothing by not importing the package.
- **No policy defaults.** Nothing here decides a timeout, a module set,
  an allowlist entry or a deployment mode for a host; the platform-owned
  knowledge (which paths are pre-auth, the serve timeouts) is stated as
  the platform's convention, and everything host-specific enters through
  a `chain.Config` field or a function argument.
- **Never constructs infrastructure implementations.** app composes
  modules and HTTP; which EventBus/KVStore/Mailer/ObjectStore a process
  runs stays the application assembler's decision, wired through
  pkgcore's kernel options. This is enforced mechanically, not just
  promised: app sits under `go/`, so `.golangci.yml`'s
  concrete-infrastructure depguard rules (redis, minio, asynq and the
  dialect drivers) apply to it exactly as to any business module.
- **Not a framework and not a business layer.** No interface here exists
  to be implemented by consumers; the module's surface is functions over
  platform types. A change that makes a host's own module depend on app,
  or that gives app a policy of its own, contradicts the exception above
  and is the point to stop.

## Package layout — dependency cost is why it is split

| Package | Concern | Dependency closure |
|---|---|---|
| `go/app` (root) | The host *kernel*: `AuthnAPIPath`, `ReadHeaderTimeout`/`ShutdownTimeout`, `PreAuthAllowlist`, `RegisterMountedRoutes`, `ServeUntilShutdown` (`doc.go`, `kernel.go`) | pkgcore, tenancy, config, observability — every composition carries the root (the smallest generated project's `main.go` calls `ServeUntilShutdown`), so it stays minimal |
| `go/app/chain` | The fixed middleware chain: `chain.Config`, `chain.Chain` — the order (authn outermost, then the optional impersonation decorator, then tenancy with the pre-auth allowlist), the authn/admin branches dispatched around it, validation (`chain.go`) | root + authn + tenancy + pkgcore — bounded by the chain's own participants; no admin import (the impersonation decorator is a `func(http.Handler) http.Handler` the host builds) |
| `go/app/bridges` | The no-import seam bridges: `Entitlements`, `UsageRecorder`, `OrgFeatureGate`, `AuthnFeatureGate`, `ShareExpiryReader` (`bridges.go`, `sharing.go`) | ai-gateway, billing, metering, org, sharing, authn, config — paid only by hosts that wire those modules |

Runnable usage documentation (`example_test.go`) ships one example per
package. The split is deliberate, not incidental: a consumer importing
only the root pays the root's closure (measured with a throwaway module
under `GOWORK=off go mod tidy`), so the kernel must never import the
chain's or the bridges' participants.

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
- `AdminRoutes` — admin's own route. Its permissions are evaluated in
  `rbac.SystemDomain` against the caller's OWN unsubstituted Principal,
  so neither tenancy resolution nor impersonation substitution may run
  ahead of it (`go/admin/AGENTS.md` states both).

The route-level gate above the chain (`rbac.GuardRoutes` over the host's
own decision table) stays host-specific by design: the table is the
host's policy, not the platform's.

## Two mandatory consumers

`examples/reference-app` (the full composition: admin branch,
impersonation, tenant-status resolver, three extra allowlist entries) and
the project skeleton `saasctl new` materializes (the minimal
compositions) both assemble through this module. The skeleton's
`project/internal/` carries no host kernel of its own — a generated
project imports app like any other module — so the two hosts cannot drift
into two hand-maintained kernels. `tools/check_host_composition.py` is
the enforcement half: neither host tree may re-declare the kernel's
symbols or re-grow its statements (the serve loop, the liveness/authn
path literals) in its own code. A change to the chain, the allowlist or
the serve lifecycle must keep both consumers working; a change that
cannot be adopted by the skeleton's smallest selection (no authn, hence
no `chain.Chain` call at all) is a symptom the change does not belong
here.

## Testing

Unit tier only, container-free: `go test -race ./...` from this
directory. The chain's branch structure and decorator position, the
allowlist's method scoping and the bridges' field mapping are pinned in
`*_test.go` beside their targets; each package's `example_test.go`
compiles and runs the documented usage. Behavior owned by another module
is not re-pinned here (the route-label seed's mechanism lives with
`go/observability`, the enrollment of the `Entitlements` closure body is
exercised end-to-end by the reference app's consult flow) — this module's
tests pin the composition it adds, not the modules it composes.

## Known limitations

- `chain.Chain` installs `authn.NewPrincipalResolver()` and the platform
  allowlist by construction; a host whose chain needs a different
  resolver or a different pre-auth set assembles `tenancy.Middleware`
  itself rather than bending `chain.Config`. No consumer needs that
  today.
