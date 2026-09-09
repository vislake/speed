---
title: "@speed/auth-core"
weight: 7
description: "The browser session lifecycle as a headless, memory-only state machine over the generated authn surface — access token in the store, refresh token in the session closure, single-flight silent refresh, generation-guarded races, and read-only React hooks."
---

# @speed/auth-core

`@speed/auth-core` is the browser session lifecycle as a headless,
memory-only state machine over the generated authn surface of
[@speed/api-sdk](/docs/user-guide/modules/web/api-sdk/).
`createAuthSession(store)` wires the generated authn operations — the
password and SMS logins, logout, tenant switch, step-up, refresh, and
the SMS-code request, register and social operations that feed the
sign-up and social sign-in flows — into one observable session. The
access token lives in the caller-supplied store; the refresh token
lives only inside the session closure and is never written anywhere. A
host that built its client with this same store and
`refreshAccessToken: () => session.refresh()` gets silent refresh for
free: an expired-token 401 on any request runs one refresh, and the
retried request carries the fresh token.

No UI, no storage writes, and deliberately no `restore`: the authn API
returns the refresh token in the token-issuing response body and sets
no refresh cookie, so nothing outside the session closure outlives the
page — a reload starts anonymous, and the user signs in again.

## When to use it

Any host that signs users in through the authn API. React hosts
additionally attach the session to the read-only hooks, and the
component families above the session — sign-in, account pages, tenant
switcher, product shell — all drive this same contract. The session is
fully usable without React too: `subscribe`/`getSnapshot` are the
whole observable surface, and operations are plain promises. The rules
the session encodes are the server's own (see
[authn](/docs/user-guide/modules/identity/authn/)): parallel
presentations of one refresh token read as theft there, which is why
the session serialises refreshes itself rather than letting callers
race them.

## Installation and wiring

```ts
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession, attachSession } from '@speed/auth-core'

const accessTokenStore = createMemoryAccessTokenStore()
const session = createAuthSession(accessTokenStore)

bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),   // silent refresh, below
  }),
)

// Non-React hosts subscribe once, at bootstrap:
session.subscribe((snapshot) => render(snapshot))

// React hosts bind the hooks to the session once, at bootstrap:
attachSession(session)
```

In a component, `useAuthState()` returns the current snapshot,
`useCurrentTenant()` the current `{ tenantId }` — `null` while
anonymous — and `usePermission('tenant', 'notes:write')` a boolean.
All three re-render on every session transition.

## Core API and usage

- **The observable session.** `getSnapshot()` reads the current state;
  `subscribe(listener)` returns the unsubscribe function. User
  operations (`loginWithPassword`, `loginWithSMSCode`,
  `completeSocialLogin`, `logout`, `switchTenant`, `verifyStepUp`)
  resolve with the new snapshot. The pre-session operations
  (`requestSMSCode`, which always answers 202, `register`,
  `socialAuthorizeUrl`) change nothing even on success — a
  registration is not a login, and the created user goes to the host's
  own follow-up. `socialAuthorizeUrl` is a pure request: the session
  never navigates.
- **The failure contract.** Every operation rejects with the raw
  `ApiError` (tell it apart with `isApiError` from
  [@speed/api-client](/docs/user-guide/modules/web/api-client/)), and a
  failed operation changes nothing: store, held refresh token and
  snapshot stay exactly as they were. When a concurrent sibling
  operation committed while this request was in flight, the request's
  own 2xx no longer describes the session — nothing of it is applied,
  and the operation rejects with `OperationSupersededError`
  (distinguish with `isOperationSuperseded`), whose `snapshot` field
  carries the winner's state. A token-issuing 2xx that violates the
  contract (missing tokens or principal) rejects with `client.protocol`
  before any state change.
- **`refresh()` is the silent path.** It resolves `true` when a fresh
  pair was stored; `false` when there is nothing to refresh, when the
  server refused the held token (the session signs out locally, the
  server having already terminated the token family), or when a tenant
  switch won the race while the refresh was in flight. A transport
  failure or server-side error rethrows the raw `ApiError` with store
  and held tokens untouched. Concurrent `refresh()` calls presenting
  the same held refresh token share one in-flight request, and a call
  after a switch or step-up still presents the held token, so it
  shares the flight too.
- **Generation guard.** A completed logout wins over a refresh that
  resolves after it, and a committed login, switch or step-up wins over
  a stale refresh: the loser's access token and snapshot are never
  applied over the winner's — the one exception being the rotated
  refresh token itself, adopted when the winning operation kept the
  held token. The resolution reflects who won: a step-up kept the
  principal, so the refresh resolves `true`; a tenant switch changed
  it, so the refresh resolves `false` and the request whose 401 started
  it fails rather than replaying under a tenant it never asked for.
- **The hooks are read-only.** `attachSession` binds one session (last
  bind wins; the previous session's transitions stop reaching the
  hooks). Every hook fails closed before attach and after logout: the
  anonymous snapshot, a `null` tenant, `false` for every permission.
  Login and logout happen in event handlers, never in effects.
- **Permission checks are set lookup only.** The host attaches lists
  per domain — `session.setPermissionSet('tenant', [...])` and
  `('system', [...])`, `null` to clear — and `usePermission(domain,
  permission)` answers "is this string in that list". Nothing here
  fetches or evaluates permissions; a domain whose list is absent
  reads `false`. Survival rules apply when a principal change commits:
  silent refresh and step-up keep both lists, a tenant switch drops
  the tenant list and keeps the system one, a login or logout clears
  both. These checks are a UX affordance, never a security boundary —
  the server authorizes; refetch a domain's lists after a login or
  tenant switch.

## Boundaries and notes

- **No persistence across page loads** — memory-only, deliberately no
  `restore`. A reload starts anonymous.
- **The refresh token is JavaScript-visible in memory.** The authn API
  sets no refresh cookie to hide it in, so the session must hold the
  token its refresh endpoint takes in the request body. No storage API
  exists here on purpose.
- **Binding flows do not live on this surface.** The authn callback
  endpoint doubles as a binding answer for an already-authenticated
  caller, and `completeSocialLogin` deliberately refuses that
  binding-shaped response with `client.protocol`: this session's
  callback surface is a sign-in surface.
- Tenant switch and step-up mint no new refresh token — they rotate
  the caller's existing one, per the authn spec; a `switchTenant` to a
  tenant the principal holds no membership in is refused server-side
  and changes nothing locally.

## Source

- [auth-core README](https://github.com/vislake/speed/blob/main/web/packages/auth-core/README.md) —
  the session contract, the failure contract and the known
  limitations.
- [auth-core AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-core/AGENTS.md) —
  the package's authoritative contract.
