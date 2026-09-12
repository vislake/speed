---
title: config
weight: 5
description: "The schema-first, database-backed settings store — config items and feature flags declared by every module, system-to-tenant scope tiers, hot update through events plus an anti-loss poller, and two pre-auth endpoints."
---

# config

speed's dynamic-configuration module: the schema-first,
database-backed settings store whose values change at runtime and take
effect hot — feature-flag enablement, tenant brand items, default
limits, AI-model defaults. Values live in the `configs` table under
two scope tiers, `system` (platform-wide) → `tenant` (per-tenant
overrides), with reads falling back from the narrow tier to the wide
one and then to the schema default. speed's other configuration
layer — process-start bootstrap input (flags, env and files) — is
outside this module's scope: it is declared on `pkgcore`'s bootstrap
declaration (the component descriptor's `BootstrapKeys`), resolved by the general-purpose
`pkgcore/config` loader (host-driven against its own target struct;
a zero-dependency package this module never imports), and its
key-material derivation convention — one purpose string per declared
key path (`pkgcore.BootstrapKeyPurpose`), composed with
`dbkit.DeriveBootstrapKey` — lives with that declaration and toolkit. This module
owns the runtime layer, and it is required for any multi-tenant host.

Modules declare items and feature flags on the registry during
`Register` (`reg.ConfigSeat().Add(pkgcore.ConfigItem{Key, Type, Default,
...})`, flags with their `DependsOn` chains); this module folds the
declarations into one schema, resolves flag dependencies at runtime,
and serves the effective values.

## When to choose it

Always — it is among the always-on modules with no off switch. Its
wiring differs from a plain module in one load-bearing way:
registration alone is not enough. `Register` declares what never needs
the assembled registry; `Attach` — called exactly once, once every
component's `Init` turn has run — folds the registry's *combined* item
and flag declarations into the schema, and demands what registration must
not touch: a migrated `configs` table, a `dbkit.Cipher` whenever any
registered item is `Sensitive` (`ErrCipherRequired` otherwise), and a
poll interval. The `*Service` `Attach` returns is what the host keeps.

## Wiring and minimal use

```go
// The composition selects the config component; the assembly constructs the
// module and drives it — Register declares during its Init turn, and the
// attached *Service is published from the module's Start turn, once every
// component's declarations are in.
reg := pkgcore.NewComponentRegistry()
// register the host's own components on reg (or let the loader select them)
if err := app.Assemble(ctx, reg, app.LoadSpec{Host: &hostConfig, Options: loaderOpts}); err != nil { /* handle err */ }

svc, err := pkgcore.Get[*config.Service](reg) // the service the assembly attached
// handle err

name, err := config.GetTyped[string](svc, ctx, "brand.site_name") // platform default
// handle err

tenantCtx := pkgcore.WithTenant(ctx, "acme")
if err := svc.Set(tenantCtx, config.ScopeTenant, "brand.site_name",
    config.Value{Data: "Acme Dental"}, "alice"); err != nil {
    // handle err
}
enabled, err := svc.IsEnabled(tenantCtx, "brand.custom_theme")
// handle err
```

The component is selected, never hand-built: the assembly constructs the
module, its `Init` turn declares through `Register`, and its `Start` turn
attaches the schema snapshot and publishes the `*Service`, which the host
reads back with `pkgcore.Get`. The db component has already applied every
selected component's declared migration set at the `Verify` stage, so the
`configs` table exists before the service attaches. A host that needs the
service *during* `Init` — before `Start` publishes it — calls
`configModule.Attach(reg)` from its own step component's `Init` and
publishes the returned service; the module's `Start` turn then completes
that snapshot instead of attaching a second service.

## Core concepts and API essentials

- **Scope, fallback, entitlements** — a value is addressed by the
  triple `(key, scope, tenant_id)`. The service — never the caller,
  never the HTTP layer — enforces each tier's entitlement on `Set`: a
  tenant write requires a tenant in the context and is attributed to
  it; a system write requires an audited system context
  (`ErrSystemScopeRequiresSystemContext` otherwise). The `user` tier
  is reserved and refused (`ErrUserScopeUnavailable`).
- **Canonical values and bounds** — every value is stored as its
  canonical string per type (decimal for ints, `time.Duration.String()`
  for durations); decode is the single choke point where a corrupt row
  surfaces as an error, never as a wrong-typed value. `GetTyped`
  supports `string`, `bool`, `int64` and `time.Duration`; bounds are
  enforced at write time, and validation errors never echo the
  offending value.
- **Sensitive items** — a `Sensitive` item is AES-GCM-sealed under
  the host's cipher before storage; plaintext exists only in the
  service's cache and an entitled `Get`. The `[redacted]` marker
  replaces it in change events, watcher deliveries, logs and errors;
  `Public`/`Sensitive` are mutually exclusive by validation, so the
  pre-auth endpoints cannot leak one by construction.
- **Hot update** — `Set` advances the process's own cache *before*
  publishing `config.item.changed` (carrying actor, old→new, redacted
  for sensitive keys); the event doubles as the write's audit record
  (persistent audit rows exist only through the optional `compliance`
  module). A failed publish does not roll the write back —
  `ErrAuditPublishFailed` is the host's signal when audit is
  mandatory. Replicas converge through the event *plus* an anti-loss
  poller on the host-chosen interval (default 30s; `0` disables it
  for single-instance hosts), so one lost event never leaves a
  replica serving stale configuration forever. `Watch` delivers each
  change as the event saw it.
- **Feature flags** — a flag is enabled only when it *and* every flag
  it depends on (transitively) report enabled, so disabling a
  dependency disables everything above it per tenant without a
  migration; cycles are rejected at Attach
  (`ErrFeatureFlagDependencyCycle`). `EnabledFlags` serves the
  enablement list to the frontend. Consumption semantics are the
  consuming modules' job — this module answers what is enabled.
- **Endpoints** — two pre-auth GET/HEAD endpoints at the exported
  `PathPublic` (`/api/v1/config/public`) and `PathSystemFeatures`
  (`/api/v1/config/features`) constants, named by hosts in their
  tenant-middleware allowlists, declared by the module's OpenAPI
  fragment (`config_getPublicConfig` / `config_getSystemFeatures`,
  generated into `@speed/api-sdk`). Both resolve the request's tenant
  through the host-wired `tenancy.Resolver` and fall back to platform
  defaults — never an error — because a login page that fails to
  render is the worst failure mode. One unset or corrupt public item
  is omitted from the snapshot, never allowed to take the endpoint
  down with it.

## Boundaries and pitfalls

- Do not read the `configs` table directly from another module: scope
  fallback, decryption, cache and flag semantics live in the
  `Service`; anything reaching around it re-implements a wrong subset.
- The `configs` table is platform data — deliberately not
  `dbkit.TenantScoped` — so it is the documented exception to the
  repository rules, not a pattern for tenant-owned data to copy.
- The two endpoints' wire contract is the module's OpenAPI fragment
  (`api/openapi.yaml`); the frontend's per-key reads and flag lookups
  still go through `@speed/api-client`'s typed wrappers, which remain
  the mapping layer under the generated operations. No
  config-*editing* UI ships.

## Source

- [config AGENTS.md](https://github.com/vislake/speed/blob/main/go/config/AGENTS.md)
- [config `example_test.go`](https://github.com/vislake/speed/blob/main/go/config/example_test.go)
