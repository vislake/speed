---
title: tenancy-ui
weight: 10
description: "The tenant-switch affordance — one controlled TenantSwitcher over an auth-core session, rendering the host's current tenant and tenant list and committing switches through the session."
---

# tenancy-ui

`@speed/tenancy-ui` is the tenant-switch affordance of a frontend built
on speed: one controlled component, `TenantSwitcher`, that renders the
host's current tenant as a trigger and the host's tenant list as a
menu, and switches the session to the picked tenant through
`session.switchTenant` — the tenant travels in the switch request
body, never in a header, because tenant context rides inside the
access token.

## What it is for

The package ships `TenantSwitcher` (with `TenantSwitcherProps` and the
`TenantOption` shape) plus the `TENANCY_UI_NAMESPACE` /
`tenancyUiResources` bundle pair. It sits at the same tier as
`@speed/ui-kit` and `@speed/auth-ui` — deliberately auth-aware (it
drives an `@speed/auth-core` session passed in as a prop) but entirely
controlled:

- **It never consumes the auth-core hooks, never reads session state
  beyond the one operation it drives, never attaches or persists a
  session, never navigates, and never touches the network directly** —
  every request is the session's own generated switch operation over
  the host-bound client.
- **The tenant list is host data.** Which tenants the signed-in user
  may switch between is the host's to know (no roster endpoint exists
  in the shipped surface), and the names render verbatim, translated
  by nobody.
- **A successful switch is announced, never silent**: it fires
  `onSwitched` exactly once, after the commit, and announces itself
  through a `role="status"` live region — a context change that
  silently alters which rows a host shows must say so out loud.

Everything after a committed switch is the host's: refetching the new
tenant's data, removing the previous tenant's query cache, re-attaching
`/me`-derived permission lists (a switch commit drops the tenant-domain
permission set by auth-core's own survival rules).

## When to choose it

A tenant-facing application where one signed-in user can belong to
several tenants and must switch between them — typically in the app
chrome, the [product-shell](/docs/user-guide/modules/web/product-shell/)
`userMenu` slot being the natural home. The switcher is never a
standalone surface: it presumes the host's sign-in already ran and the
session holds a current tenant to switch *from*.

## Wiring

```tsx
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))
attachSession(session)
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources) // host chrome

const tenants: TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  // ...the host's own roster
]
// Authenticated branch of the host gate:
//   const currentTenant = useCurrentTenant()
//   <TenantSwitcher session={session} tenants={tenants}
//     currentTenantId={currentTenant?.tenantId ?? null}
//     onSwitched={(tenantId) => { /* the host's post-switch data move */ }} />
```

`useCurrentTenant` reads the committed snapshot, so a successful switch
re-renders the trigger onto the new tenant. The suite compiles and
executes this composition over a real client, pinning the login and
each switch attempt's bearer token and `{ tenant_id }` body, so the
documented usage cannot drift from the API.

## Core concepts and API essentials

`TenantSwitcher` props: `session` (required), `tenants`
(`readonly TenantOption[]`, required), `currentTenantId` (`string |
null`, required — the host's fact, typically from
`useCurrentTenant()`), `onSwitched` (optional; fired exactly once per
committed switch, never for a failed one). Behaviour worth knowing:

- **The trigger shows the current tenant** — the matched `tenants`
  row's name; with no match or a null `currentTenantId` it is disabled
  and shows the `tenantSwitcher.noCurrentTenant` text. The current row
  in the menu is disabled and can never re-trigger a switch.
- **In flight the trigger is inert but focusable** — `aria-disabled`
  plus a refused open handler, never the native `disabled` attribute,
  which would strand the menu-close focus restore on `document.body`.
  One `role="status"` notice names the destination
  (`tenantSwitcher.switchingTo`), then becomes the `switchedTo`
  confirmation in the same live region until the next switch begins.
- **A refused switch** renders the answer's code text in one
  `role="alert"` banner and changes nothing locally: the store keeps
  its token, the trigger stays enabled, the next pick retries.
- **A switch that loses a race on the same session stays lost.** Two
  instances racing `switchTenant` to different tenants both succeed
  server-side; auth-core rejects whichever answer settles after a
  sibling's commit with `OperationSupersededError`. A superseded call
  is a lost race, not a failure: nothing renders, no `onSwitched`
  fires, and the trigger converges through the host's
  `currentTenantId`. Re-issuing the lost request would re-commit a
  tenant the user already abandoned.
- **Error text covers the reachable answers only** — the switch
  endpoint's three answers (`authn.tenant_membership_required`,
  `authn.tenant_membership_unavailable`, the account-status
  `authn.invalid_credentials`), the token-verification answers
  (`authn.authentication_required`, `authn.token_invalid`), the
  session-lifecycle family and the three `client.*` transport codes;
  everything else renders the `errors.unknown` fallback. Where the
  switch answer and the sign-in answer share a meaning, the text is a
  verbatim copy of the auth-ui bundle's — same-tier packages cannot
  import each other's catalogs.

## Boundaries and pitfalls

- **The current tenant is the host's fact** — the component shows
  `currentTenantId`'s row and reports a change; a host that never
  updates the prop after a switch shows a stale trigger. The
  `useCurrentTenant` flow is the intended shape.
- **Permission sets and query caches are the host's to move** —
  `onSwitched` is where the host removes the previous tenant's query
  cache and re-attaches permission lists; nothing here does either.
- **The trigger label and list entries are host text** — built-in
  strings cover only the component's own states.
- **No in-package tenant roster** — the list is host data by contract.
- **Session state does not survive a page load** — auth-core's
  inherited limitation: reloading starts anonymous.

## Source

- Package README:
  [web/packages/tenancy-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/tenancy-ui/README.md) — the authoritative document (props table, behaviour, error whitelist, accessibility, test rig)
- The frontend layers: [Building the frontend](/docs/user-guide/domains/frontend-building/)
- The backend surface: the [authn](/docs/user-guide/modules/identity/authn/) module page (the switch operation lives there); error codes under [authn](/docs/user-guide/error-codes/#authn)
- Related package pages: [product-shell](/docs/user-guide/modules/web/product-shell/) (the `userMenu` home of this component), [auth-ui](/docs/user-guide/modules/web/auth-ui/)
- Sibling packages `@speed/auth-core`, `@speed/i18n` and `@speed/ui-kit` have their own pages in this group
