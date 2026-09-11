---
title: product-shell
weight: 11
description: "The tenant-facing assembly shell — ProductShell composes the AppShell frame, the auth-ui sign-in family and the auth-core hooks into one three-branch view machine."
---

# product-shell

`@speed/product-shell` is the tenant-facing assembly shell of a
frontend built on speed. It composes the shared app chrome
(`@speed/layout-kit`'s `AppShell`), the sign-in family
([auth-ui](/docs/user-guide/modules/web/auth-ui/)) and the headless
session hooks (`@speed/auth-core`) into one ready-to-copy front door
for a tenant-facing business application. The platform-staff shell of
the same tier is not built.

## What it is for

The package exports `ProductShell` and its props type, plus the
`PRODUCT_SHELL_NAMESPACE` (the literal `'product-shell'`) and
`productShellResources` bundle pair. A second entry point, the
`@speed/product-shell/bootstrap` subpath, exports `bootstrapSpeedApp`:
the app-entry assembly a delivered app's `main` drives from one
declarative definition — it creates the i18n instance (the four
composed packages' namespaces auto-registered, the platform error
bundle named as fallback), attaches the auth-core session over a
memory access-token store, builds the api-client over the
generated-operation runtime binding with the session's refresh leg and the
instance's per-attempt language provider, and mounts the theme and
query-client provider stack around your `view`. The main entry keeps
the component package's dependency floor; the subpath is where the
entry-point-only pieces (`@speed/api-client`, `@speed/api-sdk`,
`@speed/ui-kit`, `@tanstack/react-query`) are imported. `ProductShell`
renders one of
three branches from the authenticated snapshot:

| Snapshot | Branch |
| --- | --- |
| authenticated | the `AppShell` frame (banner + nav drawer + main) around your app `children` |
| anonymous, app never reached | the host's `signIn` slot — pair it with auth-ui's `SignInScreen` |
| anonymous, app was reached | the `sessionEnded` slot, or the default auth-ui `SessionEndedScreen` if none |

That third branch is why the shell exists as a component rather than
as two lines of hooks in every app: a signed-out user who was inside
the app must not fall back to a fresh-visitor sign-in. The shell
remembers that the app was reached, per session, in component state;
the session itself stays in `@speed/auth-core`.

Because the shell is a whole-page switch it also owns the page-swap
accessibility duties: every branch flip moves focus into the branch's
own container (a focusable, non-tab-stop wrapper), and every flip into
the session-ended view announces itself through a `role="status"`
region. That announcement is the shell's **one string of its own** —
the bilingual `announcements.sessionEnded` key of the `product-shell`
namespace; every other string on the frame and the default ended
screen comes from the layout-kit and auth-ui namespaces the host
registers anyway. An unregistered `product-shell` namespace keeps the
focus transfer and renders no raw key text.

## When to choose it

A tenant-facing application that wants the standard door — sign-in,
authenticated frame, session-ended fallback — without re-deriving the
view machine in every app. The shell deliberately does not ship a
default sign-in surface: the `signIn` slot is the host's to supply,
because the channel mix is a product decision. An app with no
accounts at all fits the `AppShell` frame alone better.

## Wiring

Five namespaces, one session attach, one slot to fill:

```tsx
// Register each namespace exactly once on the host's one i18n instance:
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)            // frame chrome
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)    // frame strings
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)          // default ended screen
registerNamespace(i18n, PRODUCT_SHELL_NAMESPACE, productShellResources)
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)    // userMenu switcher
attachSession(session) // the host's @speed/auth-core session, before render

<ProductShell
  navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
  header="My App"
  signIn={<SignInScreen session={session} />}
  userMenu={<UserMenu session={session} />}   // tenant switcher + sign-out,
                                              // host-composed (below)
>
  <MyRoutes />   {/* the authenticated application */}
</ProductShell>
```

The suite compiles and runs this composition — sign in, frame, tenant
switch, sign out, the default session-ended screen, and a return to
the sign-in view — over a real client with every request pinned, so
the quick start cannot drift from the API.

## Core concepts and API essentials

- **The multi-tenant `userMenu`** — the shell has no tenant-switching
  code of its own. The host composes
  [tenancy-ui](/docs/user-guide/modules/web/tenancy-ui/)'s
  `TenantSwitcher` into the `userMenu` slot, feeding its
  `currentTenantId` from auth-core's `useCurrentTenant`, beside
  auth-ui's `SignOutButton`. Picking a tenant drives
  `session.switchTenant(id)`; a switch mints an access token and no
  refresh token, so no refresh leg appears mid-switch. `onSwitched` is
  the host's moment to move tenant-scoped state: auth-core's survival
  rules already dropped the previous tenant's permission lists, so the
  host re-attaches `/me`-derived lists and drops the previous tenant's
  query caches there.
- **Any `AppShell` chrome prop passes straight through** — `navItems`,
  `header`, `headerActions`, `userMenu` and the rest are layout-kit's
  surface; `navItems` must arrive host-computed, `selected` included,
  because nothing here path-matches the URL.
- **The `signIn` slot is required in practice** — without it the
  anonymous-before-the-app branch renders nothing (a blank page), by
  deliberate choice. The default ended screen's action returns the
  viewer to the `signIn` view, so a host without a `signIn` slot whose
  session ends mid-use stays on the ended screen rather than resetting
  into a branch that renders nothing.
- **The `sessionEnded` slot is optional** — with none, auth-ui's
  `SessionEndedScreen` renders; a custom node renders as-is and owns
  its own way back.
- **Don't fight the focus contract** — every branch flip moves focus
  into the branch's own container, and the session-ended flip
  announces itself; content passed in stays inside that boundary.

## Boundaries and pitfalls

- **No permission gating of its own** — the shell never consumes
  layout-kit's `RouteGuard`, never calls `usePermission`, never
  attaches permission lists. Route-level authorization is host
  composition in `children`, fed a status the host derives from the
  lists it attaches (from `/me`) and re-attaches on tenant switch;
  until those lists are attached the hooks fail closed, so gate
  nothing client-side and rely on the server regardless.
- **No tenant switcher of its own, no route matching, no network** —
  the switcher appears only through host composition in `userMenu`;
  navigation selection is host-computed; every request the composed
  views make is a session operation through the host's bound client.
- **No routing, state or query library is required** — `children`
  bring their own.
- **Session state does not survive a page load** — auth-core's
  inherited limitation: reloading starts anonymous, and the shell
  shows the sign-in branch again (the app-reached memory resets with
  the page).

## Related pages

- The frontend layers: [Building the frontend](/docs/user-guide/domains/frontend-building/)
- Related package pages: [auth-ui](/docs/user-guide/modules/web/auth-ui/) (the sign-in family and default ended screen), [tenancy-ui](/docs/user-guide/modules/web/tenancy-ui/) (the `userMenu` switcher)
- Sibling packages with their own pages in this group: `@speed/layout-kit` (the `AppShell` frame and `RouteGuard`), `@speed/auth-core` (the session hooks), `@speed/ui-kit` (theme), `@speed/i18n` (the instance and namespace helpers its exports are typed against), `@speed/api-client` (the client the `./bootstrap` subpath assembles) and `@speed/api-sdk` (the generated operations and the runtime binding `./bootstrap` binds)
