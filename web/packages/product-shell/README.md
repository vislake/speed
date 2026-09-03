# @speed/product-shell

The tenant-facing assembly shell. Where the platform-facing shell (a later
round) is the same tier for platform staff, this package composes the shared
app chrome (`@speed/layout-kit`'s `AppShell`), the sign-in family
(`@speed/auth-ui`) and the headless session hooks (`@speed/auth-core`) into
one ready-to-copy front door for a tenant-facing business application.

The package ships exactly one export — `ProductShell` — which renders one of
three branches from the authenticated snapshot:

| Snapshot | Branch |
| --- | --- |
| authenticated | The `AppShell` frame (`banner` + nav `drawer` + `main`) around your app `children` |
| anonymous, app never reached | The host's `signIn` slot (pair it with `@speed/auth-ui`'s `SignInScreen`) |
| anonymous, app was reached | The `sessionEnded` slot, or the default `SessionEndedScreen` if none |

That third branch is the reason the shell exists as a component rather than
as two lines of hooks in every app: a signed-out user who was inside the app
must not fall back to a fresh-visitor sign-in. The shell remembers that the
app was reached, per session, in component state; the session itself stays
in `@speed/auth-core`, unattached to any view.

ProductShell renders no text of its own and ships no locale files: every
string on the authenticated frame and the default session-ended screen comes
from the layout-kit and auth-ui namespaces, which the host registers anyway.

## Quick start

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import { LAYOUT_KIT_NAMESPACE, layoutKitResources } from '@speed/layout-kit'
import { AUTH_UI_NAMESPACE, authUiResources, SignInScreen, SignOutButton } from '@speed/auth-ui'
import { TENANCY_UI_NAMESPACE, tenancyUiResources, TenantSwitcher } from '@speed/tenancy-ui'
import { attachSession, useCurrentTenant } from '@speed/auth-core'
import type { AuthSession } from '@speed/auth-core'
import { ProductShell } from '@speed/product-shell'

// Bootstrap, once, before render:
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  // ...your storage / URL / navigator sources
})
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
attachSession(session) // your @speed/auth-core session, already wired to api-client

// The tenants the signed-in user may switch between -- host data (your
// membership source), in host order. The names render as host content.
const tenants = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  { id: 'tenant-2', name: 'Bright Smile Clinic' },
  { id: 'tenant-3', name: 'Harbor View Orthodontics' },
]

// The multi-tenant userMenu (see the section below): the current tenant,
// read through auth-core's useCurrentTenant, feeds tenancy-ui's switcher
// beside auth-ui's sign-out button.
function UserMenu({
  session,
  onSwitched,
}: {
  session: AuthSession
  onSwitched: (tenantId: string) => void
}) {
  const current = useCurrentTenant()
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
      <TenantSwitcher
        session={session}
        tenants={tenants}
        currentTenantId={current?.tenantId ?? null}
        onSwitched={onSwitched}
      />
      <SignOutButton session={session} />
    </Box>
  )
}

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <ProductShell
          navItems={[
            { id: 'home', label: 'Home', href: '/', selected: true },
            // ...every item carries its own selected state (host-computed)
          ]}
          header="My App"
          signIn={<SignInScreen session={session} />}
          userMenu={
            <UserMenu
              session={session}
              onSwitched={(tenantId) => {
                // The switch committed: auth-core's survival rules
                // already dropped the previous tenant's permission
                // lists, so re-attach /me-derived lists for the new
                // tenant and drop the previous tenant's query caches
                // here (see the gated-journey suite for this duty
                // played for real).
              }}
            />
          }
        >
          {/* The authenticated application: routes, pages, content */}
          <MyRoutes />
        </ProductShell>
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

`src/usage-example.test.tsx` compiles and runs this composition — sign in,
frame, tenant switch through the userMenu, sign out, the default
session-ended screen, and a return to the sign-in view — over a real
`@speed/api-client`, pinning every request in order, bodies included, so
the quick start cannot drift from the API.

## Multi-tenant userMenu

ProductShell has no tenant-switching code of its own; switching tenants is a
session operation, and the shell's `userMenu` slot is where the composed
control belongs. The composition shown above is the packaged evidence of it:

- `useCurrentTenant` (auth-core) reads the current tenant from the attached
  session; its value feeds the switcher's `currentTenantId`. Before any
  sign-in the hooks fail closed, the switcher shows the `noCurrentTenant`
  text and is disabled — there is nothing to switch from.
- Picking a row that is not the current tenant drives
  `session.switchTenant(id)`. Success is deliberately quiet: the commit
  flips the principal (the store holds the fresh access token), the host
  observes it through its own hooks, and `onSwitched` fires exactly once,
  afterwards. A refused switch (say, to a tenant the user no longer
  belongs to) renders the answer's code text under the control and changes
  nothing — the switcher stays retryable.
- The switch mints an access token and no refresh token, so no refresh leg
  ever appears mid-switch.
- `onSwitched` is the host's moment to move its own tenant-scoped state.
  Auth-core's survival rules drop the tenant-domain permission lists the
  instant the switch commits, so the host re-attaches both lists (from
  /me) and drops the previous tenant's query caches there. Route-level
  authorization over those lists is likewise host composition — layout-kit
  ships the `RouteGuard` to hang in `children`, fed a status the host
  derives from `usePermission` — and the fixture host of the gated-journey
  suite plays that whole duty (re-attach included) over this shell's
  frame; see Development.

Composing the switcher requires registering tenancy-ui's namespace on the
host's one i18n instance (the fourth registration above), like every other
package whose strings render in the frame.

## Host checklist

- Register the namespaces once each on your one i18n instance — the shell
  trio `ui-kit`, `layout-kit` and `auth-ui`, plus tenancy-ui's own whenever
  the `userMenu` composes the tenant switcher (see above) — double
  registration throws.
- Attach your session once with `attachSession` before render. Before any
  attach, and after logout, the hooks ProductShell reads fail closed to the
  anonymous snapshot, so the shell can only show the sign-in branch.
- Supply the `signIn` slot. With no slot, the anonymous-before-the-app
  branch renders nothing (a blank page); the shell deliberately does not
  ship a default sign-in surface — pairing it with the `@speed/auth-ui`
  family is the host's call, because the channel mix (password, SMS,
  social, registration) is a product decision.
- Any `AppShell` chrome prop (`navItems`, `header`, `headerActions`,
  `userMenu`, ...) is passed straight through; see `@speed/layout-kit`'s
  `AppShell` for the full surface. `navItems` must arrive host-computed,
  `selected` included — nothing here path-matches.
- The `sessionEnded` slot is optional: with no slot, the default
  `@speed/auth-ui` `SessionEndedScreen` renders, whose action returns the
  viewer to the sign-in view. A custom node renders as-is and owns its own
  way back (signing in again flips the snapshot and the frame returns).

## Text and i18n

ProductShell renders zero text of its own — no namespace, no locale files,
no error whitelist. The frame's built-in strings come from the layout-kit
namespace; the default session-ended screen's from the auth-ui namespace;
both are bilingual and host-registered. What you pass as host content
(`navItems` labels, `header`, `children`) is your i18n responsibility, as in
any package.

## Dependencies

| Package | Role |
| --- | --- |
| `@speed/layout-kit` | The authenticated frame: `AppShell` chrome and landmarks |
| `@speed/auth-ui` | The default session-ended screen (`SessionEndedScreen`) |
| `@speed/auth-core` | The hooks that read the attached session's snapshot |

`@speed/tenancy-ui` is deliberately absent: the switcher composition in the
quick start is host work in the `userMenu` slot, so tenancy-ui is a dev-only
companion of the suites (a `devDependency`, aliased to its sources by the
test config), never something this package imports, bundles or depends on.

Peers: `react`, `react-dom`, `@mui/material`, `@emotion/*` — the ambient
MUI/React tree, never duplicated. `layout-kit` and `auth-ui` already pull
their own concrete dependencies; product-shell adds nothing beyond them.
No routing, state or query library is required — your `children` bring their
own.

## What this shell does not do (yet)

- **No permission gating of its own.** The shell never consumes layout-kit's
  `RouteGuard`, never calls `usePermission`, never attaches permission lists
  (`setPermissionSet`) — route-level authorization is host composition in
  your `children`, fed a status you derive from the lists you attach (from
  /me) and re-attach on tenant switch. Until those lists are attached the
  hooks fail closed, so gate nothing client-side and rely on the server
  regardless; the composition itself is packaged evidence of the
  gated-journey suite (see Development), whose fixture host plays the whole
  host duty over this shell's frame.
- **No tenant switcher of its own.** The switcher never appears in this
  package's code; hosts compose tenancy-ui's `TenantSwitcher` into the
  `userMenu` slot (see the multi-tenant section above) and move their own
  tenant-scoped state in `onSwitched`. The quick start's journey and the
  gated-journey suite are the packaged evidence of that composition.
- **No route matching.** Navigation selection is host-computed in
  `navItems`; product-shell never inspects the URL.
- **No network, navigation or session calls.** Every request the composed
  views make is a session operation through your bound api-client.

## Development

From `web/` (the workspace root): `pnpm --filter @speed/product-shell lint`,
`pnpm --filter @speed/product-shell typecheck`, `pnpm --filter
@speed/product-shell test`, `pnpm --filter @speed/product-shell build`, or
the workspace-wide `pnpm -r <cmd>` mirrors. The suite runs in jsdom with
the real-client rig (`test-utils/`), which mirrors the one `@speed/auth-ui`
uses and stays in lockstep with it by hand.

The view machine's unit suite lives in `src/components/ProductShell.test.tsx`;
the compiled quick start above lives in `src/usage-example.test.tsx`, which
renders the four-namespace composition (tenancy-ui's switcher included) and
drives the signed-in journey through a tenant switch and out again. The
permission-gating composition over the same frame is `src/gated-journey.test.tsx`:
its fixture host — a view-id mini-router in `children` whose gates are
layout-kit `RouteGuard`s fed from auth-core's `usePermission` over lists the
host attaches and re-attaches on switch (the README's documented host duty,
played for real) — journeys a switch that drops and reloads the tenant
lists, a denial spell whose refresh keeps the guard settled so `onDenied`
fires exactly once, a refused switch answered by tenancy-ui's error text,
and a server-side session death converging to the session-ended screen.
