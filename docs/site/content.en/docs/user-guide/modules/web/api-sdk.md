---
title: "@speed/api-sdk"
weight: 6
description: "The generated typed surface of the merged API document — orval output bound to the host's client through one hand-written binding (bindRequestFn), react-query hooks over the shared QueryClient, no tenant concept and no i18n resources."
---

# @speed/api-sdk

`@speed/api-sdk` is the frontend half of the spec-first discipline:
operation functions, TanStack Query hooks and response models generated
by orval — pinned at 8.17.0 and run through `pnpm dlx`, so orval never
enters the workspace lockfile — from `contracts/speed.yaml`, the merged
document that pinned redocly `join`s from the eleven platform-module
fragments (admin, ai-gateway, authn, billing, config, integration,
notification, org, pki, sharing, storage). Everything in `src/` except
one file is generator output, stamped with a DO-NOT-EDIT header that
carries the pinned orval version: tool drift between the generator and
the committed artifact shows up as a diff in the header itself.

What it is not: it performs no HTTP of its own. Every generated call
routes through the package's single hand-written binding — `src/runtime.ts`,
exported as the `@speed/api-sdk/runtime` subpath — which adapts orval's
axios-shaped calls onto the `@speed/api-client` request function your
host bound once at bootstrap.

## When to use it

Any frontend calling platform API operations uses this package for
them. Generated hooks and functions are the only sanctioned route —
never hand-write a call for anything a spec fragment covers (the
`speed/no-direct-http` rule of [@speed/api-client](/docs/user-guide/modules/web/api-client/)
enforces that). The package is also the compile-time floor for
in-workspace consumers: [@speed/auth-core](/docs/user-guide/modules/web/auth-core/) drives the
generated authn session surface through this binding, and the account
component family renders generated hooks into a component tree.

Your own application's API is not here. The reference app's notes,
cases and smilesim operations are deliberately not merge members; they
generate into the app's own app-owned SDK
(`examples/reference-app/web/src/app-api`, the `task api:gen:app`
leg), which the app's web host imports for its own surfaces while
platform operations keep riding this package. A delivered consumer
follows the same shape: platform operations through `@speed/api-sdk`,
your operations through your own app-owned SDK, both over the one
binding below.

## Installation and wiring

Peers are `react` and `@tanstack/react-query` v5: hosts share one
`QueryClient`/`QueryClientProvider` — the package never creates one.
Bind your client once, at bootstrap:

```ts
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'

bindRequestFn(
  createClient({
    baseUrl: window.location.origin, // scheme + host only — operation paths carry /api/v1
    accessTokenStore: createMemoryAccessTokenStore(),
    refreshAccessToken: () => session.refresh(),
  }),
)
```

`bindRequestFn` is last-bind-wins. The runtime also exports
`speedRequest`, the mutator generated code imports, and
`speedRequestCredentialless` — the per-operation override that declares
the authn session-refresh request credential-less (`omitAccessToken`),
so it carries no `Authorization` header and its 401 stays terminal
under the client's bearer-only refresh rule instead of re-entering the
refresh path. The app-owned SDK's own binding re-exports this subpath, so
one `bindRequestFn` call serves both generated surfaces.

Regeneration runs `pnpm dlx orval@8.17.0 --config orval.config.ts`
followed by `node scripts/orval-nodenext-fixup.mjs` from `web/`; the
Taskfile `api:gen` task and the `api-contract.yml` workflow run exactly
that pair, and every regeneration is followed by a porcelain
consistency gate. Never hand-edit `src/index.ts`, and never add another
hand-written file inside `src/` to bridge a generation gap: tooling
problems are fixed in tooling (the nodenext fixup script rewrites
orval's extensionless mutator imports to the explicit `.js` form that
nodenext builds require — it exists precisely so no bridge file is
needed).

## Core API and usage

Every platform fragment exports its module's group — hooks, plain
functions, request/response types and per-module error envelopes:

- **The authn group** covers the session lifecycle —
  `useAuthnLoginWithPassword`, `useAuthnLoginWithSMSCode`,
  `useAuthnRefreshToken`, `useAuthnLogout`, `useAuthnSwitchTenant`,
  `useAuthnVerifyStepUp`, registration, code sending, `/me`, session
  listing and revocation, MFA and social sign-in. It exists because
  [@speed/auth-core](/docs/user-guide/modules/web/auth-core/) consumes it: a spec change whose
  regenerated surface outgrows auth-core's calls fails that package's
  typecheck.
- **Every other module follows the same shape** — notification,
  billing, org, storage, sharing, pki, admin, integration and
  ai-gateway each export their module's hooks, plain functions, types
  and error envelopes. Queries and mutations are real react-query
  hooks returning the standard result shapes.
- **Error envelopes are per-module**, `{code, params}` typed per API;
  codes resolve to bilingual user-facing text in the consuming
  package's own catalogs — no i18n resources ship here.
- **No tenant concept in generated code**: no tenant header (tenant
  context travels inside the access token) and bare spec-path query
  keys. Namespacing tenant-scoped queries under your own query keys is
  a consumer-shell discipline — apply it where a cache must not
  outlive a tenant switch.

## Boundaries and notes

- Transport concerns — base URL, token store, refresh, retry, timeout,
  reporter — are entirely the host's `createClient` configuration;
  generated code carries none of them. Need different transport
  behaviour? Change the client, never the generated surface.
- The package never creates a `QueryClient`; the hooks need your
  provider, and cache invalidation happens through react-query on the
  keys generated code exposes.
- Regeneration overwrites `src/` wholesale, and the backend half is
  regenerated in the same flow: change the spec first
  (`api/openapi.yaml` → `task api:gen`), commit both halves together,
  in the API-contract order.
- Raw-byte bodies, uploads and SSE do not travel this surface — the
  client underneath is JSON-text only.

## Source

- [api-sdk AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md) —
  the package's authoritative contract.
