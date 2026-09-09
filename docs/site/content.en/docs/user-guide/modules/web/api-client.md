---
title: "@speed/api-client"
weight: 5
description: "The frontend's single home of hand-written HTTP — createClient: injectable fetch, a memory-only access-token store, silent single-flight 401 refresh, conservative transient retries, ApiError normalisation and the reporter seam."
---

# @speed/api-client

`@speed/api-client` is where every HTTP request the speed frontend
makes is defined and performed. One `createClient` call wires the
transport decisions — an injectable `fetch`, a memory-only
access-token store, a per-request timeout, a silent single-flight 401
refresh, a conservative transient-retry budget and a structured
reporter — into one typed request function (`RequestFn`) the rest of
the frontend calls.

It is deliberately the *only* package that hand-writes HTTP: the
`speed/no-direct-http` ESLint rule makes a direct
`fetch`/`window.fetch`/`globalThis.fetch` call, a `new
XMLHttpRequest`, or an `axios`/`node-fetch` import an error in every
other package's `src`, with this package as the rule's single
whitelist. The client ships no UI, no i18n resources and no storage
API: error `code`s map to bilingual text in the consuming package's
own catalogs, and the access token lives in memory only — an access
token in `localStorage` is a credential an XSS walks away with.

It is not a session layer: the refresh token never enters this
package — the authn API returns it in the token-issuing response body
and sets no refresh cookie — so a session layer such as
[@speed/auth-core](/docs/user-guide/modules/web/auth-core/) holds it
in a closure and drives the refresh operation through the seam this
package only defines.

## When to use it

Every host that talks to a speed backend uses this package in exactly
one place: its bootstrap builds one client and binds it into the
generated surface ([@speed/api-sdk](/docs/user-guide/modules/web/api-sdk/))
with `bindRequestFn`. You rarely call the client directly — generated
operations do, and transport behaviour changes by reconfiguring this
one client, never generated code. Use the client's own functions only
where no spec fragment exists: `go/config`'s two pre-auth endpoints
are hand-kept (see [config](/docs/user-guide/modules/core/config/)),
via `fetchPublicConfig`/`fetchSystemFeatures`, or via the
`usePublicConfig`/`useFeature` hooks of the `@speed/api-client/react`
subpath when you render React.

## Installation and wiring

```ts
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession } from '@speed/auth-core'

const accessTokenStore = createMemoryAccessTokenStore()
const session = createAuthSession(accessTokenStore)   // the session layer, below

bindRequestFn(
  createClient({
    baseUrl: '/api/v1',                        // or scheme + host + prefix
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),   // silent 401 refresh
    timeoutMs: 10_000,
  }),
)
```

`fetch` is injectable (tests pass a deterministic stand-in); when
omitted, the environment's global `fetch` is captured at construction,
never looked up per call. The store starts empty, so requests carry no
`Authorization` until a login fills it. Build the client once and keep
the reference — the sharing hooks key their cache off its identity,
and `bindRequestFn` is a last-bind-wins single binding.

## Core API and usage

- **One request function, one error type.** `RequestFn` is
  `<T>(path, options?) => Promise<T>`. Every failure rejects an
  `ApiError` carrying `status` (0 when no response arrived), `code`,
  optional `traceId`/`params`/`details`, `attempts`, and `auth` — true
  exactly for HTTP 401. An envelope-answering server surfaces its
  `code` verbatim; anything else (a proxy error page, a dead network,
  a timeout) gets a reserved `client.*` code — `client.network`,
  `client.timeout`, `client.protocol`, `client.http.<status>`. The
  prefix is reserved by mechanism, not convention: `client` is no
  module's domain, so an envelope that borrows it is refused at parse
  time and surfaces as the honest `client.http.<status>`. Distinguish
  with `isApiError`; `isTransportFailure` answers whether the request
  never completed — its `status` is 0, the one status no HTTP response
  can carry, so the answer is not forgeable.
- **Bearer auth without a storage API.** The `AccessTokenStore` seam
  is two synchronous methods (`get`/`set`); the memory implementation
  is the only one shipped. The token is re-read before every attempt,
  so a retried request carries the fresh token. A request can declare
  `omitAccessToken` and travel credential-less. No tenant header
  exists anywhere — tenant context travels inside the access token.
- **Silent 401 refresh, bearer-only.** When a request that presented a
  bearer token answers 401 and `refreshAccessToken` is configured, one
  refresh runs — concurrent 401s share a single in-flight refresh —
  and the refused request is retried exactly once, any method, outside
  the transient-retry budget. Refresh failure rejects the original 401
  as an auth `ApiError` and reports `access token refresh failed`. A
  401 on a credential-less request surfaces untouched, and that rule is
  load-bearing: the session-refresh operation itself travels
  credential-less (`omitAccessToken`), so a refused refresh token
  terminates instead of re-entering the refresh path.
- **Transient retries, conservatively.** Only idempotent methods
  (GET/HEAD/OPTIONS) retry, only on 429 (honouring `Retry-After`,
  capped at `maxDelayMs`), 502/503/504, network failures and timeouts,
  under `DEFAULT_RETRY_POLICY` (3 attempts, 200 ms initial, 4 s
  ceiling) with full-jitter backoff. Caller cancellation is never
  retried and never wrapped: aborting your signal rejects the raw
  `AbortError`, so query layers keep standard cancellation semantics.
  The per-attempt timeout covers the response body, and the backoff
  sleep races your signal.
- **Structured reporting.** The `Reporter` seam receives a constant
  English message plus snake_case attributes; the default console sink
  is a stopgap, replaced through `ClientOptions.reporter`.
- **The config hooks** live at the isolated `./react` subpath, keeping
  React out of the main entry's import graph. `usePublicConfig(api)`
  returns `{ data, error, isLoading, refresh }` shared by every
  component using the same `api` — one fetch, not one per component —
  and `useFeature(api, key)` composes on that same cache, returning a
  `boolean` that is `false` while loading and on error, never
  throwing. Both endpoints resolve tenant server-side from the request
  host, so neither hook takes a tenant argument.

## Boundaries and notes

- Build the client once and share the reference; a fresh `createClient`
  per render defeats the hooks' shared cache and refetches every time.
- Uploads, SSE and raw-byte bodies are not shipped: this transport
  reads and writes JSON text only.
- The client is not a session: it never clears the store to make a
  refused refresh surface, and the refresh token itself is the session
  layer's business ([@speed/auth-core](/docs/user-guide/modules/web/auth-core/)).
- Nothing here emits text a user reads — error codes map to bilingual
  text in the consuming package's own catalogs (a point made above).

## Source

- [api-client README](https://github.com/vislake/speed/blob/main/web/packages/api-client/README.md) —
  the full public surface, the quick start and the "what is
  deliberately not here" list.
- [api-client AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md) —
  the package's authoritative contract, including the config hooks'
  caching contract.
