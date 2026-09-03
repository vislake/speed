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
import { attachSession } from '@speed/auth-core'
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
attachSession(session) // your @speed/auth-core session, already wired to api-client

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
          userMenu={<SignOutButton session={session} />}
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
frame, sign out, session-ended screen, sign in again — over a real
`@speed/api-client`, so the quick start cannot drift from the API.

## Host checklist

- Register the three namespaces once each on your one i18n instance
  (`ui-kit`, `layout-kit`, `auth-ui`) — double registration throws.
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

Peers: `react`, `react-dom`, `@mui/material`, `@emotion/*` — the ambient
MUI/React tree, never duplicated. `layout-kit` and `auth-ui` already pull
their own concrete dependencies; product-shell adds nothing beyond them.
No routing, state or query library is required — your `children` bring their
own.

## What this shell does not do (yet)

- **No permission gating.** Authorization sources are a later round: the
  platform shell's own `RouteGuard` wiring (layout-kit ships the guard; the
  shell does not consume it yet), and the permission lists a shell host
  attaches to auth-core (`setPermissionSet`). Until then, gate nothing
  client-side and rely on the server.
- **No tenant switcher.** The tenant switcher view is a later round; the
  current tenant lives in the attached session (`useCurrentTenant`).
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
the compiled quick start above lives in `src/usage-example.test.tsx`.
