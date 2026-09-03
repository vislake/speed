# AGENTS.md — @speed/api-client

Guidance for AI tooling working in or against this package. The public
surface, semantics and deferred items are documented in the package
`README.md`; this file records the invariants that keep the package
safe to consume, mirroring the repo rules in `CLAUDE.md` and
`docs/internal/21-api-contract.md`.

## What this module is

The one place in the web workspace where hand-written HTTP happens: a
typed request function built by `createClient`, with injectable fetch,
a memory-only access-token store, silent single-flight 401 refresh,
timeout, conservative idempotent retry, and a structured reporter. The
generated `@speed/api-sdk` (orval output of `task api:gen`) calls into
this runtime through its `src/runtime.ts` seam; no package other than
this one may issue HTTP requests itself.

## Invariants (code review enforces; do not weaken)

- **No storage API exists in this package.** The access token is a
  bearer credential: it lives in memory (the default
  `createMemoryAccessTokenStore`), is re-read before every attempt, and
  never touches `localStorage`, `sessionStorage`, IndexedDB or cookies.
  Do not add a storage-backed token store; the refresh token's home is
  an httpOnly cookie managed by the M1 authn module, outside this
  package.
- **No tenant header exists.** Tenant context travels inside the access
  token; the API layer derives it from token claims. Never add
  `x-tenant-id` or similar, and never accept a caller-supplied tenant.
- **fetch is injectable and never implicit at call time.** The client
  captures the environment's global `fetch` at construction (throwing a
  clear error when absent) and `ClientOptions.fetch` overrides it.
  Tests inject deterministic stand-ins (`test-utils/fetch-standin.ts`)
  and never touch a network.
- **Every failure rejects an `ApiError`** -- except caller
  cancellation, which rejects the raw `AbortError` (never retried,
  never wrapped). Envelope-bearing non-2xx responses surface the
  envelope's code/traceId/params/details; everything else synthesizes a
  reserved `client.*` code (`client.network`, `client.timeout`,
  `client.protocol`, `client.http.<status>`).
- **Refresh is once per request, single-flight -- and bearer-only.**
  The hook fires only for a refused request that itself presented a
  bearer token; a 401 on a credential-less request means the endpoint
  demands authentication that refreshing cannot provide, so it
  surfaces untouched (never retried, no false `refresh failed`
  warning) -- which is what keeps a session's own refresh request from
  re-entering the refresh path and awaiting itself. A 401 with a
  configured hook triggers one refresh shared by concurrent 401s, then
  one retry of the original request (any method), outside the
  transient-retry budget. Hook failure reports `access token refresh
  failed` and rejects the original 401 as an auth `ApiError`.
- **Credential-less-ness is declared per request, never manufactured
  by clearing the store.** `RequestOptions.omitAccessToken` sends the
  request without an Authorization header and skips the store read
  entirely -- the session-refresh operation is generated to carry it
  (orval's `speedRequestCredentialless` mutator in @speed/api-sdk).
  Clearing the store instead would momentarily strip the token from
  concurrent requests that still hold a valid one, turning their 401s
  into spurious auth failures under the bearer-only rule above; do not
  reintroduce a store-clearing refresh wiring.
- **Retry is idempotent-only and transient-only.** GET/HEAD/OPTIONS on
  429 (honouring Retry-After, capped by the policy) / 502 / 503 / 504 /
  network failure / timeout. Full-jitter backoff via `retryDelayMs`;
  the frozen `DEFAULT_RETRY_POLICY` is the default. The budget and
  timing are pure functions (`retryDelayMs`, `retryAfterDelayMs`) --
  keep them pure.
- **No user-facing text, no i18n resources.** Report messages are
  constant English strings with snake_case attributes; error `code`s
  are data that consuming packages map to bilingual text in their own
  catalogs.
- **The `speed/no-direct-http` rule whitelists this package.** New
  HTTP-touching code belongs here; keep the whitelist single.
- **React exists only behind the `./react` subpath.** `src/react.ts`
  is the one file in this package that may `import` from `react`; the
  main entry (`src/index.ts`) stays dependency-free, mirroring
  `@speed/i18n`'s `./mui-locale` isolation. Do not import `react` from
  any file reachable from the main entry, and do not add a second
  React-touching file outside `react.ts`.
- **The `usePublicConfig`/`useFeature` cache is keyed by `RequestFn`
  identity, not by component lifetime.** A fetch started by the first
  mounted consumer of a given `api` is shared (and, if still in
  flight, awaited-in-place) by every other instance backed by the same
  `api` -- including ones that mount after the fetch settles. Passing a
  fresh `RequestFn` on every render defeats the sharing; hosts must
  construct `api` once and reuse the reference.

## In this round vs. deferred

Landed: the runtime above (client, errors, retry, reporter, token
store) plus the `speed/no-direct-http` ESLint rule that routes all
other package HTTP through it. Also landed (config-web round, B1):
`fetchPublicConfig` / `fetchSystemFeatures` in `src/config-fetcher.ts`
-- typed wrappers around go/config's two pre-auth endpoints
(`PathPublic` / `PathSystemFeatures`), built on the `RequestFn` seam
above. Both path constants are hand-kept in sync with the Go side
(`go/config/AGENTS.md`'s Known limitations: no OpenAPI fragment exists
for these endpoints). Neither function accepts a tenant argument --
both endpoints resolve tenant server-side from the request host. Also
landed (config-web round, B2): `usePublicConfig` / `useFeature` in
`src/react.ts`, exported from the isolated `./react` subpath (`react`
is a required `peerDependency` of that subpath only -- the main entry
stays dependency-free). Both hooks share one cache keyed by `RequestFn`
identity via `useSyncExternalStore`: the first mounted consumer of a
given `api` starts the one fetch, every other instance backed by the
same `api` reads and re-renders off that shared state, and `refresh()`
republishes a forced refetch to all of them. `useFeature` composes on
`usePublicConfig`'s cache rather than calling `/api/system/features`
itself, and returns `false` (never throws) while loading or on error.
Neither hook does fallback-to-defaults detection, tenant-switch
revalidation, or auto-polling -- see `src/react.ts`'s header comment
for why each is a deliberate non-feature, not a gap.

Deferred with reasons:

- Uploads and SSE transports -- outside this package's scope
  (`docs/internal/21-api-contract.md`).
- A real reference-app consumer -- `@speed/api-sdk`, the orval-generated
  typed surface, has landed and calls into this runtime through its
  `src/runtime.ts` seam, and `@speed/auth-core` compile-consumes both
  in-workspace (its session layer imports this package's
  `AccessTokenStore` seam and calls the generated authn operations
  through the bound request function). `usePublicConfig`/`useFeature`
  have landed with their own README quick start (`src/react.ts`,
  `src/react-usage-example.test.ts`). The runtime first consumer is
  still to come: `examples/reference-app` has no frontend shell yet (it
  is backend-only today), so the reference app's mandatory
  first-consumer status arrives with the M1 consumer shells that bind a
  real `createClient` and, for the hooks, with the first shell that
  builds a `NavItem`-style `requiredFeature` consumer per
  `docs/internal/11-cross-cutting.md`.
- Real `refreshAccessToken` hooks -- M1 authn work (the seam
  `refreshAccessToken?: () => Promise<boolean>` is the contract).

## Known limitations

- **go/config's error responses do not carry `traceId`, so their real
  module code never reaches an `ApiError`.** `client.ts`'s
  `parseEnvelope` treats a body as a trustworthy envelope only when
  both `code` and `traceId` are strings (matching the API contract's
  required-fields schema, `docs/internal/21-api-contract.md`); a body
  missing either falls back to a synthetic `client.http.<status>` code.
  go/config's `errorEnvelope` (`go/config/http.go`) only ever encodes
  `{code, params}` -- it has no `traceId` field, and neither does
  `apperr` (no `TraceID` concept exists there), so every genuine
  `fetchPublicConfig` / `fetchSystemFeatures` failure against go/config
  degrades its real `config.*` code to `client.http.<status>` today.
  `config-fetcher.test.ts`'s two "actual go/config error shape" tests
  pin this behavior with a traceId-less mock so it stays visible rather
  than only ever exercised against a fabricated, spec-compliant body.
  This is a pre-existing gap in go/config (and the reference app's
  notes handler, which has the same omission), not something this
  package can fix on its own -- the correct fix is on the Go side
  (either `errorEnvelope` starts emitting a real `traceId`, sourced from
  request tracing, or the contract schema is revisited for hand-kept
  endpoints outside the spec-first flow). Deferred to a future backend
  round; tracked here so it is not mistaken for `client.ts` silently
  losing data it was handed.

## Public surface

The sixteen runtime exports are pinned by `src/index.test.ts`
(`ApiError`, `CONFIG_PUBLIC_PATH`, `DEFAULT_RETRY_POLICY`,
`ERROR_CODE_NETWORK`, `ERROR_CODE_PROTOCOL`, `ERROR_CODE_TIMEOUT`,
`SYSTEM_FEATURES_PATH`, `createClient`, `createConsoleReporter`,
`createMemoryAccessTokenStore`, `fetchPublicConfig`,
`fetchSystemFeatures`, `httpErrorCode`, `isApiError`,
`retryAfterDelayMs`, `retryDelayMs`), with compile-time shape-drift
guards for the type exports (`RequestFn`, `ClientOptions`,
`RequestOptions`, `AccessTokenStore`, `RetryPolicy`, `Reporter`,
`FieldError`, `HttpMethod`, `ApiErrorInit`, `ConfigFetchOptions`,
`PublicConfigResponse`, `SystemFeaturesResponse`). See the README's
public-surface table for semantics. Removing or renaming an export
breaks the pin tests and the typecheck; extend the surface
deliberately, with the README table updated in the same commit.

The `./react` subpath exports `usePublicConfig`, `useFeature` and the
`UsePublicConfigResult` type from `src/react.ts` -- not pinned by
`src/index.test.ts` (that file covers only the main entry); `src/react.test.ts`
exercises both hooks' behavior directly instead.

## Development

From this directory (`web/packages/api-client/`), or workspace-wide
from `web/` with `pnpm -r`:

```sh
pnpm lint        # eslint; the speed/no-direct-http whitelist covers this package
pnpm typecheck   # strict; relative imports in src/ carry .js extensions (nodenext)
pnpm test        # vitest; colocated unit tests plus test-utils/ helpers
pnpm build       # tsc ESM build (nodenext); dist/ is gitignored build output
```

Test layout: one file per source file (`errors.ts` -> `errors.test.ts`)
plus behavior files (`usage-example.test.ts` executes the README Quick
start against a stubbed global fetch; `react-usage-example.test.ts`
does the same for the README's "Config hooks" quick start, via
`renderHook`). Shared helpers live in `test-utils/` (`fetch-standin.ts`
scripted responders, abort-aware the way real fetch is;
`memory-reporter.ts` capture sinks). Tests never require Docker or a
network. `src/react.test.ts` and `src/react-usage-example.test.ts`
opt into the `jsdom` environment via a per-file
`// @vitest-environment jsdom` docblock (vitest 4's built-in mechanism)
rather than a package-wide `vitest.config.ts` -- every other test file
in this package keeps the faster default `node` environment, since
`renderHook`'s DOM mounting is the only thing here that needs one.
