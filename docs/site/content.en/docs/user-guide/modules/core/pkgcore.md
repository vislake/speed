---
title: pkgcore
weight: 1
description: "The dependency floor — the Module/Registry/Kernel assembly contract, tenant context, the infrastructure seam interfaces, structured errors and the merged message catalog."
---

# pkgcore

The dependency floor of a speed-based service: the module every other
Go module imports, and the one that imports none of them.

pkgcore owns seven things and nothing else: the
`Module`/`Registry`/`Kernel` wiring contract — one `Register(reg Registrar)`
call per module; tenant context plus the raw system-context marker;
the infrastructure seam interfaces `KVStore`, `EventBus`, `Mailer` and
`ObjectStore` with the in-process implementations that double as test
doubles; the registry/capability/preset machinery `Bootstrap` resolves
and validates assemblies through; the merged backend message catalog;
the `DeploymentMode` enumeration; and the conformance suites
(`eventbustest`, `kvstoretest`, `mailertest`, `objectstoretest`) every
implementation of a seam must pass. Subpackages add `apperr` (the
structured error every module returns), `config` (the bootstrap loader
for flags/env/file — startup values only) and `i18n` (rendering
backend-generated content in the recipient's locale). Database access,
SQL-level tenant enforcement, logging, runtime configuration and job
execution are deliberately out of scope — those are the modules above
it.

## When to choose it

Every speed-based binary carries it — every module of the platform
builds on pkgcore. You use it in one of two roles:

- **As a module author** — your business module implements
  `pkgcore.Module` and contributes routes, config items, feature
  flags, permissions, job handlers, notification types, events and
  audit actions through one `Register` call. Do not add methods to
  `Module` later: under lockstep versioning that breaks every module
  at once, which is why cross-cutting mechanisms become new seats on
  the `Registrar` declaration face instead.
- **As a host** — your binary composes the modules with
  `Kernel.Bootstrap` and declares which topology it runs as. The
  bare `NewKernel()` default is standalone: in-process seams only,
  zero configuration, no external services.

## Wiring and minimal use

A module declares its whole surface in `Register` — no I/O, no
services started; registration order never matters, `DependsOn` and
`Bootstrap`'s sort decide:

```go
func (m *BillingModule) Register(reg *pkgcore.ComponentRegistry) error {
    reg.RoutesSeat().Mount("/api/v1/billing", m.router())
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

A host booting it — the module set is your own composition (`pkgcore`
ships no application):

```go
// The host's bootstrap target: one field per process-start key it
// resolves. go/pkgcore/config's loader fills it from flags, the
// environment, an optional config file and the struct's own defaults,
// and a field may pin its exact variable name with config:"env=...".
var boot struct {
    DeploymentMode string
}
if err := config.New().Load(&boot); err != nil {
    return err // the loader names the key and every source it consulted
}
mode, err := pkgcore.ParseDeploymentMode(boot.DeploymentMode)
if err != nil {
    return err
}
// The host's own module values: its business modules' pkgcore.Module
// implementations, plus the config and jobs modules it wires itself.
reg, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(mode)).
    Bootstrap(ctx, billingModule, orgModule)
if err != nil {
    return err // the Prepare stage named the component, the missing capability and the mode (ErrCapabilityUnsatisfied)
}
```

A distributed host swaps in real implementations instead of the preset
defaults: `WithEventBus(bus, pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart)`,
`WithKVStore`, `WithMailer`, `WithObjectStore` inject one implementation
plus its capability bits, or `WithPreset(pkgcore.PresetDistributed)`
names the built-in Redis/SMTP/S3 composition. Business code never
branches on the mode — it only ever sees the resolved seams on the
`Registry`.

Errors ride the `apperr` contract — constructors like
`apperr.NotFound("billing.subscription_not_found")`, decorated with
`WithParam`/`WithCause`, which derive new errors so package-level
sentinels stay shareable. Codes are the stable, machine-readable API
contract (never human text); parameters serialize verbatim into the
response body, so scalars only, and `WithSensitiveParam` for what a
client must not see.

## Core concepts and API essentials

- **The `Registry`** — one field per mechanism (`Routes`, `Config`,
  `Features`, `Permissions`, `Jobs`, `Notifications`, `Events`,
  `AuditActions`, `Retention`, `Schedules`), built with the three-argument
  `NewRegistry(bus, kv, mailer)` or installed by `Bootstrap`, which
  also resolves `ObjectStore()` and the merged `Locales()` catalog.
  `Registry.EventBus()` is the bus behind the registrar.
- **Seams and capabilities** — each seam interface is designed
  against the weakest registered implementation (no server-side
  scripting on `KVStore`), and implementations declare their capability
  bits: `MultiReplicaSafe`/`SurvivesRestart`/`Stateless` on the four
  infrastructure seams the assembly validates, plus
  `KeyNeverLeavesBoundary` on go/pki's `Signer` implementations, which
  declare it through their own registry — a bit the assembly does not
  compare against a requirement (the gap go/pki's docs record).
  `Bootstrap` fails a composition whose resolved implementation cannot
  satisfy the declared mode; a missing `SurvivesRestart` is a startup
  warning only, and a `Stateless` implementation is exempt from even
  that.
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
  self-register from `init()`; a preset that names one resolves only
  after the host blank-imports it — the accepted `database/sql`-style
  cost, with `ErrUnknownImplementation` naming the missing import.

## Boundaries and pitfalls

- A new cross-cutting mechanism belongs on the declaration face — a
  `Registrar` accessor plus the registry field behind it — never as a
  new `Module` method; `Register` must not perform I/O, and a
  registrar error is never a merge — duplicate keys fail registration.
- Do not write mocks for the seams: `NewMemoryKVStore`,
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
