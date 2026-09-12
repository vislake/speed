---
title: pkgcore
weight: 1
description: "The dependency floor — the module/component assembly contract, tenant context, the infrastructure module interfaces, structured errors and the merged message catalog."
---

# pkgcore

The dependency floor of a speed-based service: the module every other
Go module imports, and the one that imports none of them.

pkgcore owns seven things and nothing else: the
module/component assembly contract — every module implementation ships
a `Component` descriptor whose `Init` callback runs one
`Register(reg *pkgcore.ComponentRegistry)` declaration body; tenant
context plus the raw system-context marker;
the infrastructure module interfaces `KVStore`, `EventBus`, `Mailer` and
`ObjectStore` with the in-process implementations that double as test
doubles; the component-registry and capability machinery
the assembly resolves and validates compositions through; the merged backend message catalog;
the `DeploymentMode` enumeration; and the conformance suites
(`eventbustest`, `kvstoretest`, `mailertest`, `objectstoretest`) every
implementation of a module must pass. Subpackages add `apperr` (the
structured error every module returns), `config` (the bootstrap loader
for flags/env/file — startup values only) and `i18n` (rendering
backend-generated content in the recipient's locale). Database access,
SQL-level tenant enforcement, logging, runtime configuration and job
execution are deliberately out of scope — those are the modules above
it.

## When to choose it

Every speed-based binary carries it — every module of the platform
builds on pkgcore. You use it in one of two roles:

- **As a module author** — your business module ships a
  `pkgcore.Component` descriptor (its name, its lifecycle callbacks and
  its asset embeds) whose `Init` callback runs one
  `Register(reg *pkgcore.ComponentRegistry)` declaration body
  contributing routes, config items, feature flags, permissions, job
  handlers, notification types, events and audit actions. Do not grow
  the descriptor's contract later: under lockstep versioning that
  breaks every module at once, which is why cross-cutting mechanisms
  become new seats on the `*pkgcore.ComponentRegistry` declaration
  face -- or, where the mechanism's consumers read it through a
  product (the HTTP route and middleware faces, provided by the `http`
  component in `go/app/httpserve`), declaration tokens a component
  provides -- instead.
- **As a host** — your binary composes the components with
  `pkgcore.NewComponentRegistry()` and drives them through the seven
  stages with `app.Assemble` (or the `app.RunAssembly` sugar),
  declaring which topology the composition runs as. The `deployment`
  key of the composition configuration defaults to standalone, the
  zero-external-services shape.

## Wiring and minimal use

A module declares its whole surface in `Register` — no I/O, no
services started; registration order never matters, the
`Requires`/`Provides` contract tokens and the assembly's dependency
sort decide:

```go
func (m *BillingModule) Register(reg *pkgcore.ComponentRegistry) error {
    pkgcore.MountRoute(reg, "/api/v1/billing", m.router()) // the http component's route face, when selected
    if err := reg.PermissionsSeat().Add("billing:read", "billing:write"); err != nil {
        return err
    }
    if err := reg.EventsSeat().Publishes(pkgcore.EventDecl{
        Type: "billing.invoice.paid", PayloadType: "billing.InvoicePaid",
        Description: "An invoice was paid in full.",
    }); err != nil {
        return err
    }
    reg.EventsSeat().Subscribe("authn.user_created", m.openCreditLedger)
    return nil
}
```

A host booting it — the component set is your own composition
(`pkgcore` ships no application):

```go
// The host's bootstrap target: one field per process-start key it
// resolves. go/app's loader fills it from flags, the environment, an
// optional config file and the struct's own defaults, and a field may
// pin its exact variable name with config:"env=...".
var boot struct {
    DatabaseDSN string
}
// The host's own components: its business modules' descriptors, plus
// the config and jobs components it wires itself.
reg := pkgcore.NewComponentRegistry()
for _, c := range hostComponents() {
    if err := reg.Register(c); err != nil {
        return err
    }
}
// The composition configuration's code-override layer names the
// components this binary selects (nil selects, false deselects); the
// deployment key defaults to standalone.
composition := pkgcore.ComponentConfig{}.With("components",
    pkgcore.ComponentConfig{}.
        With("eventbus.memory", nil).
        With("kv.memory", nil).
        With("billing", nil))
spec := app.LoadSpec{
    Host:      &boot,
    Options:   []app.ConfigOption{app.ConfigEnvPrefix("BILLING")},
    Overrides: &app.CompositionOverrides{Config: composition},
}
// The eight-stage drive; a refusal names the component and the reason
// (a capability shortfall reports as ErrCapabilityUnsatisfied with the
// component, the missing bits and the mode).
if err := app.Assemble(ctx, reg, spec); err != nil {
    return err
}
```

A distributed host swaps in real implementations by selecting their
components in the composition configuration — `eventbus.redis`,
`kv.redis`, `mailer.smtp` and `objectstore.s3` are the built-in
Redis/SMTP/S3 composition, each resolving once the binary imports its
subpackage, each declaring its own capability bits. Business code never branches on
the mode — it only ever sees the resolved values through the
registry's accessors.

Errors ride the `apperr` contract — constructors like
`apperr.NotFound("billing.subscription_not_found")`, decorated with
`WithParam`/`WithCause`, which derive new errors so package-level
sentinels stay shareable. Codes are the stable, machine-readable API
contract (never human text); parameters serialize verbatim into the
response body, so scalars only, and `WithSensitiveParam` for what a
client must not see.

## Core concepts and API essentials

- **The `ComponentRegistry`** — one seat field per mechanism (`Config`,
  `Features`, `Permissions`, `Jobs`, `Notifications`, `Events`,
  `AuditActions`, `Retention`, `Schedules`), created with
  `pkgcore.NewComponentRegistry()` from the global component
  registration plus the host's own components; the seats accept writes
  only while the Init stage runs. Its accessors read the assembled
  values from the registry's by-type context — `EventBus()`,
  `KVStore()`, `Mailer()`, `ObjectStore()` and the merged `Locales()`
  catalog, each nil when the assembly carries none. `EventBus()` is the
  bus behind the registrar, where the host publishes into what modules
  subscribed to.
- **Modules and capabilities** — each module interface is designed
  against the weakest registered implementation (no server-side
  scripting on `KVStore`), and implementations declare their capability
  bits: `MultiReplicaSafe`/`SurvivesRestart`/`Stateless` on the four
  infrastructure modules the assembly validates, plus
  `KeyNeverLeavesBoundary` on go/pki's `Signer` implementations, which
  declare it through their own registry — a bit the assembly does not
  compare against a requirement (the gap go/pki's docs record).
  The assembly's Prepare stage fails a composition whose selected
  component cannot satisfy the declared mode; a missing
  `SurvivesRestart` is a startup warning only, and a `Stateless`
  implementation is exempt from even that.
- **Tenant context** — `WithTenant`/`TenantFromContext`/
  `MustTenantFromContext` (fail-closed: `ErrNoTenant`, never "all
  tenants"), and `WithSystemContext` as a marker that suppresses
  nothing by itself — who may use it, and how it is audited, is
  `tenancy`'s wrapper's business. `WithActor`/`WithOnBehalfOf` layer
  independently so an impersonated action's audit record can carry
  both identities.
- **The catalog** — backend handlers never translate responses (they
  return codes); `pkgcore/i18n` renders the content the backend
  generates itself — emails, invoices, notification copy — in the
  recipient's locale, and a missing message is an error, never a
  fallback language.
- **Subpackage implementations** — Redis, PostgreSQL, NATS, Memcached
  and S3 implementations live in their own subpackages and
  self-register from `init()`; a composition can select one only after
  the host blank-imports its subpackage — the accepted
  `database/sql`-style cost, with `ErrUnknownComponent` naming the
  component and listing the registered ones.

## Boundaries and pitfalls

- A new cross-cutting mechanism belongs on a declaration face — a
  seat accessor on the `ComponentRegistry` plus the registrar behind
  it, or, where consumers read it through a product rather than the
  registry (the `http` component's route and middleware faces), a
  declaration token a component provides — never as a new descriptor
  callback; `Register` must not perform
  I/O, and a registrar error is never a merge — duplicate keys fail
  registration.
- Do not write mocks for the modules: `NewMemoryKVStore`,
  `NewMemoryEventBus`, `NewConsoleMailer` and `NewLocalObjectStore`
  are the doubles, and the conformance suites are the mandatory check
  for any implementation you register yourself.
- A `SystemPurpose` must be declared with `RegisterSystemPurpose`
  before `WithSystemContext` accepts it, and a system context is not
  an authorization bypass.
- The root package carries no third-party dependencies; the SDKs the
  distributed implementations need are confined to their subpackages.
  Measure before adding one: it lands in every consumer's `go.sum`.

## Source

- [pkgcore AGENTS.md](https://github.com/vislake/speed/blob/main/go/pkgcore/AGENTS.md)
- [pkgcore `example_test.go`](https://github.com/vislake/speed/blob/main/go/pkgcore/example_test.go)
