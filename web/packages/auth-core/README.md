# @speed/auth-core

The browser session lifecycle as a headless, memory-only state machine
over the generated authn surface of `@speed/api-sdk`. `createAuthSession`
wires the generated operations -- password and SMS login, logout, tenant
switch, step-up, refresh -- to one observable session with a single
in-memory access-token store as the bridge into `@speed/api-client`:
a host that built its client with this same store and
`refreshAccessToken: () => session.refresh()` gets silent refresh for
free -- an expired-token 401 on any request runs one refresh, and the
retried request carries the fresh token.

No UI and no storage writes. The access token lives in the
caller-supplied store (in memory by design -- see the access-token rules
in the workspace standards); the refresh token lives only inside the
session closure and is never written anywhere; there is no `restore`
(see Known limitations). React hooks (`useAuthState`,
`useCurrentTenant`, `usePermission`) read one session the host attaches
with `attachSession`; react is a peer dependency, so only a host that
already renders React carries it.

## What ships

| File | Exports |
|---|---|
| `session.ts` | `createAuthSession(store)`, `AuthSession`, `AuthSnapshot`, `AuthSessionListener`, `AuthDomain`, `AuthPermissionSets` |
| `hooks.ts` | `attachSession(session)`, `useAuthState()`, `useCurrentTenant()`, `usePermission(domain, permission)` |

`src/index.ts` re-exports these; everything else is internal.

## Quick start

```ts
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession } from '@speed/auth-core'

const accessTokenStore = createMemoryAccessTokenStore()
const session = createAuthSession(accessTokenStore)

bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),
  }),
)

// subscribe to state transitions once, at app bootstrap:
session.subscribe((snapshot) => render(snapshot))

// render paths read the current snapshot:
const snapshot = session.getSnapshot() // { state: 'anonymous', principal: null }

// React hosts bind the hooks to the session once, at bootstrap:
import {
  attachSession,
  useAuthState,
  useCurrentTenant,
  usePermission,
} from '@speed/auth-core'

attachSession(session)

// in a component (all re-render on every session transition):
const snapshot = useAuthState()
const currentTenant = useCurrentTenant() // { tenantId } -- null while anonymous
const canCreateNotes = usePermission('tenant', 'notes:write')
```

Before any session is attached -- and after a logout -- every hook
fails closed: the anonymous snapshot, a null tenant, `false` for every
permission. Attaching another session later rebinds (last bind wins);
the previous session's transitions stop reaching the hooks.

This session-and-hooks flow is compiled and executed by the package
suite (`src/usage-example.test.tsx`), so the documented usage cannot
drift from the API. The suite drives the flow through the scripted
request seam bound with the same `bindRequestFn` a host's real client
uses; the real-client composition itself (`createClient` over a live
fetch, silent-401 refresh included) is exercised in `session.test.ts`.

## Permission checks are set lookup only

`usePermission(domain, permission)` answers "is this string in the
host-attached list for that domain" -- the `tenant` domain for the
permissions the principal holds inside its current tenant, `system`
for platform-staff permissions that are tenant-independent. The host
attaches the lists through the session; nothing here fetches or
evaluates them, and a domain whose list is absent reads `false`.

```ts
session.setPermissionSet('tenant', ['notes:read', 'notes:write']) // after a /me fetch
session.setPermissionSet('system', ['users:manage'])              // platform staff
session.setPermissionSet('tenant', null)                          // clears a domain
```

The session applies the survival rules when a principal change commits:
a silent refresh or a step-up keeps both lists, a tenant switch drops
the tenant list and keeps the system one, and a login -- even by the
same user in the same tenant -- or an anonymous transition clears both.
These checks are a UX affordance, never a security boundary -- the
server authorizes. Refetch a domain's lists after a login or a tenant
switch: the session clears what must not survive, but it never knows
whether a list you attach still describes the principal it was fetched
under.

## The failure contract

- Every user operation -- `loginWithPassword`, `loginWithSMSCode`,
  `logout`, `switchTenant`, `verifyStepUp` -- rejects with the raw
  `ApiError` from the request (tell it apart with `isApiError` from
  `@speed/api-client`), and a failed operation changes nothing: the
  store, the held refresh token and the snapshot are exactly what they
  were before the attempt.
- A token-issuing 2xx that violates the contract (missing tokens or
  principal, malformed fields) is a protocol violation, never a
  successful login: it rejects with an `ApiError` of
  `status: 200` and code `client.protocol`, again changing nothing.
- `refresh()` is the silent path. It resolves `true` when a fresh pair
  was stored, `false` when there is nothing to refresh or the server
  refused the held token (the session is over and signs out locally --
  the server has already terminated the token family). A transport
  failure or server-side error rethrows the raw `ApiError` with the
  held tokens restored in place.
- Concurrent `refresh()` calls under one generation share a single
  in-flight request: the authn server treats parallel refreshes as
  token theft and rotates the whole family, so the session serialises
  them itself.
- A completed `logout` wins over a refresh that resolves after it, and
  a committed login/switch/step-up wins over a stale refresh: an
  issued pair that lost the race is discarded, never applied.

## Known limitations

- **No persistence across page loads.** The session is memory-only and
  there is deliberately no `restore`: reloading the page starts
  anonymous. The authn API returns the refresh token in the login
  response body and sets no refresh cookie, so nothing outside the
  session closure outlives the page -- a reload cannot re-establish
  the session, and the user signs in again. A persistence/restore
  layer is planned for a later round, layered on top of this API.
- **The refresh token is JavaScript-visible in memory.** The authn API
  sets no refresh cookie to hide it in, so the session must hold the
  token its refresh endpoint takes in the request body. The access
  token is likewise readable by any script running in the page; no
  storage API exists here on purpose.
- The two token-issuing operations that mint no refresh token -- tenant
  switch and step-up -- keep rotating the caller's existing one, per
  the authn spec. A `switchTenant` to a tenant the principal has no
  membership in is refused by the server and changes nothing locally.
