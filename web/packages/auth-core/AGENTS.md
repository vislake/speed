# AGENTS.md — @speed/auth-core

Guidance for AI tooling (and humans) working in this package. It ships
with the package to consuming projects; the discipline here is the
module-boundary rules from the workspace CLAUDE.md applied to one
browser package.

## What this package is

The session lifecycle of a browser client: `createAuthSession(store)`
in `src/session.ts` turns the generated authn operations of
`@speed/api-sdk` (password/SMS login, logout, tenant switch, step-up,
refresh) into one observable memory-only state machine. It is headless:
no UI, no React, no DOM. It sits directly above `@speed/api-client`
(the access-token store it receives is the same store that package
reads on every send) and `@speed/api-sdk` (the generated operations it
calls). Never import either of those from below this layer.

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
  failures, with the held tokens restored.
- **The generation guard is the concurrency invariant.** User
  operations capture the generation at entry and bump it only on
  commit; refresh never bumps and drops every write whose generation
  no longer holds. Do not "simplify" this into bump-on-entry or
  compare-free writes — the regression tests around concurrent
  login/refresh/logout pin the current semantics.
- **Refresh is single-flight per generation.** Concurrent callers
  share one in-flight request; a second parallel refresh reads as
  token theft to the authn server.
- **The refresh request travels credential-less.** `runRefresh` clears
  the store first and restores the previous token on a rethrowable
  failure. That ordering is load-bearing for the api-client bearer-only
  refresh rule; keep it, and keep the restore skip on generation
  change.
- **Response validation is fail-closed and happens before any state
  change** — even when the operation's generation is stale, a
  contract-violating 2xx still rejects (the caller deserves to know its
  own request failed) before the stale-result drop applies.

## Public surface

`src/index.ts` exports exactly `createAuthSession` and the three types
(`AuthSession`, `AuthSnapshot`, `AuthSessionListener`). Anything else
that grows here stays unexported until a consumer proves it needs to be
public.

## Testing

- `src/session.test.ts` is the one test file, run with vitest in a
  plain node environment (no DOM). It scripts the generated operations
  through `bindRequestFn` — tests never touch a real server and never
  mock the session internals; they drive the exported API and assert
  observable state (store contents, snapshots, notification order,
  request bodies).
- The failure contract, protocol violations, the generation guard
  (failed login leaves an in-flight refresh intact; logout cannot be
  resurrected; a newer login is never overwritten), single-flight
  refresh, store-clear/restore and the real-client composition
  (silent-401 refresh through `createClient`) each have dedicated
  tests. A change to any of those behaviours must come with a test
  that pins the new behaviour and fails on the old.
- No test file may import from another package's `dist/`; the vitest
  aliases map the `@speed/*` specifiers onto sibling sources.

## Known limitations

- No persistence across page loads and no `restore`: reloading starts
  anonymous. Planned for a later round.
- No React hooks; consumers bridge `getSnapshot`/`subscribe` (for
  example through `useSyncExternalStore`) themselves in their shells.
- Token-issuing responses are validated structurally (presence and
  types of `access_token`, `refresh_token`, `principal`) — the access
  token's signature and claims are the server's domain, out of scope
  here.
