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
- **Refresh is once per request, single-flight.** A 401 with a
  configured hook triggers one refresh shared by concurrent 401s, then
  one retry of the original request (any method), outside the
  transient-retry budget. Hook failure reports `access token refresh
  failed` and rejects the original 401 as an auth `ApiError`.
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
both endpoints resolve tenant server-side from the request host.

Deferred with reasons:

- `useFeature` / `usePublicConfig` React hooks -- the fetchers they will
  wrap now exist (`fetchPublicConfig` / `fetchSystemFeatures` above);
  the hooks themselves (shared single-flight cache, `refresh()`) land in
  a follow-up block behind an isolated `./react` subpath export, so this
  package's main entry keeps zero runtime dependencies.
- Uploads and SSE transports -- outside this package's scope
  (`docs/internal/21-api-contract.md`).
- A real first consumer -- `@speed/api-sdk` has landed and consumes this
  runtime through its `src/runtime.ts` seam, but both packages are still
  test-consumed only: the reference app's mandatory first-consumer status
  arrives with the M1 consumer shells that import the generated SDK.
- Real `refreshAccessToken` hooks -- M1 authn work (the seam
  `refreshAccessToken?: () => Promise<boolean>` is the contract).

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
start against a stubbed global fetch). Shared helpers live in
`test-utils/` (`fetch-standin.ts` scripted responders, abort-aware the
way real fetch is; `memory-reporter.ts` capture sinks). Tests never
require Docker or a network.
