---
title: "@speed/layout-kit"
weight: 4
description: "The shared app-chrome layer — AppShell (a responsive header, nav drawer and main content region) and RouteGuard (a content gate driven by a host-injected status), both controlled and auth-agnostic at the same tier as @speed/ui-kit."
---

# @speed/layout-kit

Shared app chrome for any project built on speed: `AppShell` — a
responsive header, nav drawer and content region — and `RouteGuard`, a
route/content gate driven entirely by a host-injected status. Both
components are controlled and props-driven, at the same architectural
tier as `@speed/ui-kit`: no business, tenant or auth-mechanism
semantics live here. `RouteGuard` in particular takes its
allow/deny/pending decision as a plain `status` value — never a
callback it invokes, never a concrete authentication or routing
import — so a tenant-facing shell and a platform-staff console share
this exact package while wiring in two different authorization
sources.

## What it is for

- **`AppShell`** — the app frame: a fixed `AppBar` (the
  `header`/banner landmark), a nav `Drawer` (a labelled `nav`
  landmark — permanent at `md` and up, temporary and overlaid below,
  off the ambient theme's own breakpoints, no new one introduced),
  and a `main` content landmark. A visually-hidden skip-to-content
  link is the first focusable element and focuses `main` directly
  (never a fragment navigation, which would rewrite a hash-routed
  host's current route).
- **`RouteGuard`** — gates `children` on a host-computed
  `RouteGuardStatus`: `'allowed'` renders the children, `'pending'`
  renders `pendingFallback` or a default labelled spinner, `'denied'`
  renders `deniedFallback` or ui-kit's `EmptyState
  variant="noPermission"` — the package's one concrete ui-kit
  coupling. The three states
  are mutually exclusive by construction — no separate loading flag
  could disagree with allowed. `onDenied` fires exactly once per
  transition *into* `'denied'` — the seam for a host's router
  redirect or telemetry call, decoupled from render.

Not a router, not an auth gate, not a navigation system. AppShell
carries no path-matching: `navItems` is a `readonly AppShellNavItem[]`
the host computes in full, including which item is `selected`.

## When to choose it

When you need the standard frame — header, collapsible nav drawer,
content region — and a gate between the signed-in surface and what a
caller may see, without coupling the chrome to your router or
authorization source. The session-family packages decide nothing about
the frame, and the frame nothing about sessions. The
`@speed/product-shell` package composes `AppShell` as its
authenticated frame and exercises the `RouteGuard` shape with statuses
its host computes.

## Wiring and minimal use

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
  // The host's real authorization source computes this value —
  // RouteGuard never imports an auth package.
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
```

`ui-kit`'s namespace is required alongside `layout-kit`'s — the
default denied fallback reuses its `emptyState.noPermission.*`
bundle. Registration must run exactly once per i18n instance, at
bootstrap.

## Core API and usage essentials

- **`AppShell` props** — `navItems` (required; each item
  `{ id, label, icon?, href?, onClick?, selected? }` — `href` renders
  a link, `onClick` fires regardless), `header`, `headerActions` and
  `userMenu` are AppBar slots that render exactly the content passed
  in (an omitted slot renders nothing, never a placeholder),
  `children` renders inside the `main` landmark, `sidebarWidth`
  defaults to 280px, and `mobileOpen`/`onMobileOpenChange` is the
  optional controlled pair for the mobile drawer — omit both and
  AppShell manages the toggle itself, the family's one
  interaction-local exception.
- **Narrow-viewport protections are CSS-only** — the mobile drawer's
  paper width is capped to `min(sidebarWidth, 85vw)`, the AppBar rows
  wrap under real overflow pressure, and the offset placeholders that
  keep content clear of the fixed header derive their height from a
  measured header (ResizeObserver). No props changed.
- **`RouteGuard` props** — `status`, `children`, `pendingFallback`,
  `deniedFallback`, `headingLevel` and `onDenied`. `headingLevel`
  (ui-kit's own `h1`..`h6` union, default `'h1'`) reaches only the
  default denied composition: the gate replaces the page content it
  guards, so the fallback is the page's own heading — a host whose
  page heading precedes the gate passes the level that continues the
  page's order (the usage example gates below its own `h1` at
  `headingLevel="h2"`).
- **Built-in strings** come from the `layout-kit` namespace:
  `appShell.skipToContent`, `appShell.navLabel`,
  `appShell.openNav`/`appShell.closeNav` and `routeGuard.pending` —
  the denied fallback carries no text of its own, by design. Hosts
  reword it by registering their own identical-key pair under
  `LAYOUT_KIT_NAMESPACE` — never by editing component text.

## Boundaries and pitfalls

- **No auth, no router, no data.** The package depends on
  `@speed/i18n` and `@speed/ui-kit` only — no `auth-core`,
  `api-client`, `api-sdk` or any concrete authentication or routing
  package anywhere, and no direct dependency on `@speed/tokens`:
  `AppShell` reads breakpoints and z-index through the ambient MUI
  theme the host's `AppThemeProvider` already builds.
- **The status is host-computed.** Do not expect RouteGuard to fetch
  permissions or know a session; derive `status` from your own
  authorization source (a permission fetch, or a query the server
  answers — a refused read fails closed to `denied`).
- Do not pass `href`-less crumbs or items expecting navigation — a
  nav item without `href` is inert except for its `onClick`.
- The mobile drawer is overlaid, not push-in, below `md`: hosts that
  need a different responsive shape compose their own frame.

## Source

- [web/packages/layout-kit/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/layout-kit/AGENTS.md) — package rules and recorded decisions
- Related: the theme and `EmptyState` it builds on ([ui-kit](/docs/user-guide/modules/web/ui-kit/)), the i18n instance both render through ([i18n](/docs/user-guide/modules/web/i18n/)), and the [frontend-building](/docs/user-guide/domains/frontend-building/) domain guide
