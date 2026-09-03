# @speed/api-client

The hand-written HTTP runtime of the speed frontend: one `createClient`
call wires injectable fetch, a memory-only access-token store, a silent
single-flight 401 refresh, a timeout, a conservative retry policy and a
structured reporter into a single typed request function. It is the
HTTP layer the generated `@speed/api-sdk` surface (the orval output of
`task api:gen`) calls into; other packages never hand-write HTTP
requests -- the `speed/no-direct-http` ESLint rule enforces that, and
this package is its single whitelist.

The package ships no UI, no i18n resources and no storage API: error
`code`s map to user-facing text in the consuming package's own
catalogs, and the access token lives in memory only (see below).

## Quick start

```ts
import {
  createClient,
  createMemoryAccessTokenStore,
  isApiError,
} from '@speed/api-client'

/** A note as the API returns it. */
interface Note {
  id: string
  title: string
}

/** Loads the current session's notes through the API client. */
export async function loadNotes(): Promise<Note[]> {
  const api = createClient({
    // Scheme + host + optional prefix, or a same-origin '/api/...'
    // path in development. The fetch implementation is injectable
    // (tests pass a deterministic stand-in); when omitted, the
    // environment's global fetch is captured at construction time.
    baseUrl: '/api/v1',
    // The bearer token store starts empty: auth fills it in memory,
    // never in storage (an access token in localStorage is a
    // credential an XSS walks away with). With no token, requests go
    // out without Authorization.
    accessTokenStore: createMemoryAccessTokenStore(),
    // Silent 401 refresh: M1 authn supplies the real hook against the
    // session-refresh endpoint (the refresh token is an httpOnly
    // cookie JavaScript never sees). Until then every 401 rejects an
    // ApiError with auth: true, and hosts route it to sign-in.
    refreshAccessToken: async () => false,
    // Abort requests slower than 10s. Transient retries follow
    // DEFAULT_RETRY_POLICY: idempotent methods only (GET/HEAD/OPTIONS),
    // up to 3 attempts, 200ms doubling backoff capped at 4s.
    timeoutMs: 10_000,
  })

  try {
    return await api<Note[]>('/notes', { query: { page: 1 } })
  } catch (error) {
    if (isApiError(error)) {
      // error.code is the envelope's module code ('notes.…') or a
      // reserved client.* code when the API layer itself failed; map
      // codes to user-facing text through the i18n catalog, never here.
      console.error('loading notes failed', {
        code: error.code,
        trace_id: error.traceId,
      })
    }
    throw error
  }
}
```

## What it does

- **One error type.** Every failed request rejects an `ApiError`.
  Responses carrying the API's envelope (`{ code, traceId, params?,
  message?, details? }` -- the contract in `docs/internal/21-api-contract.md`)
  surface the envelope's `code`/`traceId`/`params`/`details` verbatim.
  Responses without a valid envelope (a proxy error page, a 500 from an
  upstream, a timeout, a dead network) get a synthesized code in the
  reserved `client.` namespace: `client.network`, `client.timeout`,
  `client.protocol` (a 2xx whose body is not JSON), `client.http.<status>`.
  `error.attempts` reports how many HTTP attempts were made.
- **Bearer auth without a storage API.** The token store is a plain
  two-method interface (`get(): string | null`, `set(token: string |
  null): void`); the memory implementation is the only one the package
  ships. The token is re-read before every attempt, so a retry after a
  refresh carries the fresh token. No tenant header exists anywhere:
  tenant context travels inside the access token.
- **Silent 401 refresh, once per request.** When a request answers 401
  and a `refreshAccessToken` hook is configured, the client runs one
  refresh -- concurrent 401s share a single in-flight refresh promise,
  so a burst of expired-session requests triggers exactly one refresh --
  and retries the original request exactly once, any method, outside
  the transient-retry budget. Refresh failure rejects the original 401
  as an auth `ApiError` and reports `access token refresh failed`
  through the reporter. The retry mechanics of the refresh token
  itself (an httpOnly cookie) belong to the M1 authn round; this
  package only defines the seam.
- **Transient retry, conservatively.** Only idempotent methods
  (GET/HEAD/OPTIONS) are retried, only on 429 (honouring `Retry-After`,
  capped at `maxDelayMs`), 502/503/504, network failures and timeouts.
  Delays are exponential full jitter (`retryDelayMs`), and the budget
  defaults to `DEFAULT_RETRY_POLICY` = 3 attempts / 200ms initial /
  4s ceiling. Caller cancellation is never retried and never wrapped:
  aborting your `signal` rejects the raw `AbortError`, so query layers
  (TanStack Query) keep standard cancellation semantics.
- **Structured reporting.** The reporter sink receives a constant
  English message plus snake_case attributes. The default sink writes
  to `console.error`/`console.warn` -- a stopgap until the M1 round
  wires the app-shell diagnostics pipeline; hosts replace it through
  `ClientOptions.reporter`.

## Public surface

| Export | Kind | Purpose |
| --- | --- | --- |
| `createClient(options)` | function | Builds the request function. Construction validates configuration (baseUrl, fetch availability, retry policy, timeout) and fails fast on mistakes. |
| `RequestFn` | type | `<T>(path, options?) => Promise<T>` -- the returned function; call it as `api<T,>(path)` in TSX files. |
| `ApiError` | class | The one error type. `status` (0 when no response arrived), `code`, `traceId?`, `params?`, `details?`, `attempts`, `auth` (true exactly for HTTP 401), `message` (envelope message or an English diagnostic). |
| `isApiError(value)` | function | Type guard; accepts `instanceof` and structurally identical errors (a second copy of the library). |
| `ERROR_CODE_NETWORK` / `ERROR_CODE_TIMEOUT` / `ERROR_CODE_PROTOCOL` | const | Reserved `client.` codes. |
| `httpErrorCode(status)` | function | Shapes a bare non-2xx status into `client.http.<status>`. |
| `FieldError` | type | `{ field, code, params? }` -- one field-level validation failure in `details`. |
| `AccessTokenStore` | type | The sync `get`/`set` seam hosts implement (or use the memory store). |
| `createMemoryAccessTokenStore()` | function | The memory-only implementation. |
| `RetryPolicy` / `DEFAULT_RETRY_POLICY` | type / const | `{ maxAttempts, initialDelayMs, maxDelayMs }`; default `{ 3, 200, 4000 }`. |
| `retryDelayMs(attempt, policy, random?)` | function | Pure full-jitter backoff maths (test-friendly: inject `random`). |
| `retryAfterDelayMs(header, now?)` | function | Pure `Retry-After` parser: delta-seconds or HTTP-date, ms delay or null. |
| `Reporter` / `createConsoleReporter()` | type / function | The diagnostics seam and its console-backed default. |
| `fetchPublicConfig(api, options?)` | function | GETs `CONFIG_PUBLIC_PATH` (go/config's `PathPublic`); resolves `PublicConfigResponse`. |
| `fetchSystemFeatures(api, options?)` | function | GETs `SYSTEM_FEATURES_PATH` (go/config's `PathSystemFeatures`); resolves `SystemFeaturesResponse`. |
| `CONFIG_PUBLIC_PATH` / `SYSTEM_FEATURES_PATH` | const | The two path strings, hand-kept in sync with go/config (no spec fragment exists yet). |
| `PublicConfigResponse` / `SystemFeaturesResponse` / `ConfigFetchOptions` | type | Wire shapes for the two fetchers above; no tenant field anywhere -- both endpoints resolve tenant server-side from the request host. |
| `usePublicConfig(api)` *(`@speed/api-client/react`)* | hook | Fetches once per `api` identity and shares the result -- loading/error/data plus a `refresh()` -- with every other instance backed by the same `api`. See the package `AGENTS.md` for the caching contract; a full quick-start lands with the round's docs block. |
| `useFeature(api, key)` *(`@speed/api-client/react`)* | hook | `boolean`, composed on `usePublicConfig`'s cache -- `false` while loading and on error, never throws. |
| `UsePublicConfigResult` *(`@speed/api-client/react`)* | type | `{ data, error, isLoading, refresh }` -- `usePublicConfig`'s return shape. |

## What is deliberately not here

- **A dedicated Quick start for the hooks** -- `usePublicConfig` /
  `useFeature` have landed (`@speed/api-client/react`, see the Public
  surface table above), but the runnable README example for them --
  mirroring the Quick start above, executed for real by a test the way
  `usage-example.test.ts` does for the main entry -- lands with this
  round's docs block, alongside a real reference-app consumer.
- **Uploads and SSE** -- outside this package's scope
  (docs/internal/21-api-contract.md).
- **A real first consumer** -- `@speed/api-sdk`, the orval-generated
  typed surface, has landed and calls into this package through its
  `src/runtime.ts` seam. Both are still test-consumed only: the reference
  app's mandatory first-consumer status arrives with the M1 consumer
  shells that import the generated SDK.
- **i18n resources** -- error codes map to bilingual text in the
  consuming package's catalogs; nothing here emits user-facing text.

## Development

From `web/packages/api-client/` (the whole web workspace installs and
tests it too):

```sh
pnpm lint
pnpm typecheck
pnpm test
pnpm build
```

Tests never touch a network: every request goes through a scripted
fetch stand-in (`test-utils/fetch-standin.ts`), and the README Quick
start above is executed verbatim by `src/usage-example.test.ts`.
