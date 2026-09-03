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
generated `@speed/api-sdk` (orval output of `task api:gen`, a later
round) will call into this runtime; no package other than this one may
issue HTTP requests itself.

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
other package HTTP through it.

Deferred with reasons:

- `useFeature` / `usePublicConfig` hooks and the public-config fetcher
  -- they consume the M1 config endpoints (`docs/internal/12-frontend.md`).
- Uploads and SSE transports -- outside this package's scope
  (`docs/internal/21-api-contract.md`).
- `@speed/api-sdk` -- the orval-generated typed surface is a separate
  package; until it lands, this package is exercised by its own unit
  tests only (the reference app's mandatory first-consumer status
  applies to the generated SDK round).
- Real `refreshAccessToken` hooks -- M1 authn work (the seam
  `refreshAccessToken?: () => Promise<boolean>` is the contract).

## Public surface

The twelve runtime exports are pinned by `src/index.test.ts`
(`ApiError`, `DEFAULT_RETRY_POLICY`, `ERROR_CODE_NETWORK`,
`ERROR_CODE_PROTOCOL`, `ERROR_CODE_TIMEOUT`, `createClient`,
`createConsoleReporter`, `createMemoryAccessTokenStore`,
`httpErrorCode`, `isApiError`, `retryAfterDelayMs`, `retryDelayMs`),
with compile-time shape-drift guards for the type exports
(`RequestFn`, `ClientOptions`, `RequestOptions`, `AccessTokenStore`,
`RetryPolicy`, `Reporter`, `FieldError`, `HttpMethod`, `ApiErrorInit`).
See the README's public-surface table for semantics. Removing or
renaming an export breaks the pin tests and the typecheck; extend the
surface deliberately, with the README table updated in the same
commit.

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
