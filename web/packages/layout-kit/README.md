# @speed/layout-kit

Shared app chrome for any project built on speed: `AppShell` (a
responsive header, nav drawer and content region) and `RouteGuard` (a
route/content gate driven entirely by a host-injected status). Both
components are controlled and props-driven, at the same architectural
tier as `@speed/ui-kit` -- no business, tenant, or auth-mechanism
semantics live here. `RouteGuard` in particular takes its
allow/deny/pending decision as a plain `status` value, never a
callback it invokes and never a concrete authentication or routing
import, so `product-shell` (shipped -- this package's first consumer)
and `admin-shell` (a later round) share this exact package while
wiring in two different authorization sources. Every built-in string renders from the
bilingual `layout-kit` namespace registered through `@speed/i18n`, the
same discipline `ui-kit` established, and the workspace's
`no-literal-text` ESLint rule refuses inline text in this package's
`src`.

## What ships

| Module | Exports |
|---|---|
| `components/AppShell.tsx` | `AppShell`, `AppShellProps`, `AppShellNavItem` |
| `components/RouteGuard.tsx` | `RouteGuard`, `RouteGuardProps`, `RouteGuardStatus` |
| `resources.ts` | `LAYOUT_KIT_NAMESPACE`, `layoutKitResources` |

Everything else (`src/internal/`) is shared component plumbing and is
deliberately not exported.

## Quick start

A host registers the `layout-kit` namespace once, alongside `ui-kit`'s
(reused by `RouteGuard`'s default denied fallback), inside the same
provider tree `ui-kit`'s own README documents:

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  AppShell,
  LAYOUT_KIT_NAMESPACE,
  layoutKitResources,
  RouteGuard,
  type RouteGuardStatus,
} from '@speed/layout-kit'
import { useState } from 'react'

const i18n = createI18n()
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)

function AppContent() {
  // Stands in for a real authorization source -- no auth-core round
  // exists yet, so a host wires this to whatever it uses today (a
  // hook backed by a real package, a stub like this one in a test).
  // RouteGuard needs nothing more than the resulting status value.
  const [status] = useState<RouteGuardStatus>('allowed')
  return (
    <AppShell
      navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
      header="My App"
    >
      <RouteGuard status={status}>{/* protected screen content */}</RouteGuard>
    </AppShell>
  )
}

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <AppContent />
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` validates before it mutates (identical leaf key
sets across both languages) and must run exactly once per instance,
same as every other speed namespace. This exact composition is
compiled and executed by the package suite
(`src/usage-example.test.tsx`), so the documented usage cannot drift
from the API.

## AppShell

The responsive app-chrome shell: a fixed `AppBar` (the `header`/banner
landmark), a nav `Drawer` (a labelled `nav` landmark: permanent at the
`md` breakpoint and up, temporary and overlaid below it -- driven off
`useMediaQuery(theme.breakpoints.up('md'))`, no new breakpoint
introduced), and a `main` content landmark. A visually-hidden
skip-to-content link is the first focusable element in the shell.

AppShell carries no navigation logic of its own: `navItems` is a
`readonly AppShellNavItem[]` the host computes in full, including
which item is `selected` -- AppShell never does path-matching, since
different hosts use different routers.

| Prop | Type | Notes |
|---|---|---|
| `navItems` | `readonly AppShellNavItem[]` (required) | each item: `{ id, label, icon?, href?, onClick?, selected? }`; `href` renders a link, `onClick` fires regardless |
| `header` | `ReactNode` | start-of-AppBar content (logo, product name) |
| `headerActions` | `ReactNode` | end-of-AppBar content (search, notifications) |
| `userMenu` | `ReactNode` | far end-of-AppBar content (account menu trigger) |
| `children` | `ReactNode` (required) | the content region, rendered inside the `main` landmark |
| `mobileOpen` / `onMobileOpenChange` | `boolean` / `(open: boolean) => void` | optional controlled pair for the mobile drawer; omit both to let AppShell manage the toggle itself (the one interaction-local exception, matching `ui-kit`'s `ConfirmDialog` double-confirm arm) |
| `sidebarWidth` | `number` | drawer width in px, both variants; defaults to 280 |
| `sx` | `SxProps<Theme>` | escape hatch, merged onto the root layout box |

Slots render exactly the content passed in -- an omitted slot renders
nothing, never a placeholder.

## RouteGuard

Gates `children` on a host-computed `status`, never a callback
RouteGuard invokes:

```ts
export type RouteGuardStatus = 'allowed' | 'denied' | 'pending'

export interface RouteGuardProps {
  readonly status: RouteGuardStatus
  readonly children?: ReactNode
  readonly pendingFallback?: ReactNode
  readonly deniedFallback?: ReactNode
  readonly onDenied?: () => void
}
```

| `status` | Renders |
|---|---|
| `'allowed'` | `children` |
| `'pending'` | `pendingFallback`, or a default centered, labelled MUI `CircularProgress` |
| `'denied'` | `deniedFallback`, or `@speed/ui-kit`'s own `EmptyState variant="noPermission"` -- the package's one concrete `ui-kit` coupling, never anything auth- or routing-shaped |

`onDenied` fires exactly once per transition *into* `'denied'` (a
`useEffect` keyed on `status`, guarded so a re-render that leaves
`status` at `'denied'` does not re-fire) -- the seam for a host's
router redirect or telemetry call, fully decoupled from render.

The three states are mutually exclusive by construction: there is no
separate boolean "loading" flag that could disagree with "allowed".

## Text and i18n

| Key | Purpose |
|---|---|
| `appShell.skipToContent` | the visually-hidden skip-to-content link |
| `appShell.navLabel` | the nav drawer landmark's accessible name |
| `appShell.openNav` / `appShell.closeNav` | the mobile nav toggle button's `aria-label`, by state |
| `routeGuard.pending` | the default pending spinner's `aria-label` |

`routeGuard`'s denied fallback carries no text of its own -- it is
entirely `ui-kit`'s `emptyState.noPermission.*` bundle, by design (see
RouteGuard above). The two files (`src/locales/zh-CN.json`,
`en-US.json`) carry identical leaf key sets, enforced by registration
and by `tools/check_i18n_keys.py` in CI. Hosts reword the kit by
registering their own identical-key bundle pair under
`LAYOUT_KIT_NAMESPACE` at bootstrap -- never by editing component
text.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree |
| `@mui/material` | peer (required, ^9) | `AppBar`/`Drawer`/`CircularProgress` and the ambient theme (breakpoints, z-index) AppShell reads through `useTheme()` |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements |
| `@speed/i18n` | dependency | the namespace registration and translation hook every component renders through |
| `@speed/ui-kit` | dependency | `RouteGuard`'s default denied fallback (`EmptyState variant="noPermission"`) -- the package's only concrete coupling, sanctioned for chrome/primitives |

No direct dependency on `@speed/tokens`: `AppShell` reads
`breakpoints.values` and `zIndex.values` through the ambient MUI theme
a host's `AppThemeProvider` already builds, exactly as `ui-kit`'s own
components do -- both are already MUI-identical defaults, so nothing
layout-relevant is missing (`tokens/src/breakpoints.ts`,
`z-index.ts`). No dependency on `auth-core`, `api-client`, `api-sdk`,
or any concrete authentication or routing package, anywhere in this
package -- see the AGENTS.md non-negotiable rules.

## Deferrals and recorded decisions

- **`auth-core` wiring**: `RouteGuard`'s `status` is fed by a local
  `useState` stub in the Quick start above -- no real authorization
  source exists yet (a later, undispatched round). The prop shape
  needs no change once one lands: that round only computes a
  `RouteGuardStatus` and passes it in.
- **`admin-shell`**: out of scope here; this package ships only the
  shared chrome the platform-staff shell will later assemble. The
  tenant-facing half of that row is closed: `product-shell`, this
  package's first consumer, composes `AppShell` as its authenticated
  frame, and its gated-journey suite proves the `RouteGuard`
  composition shape -- a stand-in host mounting the gate inside the
  shell's children and feeding it a status it computes from the
  role lists it attached, exactly the host-injected-status contract
  this package's rules require.
- **Reference-app consumer**: the reference app's consumer shell
  (`examples/reference-app/web`) composes both exports as host
  composition -- product-shell's `ProductShell` renders `AppShell` as
  its authenticated frame, and the shell's notes view mounts
  `RouteGuard` with a status the host derives from the served
  notes-list query (the server's rbac layer answers a caller without
  `notes:read` with 403, failing the gate closed to `denied`) -- the
  exact host-injected shape this package's rules require; the
  package-level proof (`src/usage-example.test.tsx`) remains the
  in-form leg, and the browser page leg is M4's html-runner/e2e work.
- **Storybook / browser-side visual verification**: no preview-harness
  round exists yet, same deferral `ui-kit` carries; `color-contrast`
  stays axe-disabled for the same jsdom reason.
- **Skip-to-content beyond a single visible-on-focus link**: no
  broader "landmark navigation menu" affordance is in scope -- a
  `product-shell`-level concern once real pages exist.

## Development

From `web/packages/layout-kit`: `pnpm lint`, `pnpm typecheck`, `pnpm
test`, `pnpm build`. The test suite runs in jsdom
(`vitest.config.ts`); shared helpers live in `test-utils/`
(`renderWithProviders` mounts the unit under the real host tree --
`I18nextProvider` plus `ui-kit`'s own `AppThemeProvider`, both
namespaces registered -- and `expectNoAxeViolations` runs axe;
`mockMatchMedia` stubs jsdom's missing `window.matchMedia` for
`AppShell`'s desktop/mobile split). Bilingual fixtures are the shipped
locale files under `src/locales/`, imported by sources and tests;
`src/usage-example.test.tsx` compiles and executes the Quick start
composition above, so the documented usage cannot drift from the API.
