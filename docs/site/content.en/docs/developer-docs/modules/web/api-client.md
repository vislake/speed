---
title: "@speed/api-client: the single home of hand-written HTTP"
weight: 5
description: "Why api-client is the frontend's only hand-written HTTP layer: injectable fetch captured at construction, a memory-only access-token store, the bearer-only single-flight 401 refresh, conservative transient retries, one ApiError type with a mechanism-reserved client.* namespace, and the React-isolated config hooks."
---

# @speed/api-client: the single home of hand-written HTTP

Every request the frontend makes — the generated calls of `@speed/api-sdk`,
the operations of the `@speed/auth-core` session, a host's own config
reads — travels through one request function built by one `createClient`
call. This page explains the design decisions behind that single seam and
the threats each one answers; how to drive it day to day is the [api-client
usage page](/docs/user-guide/modules/web/api-client/) in the user guide.

## Responsibility and boundary

- **The one whitelist of the `speed/no-direct-http` rule.** A direct
  `fetch`, a `window.fetch` / `globalThis.fetch`, a `new
  XMLHttpRequest`, or an axios/node-fetch import in any other
  package's `src` is an ESLint error, and this package is the rule's
  single config-level whitelist (rule and tests in
  `web/eslint-rules/`). One home means transport behaviour — auth,
  retry, timeout, error shape — is decided once and shared by every
  caller; the generated SDK and the session layer both call down into
  it, never around it.
- **No UI, no i18n resources, no storage API.** Error codes map to
  bilingual text in the consuming package's own catalogs — nothing
  here emits user-facing text. The access-token store is memory-only
  by design (below).
- **No tenant header anywhere.** Tenant context travels inside the
  access token; the client has no tenant concept to attach.
- **JSON text only** — uploads, SSE and raw-byte responses are not
  shipped, each reason recorded in the package README.
- **The config hooks live behind the isolated `./react` subpath**,
  mirroring `@speed/i18n`'s `./mui-locale`: the main entry stays
  dependency-free, react a required peer of the subpath only.

## Design: why every failure is one `ApiError`

Every failed request rejects one `ApiError` carrying the API envelope's
`code` / `traceId` / `params` / `details` verbatim — or a synthesized
code when there was no envelope to read. Two decisions make the shape
trustworthy. First, **`code` is the only required wire field**: the
backend sends `{code, params}` only, so requiring anything more would
discard every real module code into the `client.*` fallback; `traceId`
surfaces for correlation when a backend sends one. Second, **the
`client.` prefix is reserved by mechanism, not convention**: server
codes are module-scoped (`authn.*`, `notes.*`) and `client` is no
module's domain, so an envelope whose code starts with `client.` is
refused at parse time and surfaces as the honest
`client.http.<status>` — a misbehaving backend or intermediary cannot
make a session error read as "the request timed out".
`isTransportFailure` answers "did this request never reach a usable
response" through `status` 0 — the one status no HTTP response can
carry — and consumer whitelists should consult it rather than match
codes.

## Design: why the bearer token lives in memory

An access token in `localStorage` is a credential an XSS walks away
with. The token store is a plain two-method seam (`get` / `set`); the
memory implementation is the only one the package ships, and no
storage API exists here at all. Two consequences follow. The token is
re-read before every attempt, so a retry after a refresh carries the
fresh token. And a request can declare `omitAccessToken` to skip the
store entirely even while a token is held — the generated
session-refresh operation is declared exactly that way (see the
[api-sdk](/docs/developer-docs/modules/web/api-sdk/) page).

The refresh token is not this package's business either: the authn API
returns it in the token-issuing response bodies and sets no refresh
cookie, so the session layer (`@speed/auth-core`) holds it in its
closure and drives the refresh operation. `api-client` only defines
the seam: `refreshAccessToken?: () => Promise<boolean>`.

## Design: why the 401 refresh is bearer-only, single-flight and once

When a request that *presented a bearer token* answers 401 and the
hook is configured, the client runs one refresh — concurrent 401s
share a single in-flight refresh promise, so a burst of expired-session
requests triggers exactly one refresh — then retries the original
request exactly once, any method, outside the transient-retry budget.
Refresh failure rejects the original 401 as an `auth: true` `ApiError`
and reports `access token refresh failed`.

The bearer-only rule is load-bearing. A 401 on a credential-less
request means the endpoint demands authentication, which refreshing
cannot provide, so it surfaces untouched. The session's own refresh
request travels credential-less *by declaration*, so a refused refresh
token surfaces instead of re-entering the refresh path and awaiting
itself. The refresh
round is also a separate exchange: `timeoutMs` bounds each HTTP
exchange, never the time spent inside the refresh hook, so a slow
refresh cannot degrade the refused 401's own envelope — its code and
trace id — into a synthetic `client.http.401`.

## Design: why transient retries are conservative

Only idempotent methods (GET/HEAD/OPTIONS) are retried, only on 429
(honouring `Retry-After`, capped), 502/503/504, network failures and
timeouts, with exponential full-jitter delays under the frozen
`DEFAULT_RETRY_POLICY` of 3 attempts / 200 ms / 4 s ceiling. The
transient budget never overlaps the 401-refresh round: the refresh
retry performs no transient retry and never consumes one. Cancellation
is never retried and never wrapped — aborting your `signal` rejects
the raw `AbortError`, so query layers such as TanStack Query keep
standard cancellation semantics. The per-attempt timer covers the
whole exchange, the response body included, so a server that answers
headers and then stalls its body rejects with `client.timeout` instead
of hanging the request on a half-open response. A response discarded
for a retry has its body cancelled first, releasing the connection.

## Stable surface

`createClient(options)` returning the `RequestFn` type; the `ApiError`
class (`status` 0 when no response arrived, `auth` true exactly for
HTTP 401) with the `isApiError` guard and the `isTransportFailure`
predicate; the reserved `ERROR_CODE_NETWORK` / `ERROR_CODE_TIMEOUT` /
`ERROR_CODE_PROTOCOL` constants and `httpErrorCode(status)`; the
`AccessTokenStore` type and `createMemoryAccessTokenStore()`; the
`RetryPolicy` type, the frozen `DEFAULT_RETRY_POLICY` and the pure
`retryDelayMs` / `retryAfterDelayMs` helpers; the `Reporter` seam with
its console-backed default; the two pre-auth config fetchers
(`fetchPublicConfig` / `fetchSystemFeatures` over
`CONFIG_PUBLIC_PATH` / `SYSTEM_FEATURES_PATH`, hand-kept in sync with
`go/config`, which owns no spec fragment for either endpoint); and the
`./react` subpath's `usePublicConfig` / `useFeature` — one fetch shared
per `RequestFn` identity, `useFeature` composing on that same cache and
defaulting to `false` while loading or on error, never throwing.

## Source

- Package contract and decisions: [web/packages/api-client/README.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/README.md), [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md)
- The enforcing rule: [web/eslint-rules/](https://github.com/vislake/speed/tree/main/web/eslint-rules) (`speed/no-direct-http`)
- Design: [docs/internal/12-frontend.md](https://github.com/vislake/speed/blob/main/docs/internal/12-frontend.md) (mechanism notes: token placement, refresh seam, no-direct-http)

## Related pages

- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — the layer survey this page hangs off; "One hand-written HTTP home" is this package's section
- How to use it: [api-client in the user guide](/docs/user-guide/modules/web/api-client/)
- The rest of the web HTTP group: [api-sdk](/docs/developer-docs/modules/web/api-sdk/) — the generated surface that calls through this seam; [auth-core](/docs/developer-docs/modules/web/auth-core/) — the session layer that fills the refresh seam
- The Go side of the two config fetchers: [config](/docs/developer-docs/modules/core/config/)
