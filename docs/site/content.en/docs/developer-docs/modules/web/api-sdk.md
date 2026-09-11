---
title: "@speed/api-sdk: the generated half of the API contract"
weight: 6
description: "Why the frontend typed surface is orval-generated from the merged document: the one hand-written transport binding (bindRequestFn), the pinned generator never entering the lockfile, the credential-less refresh mutator, platform merge membership, and why generated code carries no tenant concept and no i18n resources."
---

# @speed/api-sdk: the generated half of the API contract

The backend half of the speed API contract is generated server code that
participates in compilation; this package is the frontend half of the
same discipline. Operation functions, TanStack Query hooks and response
models are orval output from the merged OpenAPI document, every HTTP
call they make is routed through `@speed/api-client`, and nothing here
is hand-written except one binding file. This page explains the design of
that all-generated surface and of the binding; the regeneration mechanics
live on the [API contract](/docs/developer-docs/api-contract/) page,
and how to consume the package is the [api-sdk usage page](/docs/user-guide/modules/web/api-sdk/).

## Responsibility and boundary

- **`src/` is orval output except one file.** `src/index.ts` is
  generated wholesale, stamped with a DO-NOT-EDIT header that carries
  the pinned orval version; `src/runtime.ts` is the package's only
  hand-written source file, exported as the `./runtime` subpath.
- **Zero hand-written HTTP** — the `speed/no-direct-http` rule applies
  to the generated files too. Generated functions call a mutator
  (`speedRequest`) that adapts their axios-shaped calls onto the host's
  request function.
- **No tenant concept.** No tenant header exists anywhere — tenant
  context travels inside the access token — and generated query keys
  are bare spec-path strings; tenant query-key namespacing is a
  consumer-shell discipline, never package code.
- **No i18n resources.** Errors are the spec's typed `{code, params}`
  envelopes; codes resolve to bilingual text in consuming packages'
  own catalogs.
- **Peers only for React hosts**: `react` and `@tanstack/react-query`
  v5 are peers because hosts share one `QueryClient` —
  the package never creates one, and the hooks are useless without the
  host's provider tree.

## Design: why everything but one file is generated

Hand-written API calls are drift by construction — each one can silently
disagree with the spec, and nothing short of an integration test notices.
Generated code cannot drift: the spec is the only source, and the
generated surface participates in the type system of its consumers.
`@speed/auth-core`, the first in-workspace compile consumer of the authn
group, type-checks against these operations, so a spec change whose
regenerated surface outgrows the session's calls fails that package's
typecheck — the compile-enforcement loop, frontend half.

The DO-NOT-EDIT header is part of the mechanism, not decoration: it
carries the pinned orval version, so tool drift between the generator
and the committed artifact shows up as a diff in the header itself. The
same reasoning forbids a second hand-written file in `src/` as a bridge
over generation gaps — tooling problems are fixed in tooling (the fixup
below), never in shipped source.

## Design: why the transport binding is one file and one bind

Generated code must not know how a host wires its HTTP transport —
base URL, token store, refresh, retry, timeout, reporter are entirely
the host's `createClient` configuration. `runtime.ts` is that boundary:
`bindRequestFn`, called once by the host at bootstrap (last bind wins),
installs the request function every generated call rides; the
`speedRequest` mutator maps orval's axios-shaped options object onto
the `RequestFn` contract. The app-owned SDK (`task api:gen:app`'s
output for the reference app's own fragments) re-exports this same
subpath, so one `bindRequestFn` call at bootstrap serves both generated
surfaces.

One per-operation override exists: the authn session-refresh operation
is generated against a second mutator, `speedRequestCredentialless`,
which declares the request credential-less (`omitAccessToken`) — the
refresh authenticates with the refresh token in its body, and its 401
must stay terminal under api-client's bearer-only refresh rule rather
than re-entering the refresh path.

## Design: why the generator is pinned, not depended on

orval is pinned to 8.17.0 and never enters the workspace lockfile: it
is fetched on demand through `pnpm dlx`, with every runner — the
Taskfile `api:gen` task and the `api-contract.yml` workflow — using the
same command so the two cannot drift. A version bump must land in
`orval.config.ts`'s comment, the Taskfile and the workflow together.
The fixup script exists for the one genuine gap: orval emits mutator
imports without a file extension, which TypeScript accepts under
bundler resolution but rejects under nodenext (TS2835), where this
package's build runs. `web/scripts/orval-nodenext-fixup.mjs` rewrites
every such import to the explicit `./runtime.js` form after each
regeneration and exits non-zero when orval's emission changes — so
generator drift fails CI instead of shipping an unbuildable package.

## Design: what the merged document covers

The input is `contracts/speed.yaml`, the pinned-redocly `join` of the
eleven platform-module fragments (admin, ai-gateway, authn, billing,
config, integration, notification, org, pki, sharing, storage). Membership is
module-driven: every platform module with an HTTP fragment is a member,
while the
reference app's own fragments (notes, cases, smilesim) are deliberately
not — they are the app's own API, generated by the app-owned leg into
the app-owned SDK the app's web host imports (`src/app-api`). A
platform fragment therefore only reaches this package by entering the
merge, and each module's group ships the same shape — react-query hooks,
plain functions, types and error envelopes — with errors typed
per-module.

## Stable surface

The frozen parts are the bindings and the process, not the file list:
the `./runtime` subpath (`bindRequestFn`, the `speedRequest` mutator,
the `speedRequestCredentialless` override) is hand-written and stable;
the DO-NOT-EDIT boundary is a contract — `src/index.ts` is whatever
orval 8.17.0 generates from the merged document, and a direct edit is
a violation the next regeneration erases; and the peer family
(react + react-query v5) is the shared-QueryClient contract hosts
compose against.

## Source

- Package contract: [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md)
- Generator wiring: [web/orval.config.ts](https://github.com/vislake/speed/blob/main/web/orval.config.ts), [web/scripts/orval-nodenext-fixup.mjs](https://github.com/vislake/speed/blob/main/web/scripts/orval-nodenext-fixup.mjs)

## Related pages

- [The API contract](/docs/developer-docs/api-contract/) — the spec-first discipline, generation legs and porcelain gates this package is the frontend half of
- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — where the generated packages sit in the layers
- How to use it: [api-sdk in the user guide](/docs/user-guide/modules/web/api-sdk/)
- The rest of the web HTTP group: [api-client](/docs/developer-docs/modules/web/api-client/) — the transport behind the binding; [auth-core](/docs/developer-docs/modules/web/auth-core/) — the first in-workspace compile consumer of the generated authn surface
