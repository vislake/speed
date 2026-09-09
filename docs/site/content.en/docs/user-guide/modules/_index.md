---
title: Module reference
weight: 0
description: "The by-module reading of the user guides — one page per Go module: what it is for, when to choose it, how to wire it, its core concepts and its boundaries."
---

# Module reference

speed is not an application you run — it is independently released Go
modules and npm packages that your product pulls in and composes into
one binary. The [domain guides](../domains/) walk one product need end
to end; this section is the other reading: **one page per module**, in
the module dependency order, covering what the module is for, when to
choose it, how to wire it, its core concepts, and its boundaries and
pitfalls.

## How this section is organised

Pages are grouped along the module dependency direction — everything a
group uses sits in the groups below it, and a module never imports
above its own layer:

- **core** — the dependency floor every binary and every other module
  sits on: [pkgcore](./core/pkgcore/), [dbkit](./core/dbkit/),
  [tenancy](./core/tenancy/),
  [observability](./core/observability/),
  [config](./core/config/), [jobs](./core/jobs/),
  [ratelimit](./core/ratelimit/).
- **services** — platform services built on the core group:
  `storage`, `notification`, `pki`.
- **identity** — who your users are and what they may do: `authn`,
  `rbac`, `org`.
- **capabilities** — the product-facing capability modules on the top
  layers: `metering`, `billing`, `sharing`, `integration`,
  `ai-gateway`, `compliance`, `admin`.
- **tools** — developer-facing tooling: `saasctl`.
- **web** — the `@speed` npm packages (tokens, i18n, ui-kit,
  api-client, api-sdk, layout-kit, auth-core, auth-ui, tenancy-ui,
  product-shell, account-ui, billing-ui), the frontend counterparts of
  these modules.

Each group has its own overview page ([core](./core/) is here; the
remaining groups follow the same shape), and each module page is
self-contained: the "Source" section at the bottom links the module's
own `AGENTS.md`, which carries the full API tables, rules and error
index this site condenses.

## Relationship to the domain guides

The domain guides — [identity and access](../domains/identity-access/)
among them, one per product need — answer "my product needs X: which
modules, minimal steps, example". The module pages answer "I am
integrating module X: full usage". Both point at the same facts from
opposite directions; the error codes every module answers with are
listed once in the [error code index](../error-codes/).

Start with the [Quickstart](/docs/quickstart/) if you have not built a
speed-based service yet; the [walkthrough](../walkthrough-reference-app/)
and [operating](../operating/) pages cover the assembled shape this
section decomposes.
