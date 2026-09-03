# AGENTS.md — @speed/auth-core

Guidance for AI tooling (and humans) working in this package. It ships
with the package to consuming projects; the discipline here is the
module-boundary rules from the workspace CLAUDE.md applied to one
browser package.

## What this package is

The session lifecycle of a browser client: `createAuthSession(store)`
in `src/session.ts` turns the generated authn operations of
`@speed/api-sdk` into one observable memory-only state machine -- the
session operations (password/SMS login, logout, tenant switch,
step-up, refresh), the social callback login (`completeSocialLogin`)
and the pre-session operations the sign-up and social flows need
(`requestSMSCode`, `register`, `socialAuthorizeUrl`). The pre-session
operations never change session state, on success or failure: an SMS
code request is an acceptance, a built authorize URL has nothing to
commit, and a registration returns the created `AuthnUser` --
registering is not signing in, and the host follows up with a login.
`completeSocialLogin` carries the full login contract, plus one extra
refusal: a sign-in flow answered with the binding-shaped response
(user and identity, no tokens) is a `client.protocol` violation,
rejected before any state change. It is headless: no UI, no DOM. The
React hooks in `src/hooks.ts` (`useAuthState`, `useCurrentTenant`,
`usePermission`, plus the `attachSession` seam that binds them to one
session, last bind wins) live on the same layer -- react is a peer
dependency, never a regular one. The package sits directly above
`@speed/api-client` (the access-token store it receives is the same
store that package reads on every send) and `@speed/api-sdk` (the
generated operations it calls). Never import either of those from
below this layer.

## Rules that are load-bearing here

- **Never hand-write a backend call in this package.** Every request
  goes through a generated operation of `@speed/api-sdk`, which routes
  through the single `bindRequestFn` seam. No `fetch`, no endpoint
  paths as strings, no response types of our own.
- **The session never writes storage, and the refresh token never
  enters the store.** The access token is a memory credential by
  design; the refresh token is closure-only. A future persistence
  round layers on top of this API; it does not reach into it.
- **User operations reject raw and change nothing on failure.** Do not
  wrap, translate or pre-commit. `ApiError` from `@speed/api-client`
  is the one error type a caller must tell apart (`isApiError`), and
  `client.protocol` is reserved for contract-violating 2xx from
  token-issuing endpoints — that check lives in `parseIssued` and
  never moves.
- **`refresh()` is the silent path.** It resolves `false` (never
  rejects) for an invalid/expired/refused refresh token and signs the
  session out locally; it rethrows raw only for transport/server
  failures, with the store and the held tokens untouched.
- **The generation guard is the concurrency invariant.** User
  operations capture the generation at entry and bump it only on
  commit; refresh never bumps, and a stale refresh response applies
  nothing over the winner's state — one adoption excepted: the
  rotated refresh token still lands in the held slot when the winning
  operation kept the held token (a tenant switch or step-up mints no
  new one, and the server has already consumed the held token for
  that refresh). Do not "simplify" this into bump-on-entry or
  compare-free writes — the regression tests around concurrent
  login/refresh/logout pin the current semantics.
- **The permission sets are host-attached data with survival rules,
  never evaluation.** The session only carries the per-domain lists
  (`setPermissionSet`) and applies the rules in its header when a
  principal change commits (a silent refresh or a step-up keeps both
  lists, a tenant switch drops the tenant list and keeps the system
  one, a login — even by the same user in the same tenant — or an
  anonymous transition clears both, a failed operation changes
  nothing). Set membership is decided by the hooks and the shells,
  never here — and never fetched from a server here either: the
  /me-derived lists belong to the host to attach. Because a login and
  a tenant switch clear what must not survive, the host discipline is
  to refetch a domain's lists after those commits instead of reusing
  lists fetched under an earlier principal: the session never
  correlates a list with the principal it was fetched under, so
  attaching a stale list is a host bug it cannot detect.
- **The hooks read the attached session and never drive it.** A
  component that must log in calls `session.loginWithPassword` from an
  event handler; hooks never mutate state, never fetch, and fail
  closed (anonymous snapshot, null tenant, `false` permissions) before
  `attachSession` has been called.
- **Refresh is single-flight per held refresh token.** Presentations
  of the same held token share one in-flight request; a second
  parallel presentation of it reads as token theft to the authn
  server, so the session serialises them itself. A call made after a
  tenant switch or step-up presents the same held token and still
  shares that flight.
- **The refresh request travels credential-less by declaration.** The
  generated mutator's `omitAccessToken` keeps the request free of the
  Authorization header, and `runRefresh` never reads, clears or
  restores the token store — there is nothing to restore on a
  rethrowable failure. Keep it that way: a clear-and-restore ordering
  would re-arm the api-client bearer-only refresh rule against the
  session's own refresh request.
- **Response validation is fail-closed and happens before any state
  change** — even when the operation's generation is stale, a
  contract-violating 2xx still rejects (the caller deserves to know its
  own request failed) before the stale-result drop applies.

## Public surface

`src/index.ts` exports `createAuthSession`, the session types
(`AuthSession`, `AuthSnapshot`, `AuthSessionListener`, plus
`AuthDomain` and `AuthPermissionSets`, the two names that parameterise
the host-attached permission lists) and the hooks (`attachSession`,
`useAuthState`, `useCurrentTenant`, `usePermission`). Anything else
that grows here stays unexported until a consumer proves it needs to be
public.

## Testing

- `src/session.test.ts` (the state machine, plain node environment, no
  DOM) and `src/hooks.test.ts` (the React bindings, per-file jsdom
  pragma, explicit `afterEach(cleanup)` — vitest runs without globals
  here, which disables @testing-library/react's auto-cleanup) are the
  behaviour tests, run with vitest. `src/usage-example.test.tsx`
  compiles and runs the README Quick start's session-and-hooks flow
  through the same scripted harness (the real createClient composition
  is proven in session.test.ts), so the documented usage cannot drift
  from the API. All of them drive sessions through the shared scripted
  harness in `test-utils/session-harness.ts` — tests never touch a
  real server and never mock package internals; they drive the
  exported API and assert observable state (store contents,
  snapshots, notification order, request bodies).
- The failure contract, protocol violations, the generation guard
  (failed login leaves an in-flight refresh intact; logout cannot be
  resurrected; a newer login is never overwritten), single-flight
  refresh, the store-untouched-while-refreshing guarantee and the
  real-client composition
  (silent-401 refresh through `createClient`) each have dedicated
  tests. The hooks' fail-closed reads before attach, the
  tenant/permission selectors over a scripted session, domain
  separation, the permission-set survival rules and the attach
  rebinding semantics have theirs in both files. A change to any of
  those behaviours must come with a test that pins the new behaviour
  and fails on the old.
- No test file may import from another package's `dist/`; the vitest
  aliases map the `@speed/*` specifiers onto sibling sources.

## Known limitations

- No persistence across page loads and no `restore`: reloading starts
  anonymous. Planned for a later round.
- Token-issuing responses are validated structurally (presence and
  types of `access_token`, `refresh_token`, `principal`) — the access
  token's signature and claims are the server's domain, out of scope
  here.
