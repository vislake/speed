# AGENTS.md — @speed/api-client

Guidance for AI tooling working in or against this package. The public
surface, semantics and deferred items are documented in the package
`README.md`; this file records the invariants that keep the package
safe to consume.

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
  the session layer above this package (the authn API returns it in the
  token-issuing response bodies, never a cookie or this store).
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
  envelope's code plus its traceId/params/message/details whenever the
  backend sent them (`code` is the only required wire field --
  `parseEnvelope` must never demand `traceId`, which no backend sends);
  everything else synthesizes a reserved `client.*` code
  (`client.network`, `client.timeout`, `client.protocol`,
  `client.http.<status>`). The reservation is a parse-time mechanism,
  not a convention: server codes are module-scoped and `client` is no
  module's domain, so `parseEnvelope` refuses any code starting with
  `client.` -- a backend or intermediary cannot forge an envelope that
  reads as this client's own transport diagnosis. `isTransportFailure`
  is the exported predicate consumers use to distinguish this client's
  genuine transport failures (`status` 0 -- unanswerable by any HTTP
  response) from server-answered errors.
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
  by clearing the store.** `RequestOptions.omitAccessToken` skips the
  store read entirely, so no store token is attached and the request
  carries only its own headers -- without a caller-supplied
  `authorization` that means it travels without an Authorization
  header. The session-refresh operation is generated to carry the
  declaration (orval's `speedRequestCredentialless` mutator in
  @speed/api-sdk). Clearing the store instead would momentarily strip
  the token from concurrent requests that still hold a valid one,
  turning their 401s into spurious auth failures under the bearer-only
  rule above; do not reintroduce a store-clearing refresh wiring. The
  caller's own headers are never stripped by the declaration: a
  caller-supplied `authorization` header is the caller's, and survives.
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
  construct `api` once and reuse the reference. The sharing excludes
  the failure state by design: a load that settles on an error caches
  nothing, so the next subscriber of that `api` starts a fresh fetch
  instead of inheriting the dead state for the client's whole lifetime
  (the failure-recovery shape startup fetches depend on; `refresh()`
  remains the explicit retry lever).

## What ships vs. what is deferred

The runtime above (client, errors, retry, reporter, token store) plus
the `speed/no-direct-http` ESLint rule that routes all other package
HTTP through it.

`fetchPublicConfig` / `fetchSystemFeatures` live in
`src/config-fetcher.ts` -- typed wrappers around go/config's two
pre-auth endpoints (`PathPublic` / `PathSystemFeatures`), built on the
`RequestFn` seam above. Both path constants are hand-kept in sync with
the Go side (go/config ships an OpenAPI fragment declaring the pair as
`config_getPublicConfig` / `config_getSystemFeatures`). The generated
operations that fragment produces -- `@speed/api-sdk`'s
`useConfigGetPublicConfig` / `useConfigGetSystemFeatures` -- are the
primary call surface for those endpoints; these wrappers remain the
per-key mapping layer over them, because the generated type for the
public-config body can only be a record of dynamic keys -- the per-key
typing the hooks expose cannot come from generation. A generated
operation must agree with the two constants here on the paths. Neither
function accepts a tenant argument -- the endpoints carry no tenant on the
wire: which tenant (if any) the request maps to is the server host's own
wiring, through go/config's host-injected request-to-tenant resolver (the
reference app satisfies it with `tenancy.NewDomainResolver` over its host
map), with platform defaults when nothing matches.

`usePublicConfig` / `useFeature` live in `src/react.ts`, exported from
the isolated `./react` subpath. The manifest declares `react` as a
peer and marks it optional (`peerDependenciesMeta.react.optional`) --
npm peers are package-level, not per-subpath, so the optional marker
is what keeps the main entry React-free in practice: a consumer that
only uses the main entry installs no react, while a consumer of the
`./react` subpath supplies it as its own dependency (the package's
own suites resolve it from devDependencies). `src/package.json.test.ts`
pins that metadata shape. Both hooks share one cache keyed by
`RequestFn` identity via `useSyncExternalStore`: the first mounted
consumer of a given `api` starts the one fetch, every other instance
backed by the same `api` reads and re-renders off that shared state,
and `refresh()` republishes a forced refetch to all of them.
`useFeature` composes on `usePublicConfig`'s cache rather than calling
`/api/v1/config/features` itself, and returns `false` (never throws)
while loading or on error. Neither hook does fallback-to-defaults
detection, tenant-switch revalidation, or auto-polling -- see
`src/react.ts`'s header comment for why each is a deliberate
non-feature, not a gap. Two defensive details recorded here so they
are not "simplified" away: both config fetchers refuse an **empty**
2xx body (the RequestFn's own 204-style empty-success shape) as a
coded `client.protocol` error -- go/config always writes a JSON
document, so an empty answer is a broken one, and resolving
`undefined` would pass for the typed document until a consumer reads
a field off it. The refusal is the request's own
(`RequestOptions.requireJsonBody`, client.ts), so it carries the
exchange's real status and attempt count rather than a
wrapper-synthesized pair -- a 503/503/empty-200 exchange surfaces as
attempts 3 / status 200, never the hardcoded 1/0 a fetcher-level
error could only fake -- and `useFeature` null-guards
`data.features` (absent/null reads as "nothing enabled", never a
render-time throw), because the response shape is this
hand-maintained seam and a payload violating it must fail softly.

Deferred with reasons:

- Uploads and SSE transports -- not shipped.
- A browser-page consumer -- a real browser driving the real server.
  The consumer shell (`examples/reference-app/web`, an external
  member of the web workspace, never versioned) already binds one
  real `createClient` over the environment's own fetch into the
  api-sdk seam: `@speed/api-sdk`, the orval-generated typed surface,
  calls into this runtime through its `src/runtime.ts` seam, and
  `@speed/auth-core` compile-consumes both in-workspace (its session
  layer imports this package's `AccessTokenStore` seam, calls the
  generated authn operations through the bound request function, and
  fills the `refreshAccessToken` hook with `() => session.refresh()`).
  The shell's home view reads the server's effective Public values
  and feature flags through `usePublicConfig`/`useFeature` on that
  same bound client, the shape a `requiredFeature`-style consumer
  needs. What is not shipped is browser automation driving the
  server-served page.

## Known limitations

- **No backend sends `traceId`, so an `ApiError` never carries one
  against the real API.** `code` is the envelope's only required
  field; `traceId` is optional (parsed and surfaced when present,
  omitted from reporter attributes when absent), so the `{code,
  params}` bodies every module's error writer actually encodes keep
  their codes end to end. `config-fetcher.test.ts`'s two traceId-less
  fixture tests and `client.test.ts`'s backend-shaped envelope test
  pin the literal wire shape. The correlation half -- a backend-wide
  change for every module's error writer to emit a real `traceId`
  (sourced from request tracing), so user reports can be tied to
  server logs -- is not implemented.

## Public surface

The seventeen runtime exports are pinned by `src/index.test.ts`
(`ApiError`, `CONFIG_PUBLIC_PATH`, `DEFAULT_RETRY_POLICY`,
`ERROR_CODE_NETWORK`, `ERROR_CODE_PROTOCOL`, `ERROR_CODE_TIMEOUT`,
`SYSTEM_FEATURES_PATH`, `createClient`, `createConsoleReporter`,
`createMemoryAccessTokenStore`, `fetchPublicConfig`,
`fetchSystemFeatures`, `httpErrorCode`, `isApiError`,
`isTransportFailure`, `retryAfterDelayMs`, `retryDelayMs`), with
compile-time shape-drift
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
