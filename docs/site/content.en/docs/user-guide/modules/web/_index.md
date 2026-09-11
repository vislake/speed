---
title: Web packages
weight: 7
description: "The frontend face of a speed-based product — the twelve @speed npm packages in layers: tokens, i18n, ui-kit and layout-kit at the foundation, api-client and api-sdk for HTTP, the auth-core/auth-ui/account-ui/tenancy-ui session family, product-shell for assembly and billing-ui for billing reads."
bookCollapseSection: true
---

# Web packages

These twelve `@speed/*` npm packages are the frontend half of a
speed-based product. Like the Go modules, they are libraries you
compose into your own application — none of them is an app you run —
and they are delivered under the same lockstep versioning: one version
number for the whole platform, Go modules and packages together. The
[module reference](/docs/user-guide/modules/) pages for the Go side
cover the backend; this group covers everything that renders in a
browser.

The packages are layered, and the layer order is the dependency order:

- **Foundation** — `@speed/tokens` (the design-token tree as
  dependency-free data), `@speed/i18n` (the react-i18next wrapper with
  the no-fallback missing-key discipline), `@speed/ui-kit` (the token
  tree mapped onto an MUI v9 theme, plus seven controlled components),
  and `@speed/layout-kit` (the shared app chrome: `AppShell` and
  `RouteGuard`). These four have their own pages in this group:
  [tokens](./tokens/), [i18n](./i18n/), [ui-kit](./ui-kit/) and
  [layout-kit](./layout-kit/).
- **One HTTP client** — `@speed/api-client` is the single home of
  hand-written HTTP: an injectable `fetch`, a memory-only token store,
  silent single-flight 401 refresh, conservative transient retries,
  every failure normalized into one `ApiError`. `@speed/api-sdk` is
  the generated typed surface of the merged API document, calling
  through that client via the one `bindRequestFn` binding. Neither carries
  i18n resources or a tenant header — tenant context travels inside
  the access token.
- **Session and identity** — `@speed/auth-core` is the headless
  session state machine; `@speed/auth-ui` renders the sign-in family;
  `@speed/account-ui` the signed-in account pages; `@speed/tenancy-ui`
  the tenant switcher.
- **Assembly** — `@speed/product-shell` composes the frame, the
  sign-in family and the session hooks into the three-branch view
  machine (signed out, signed in, dead session); `@speed/billing-ui`
  renders the billing-documents read surface over the generated
  billing operations.

Each of the packages has its own page in this group, covering what it
is for, when to choose it, how to wire it, its core API and its
boundaries.

## Two disciplines every package obeys

- **HTTP happens in exactly one place.** `api-client` is the only
  package that performs HTTP of its own; the generated SDK routes
  through its `RequestFn` interface, and component packages never touch the
  network at all — every interaction reports through a callback or a
  session operation. The workspace's `speed/no-direct-http` ESLint
  rule enforces the claim: a direct `fetch`/`XMLHttpRequest`/`axios`
  call in any other package's `src` is an error.
- **User-facing text is bilingual and never inline.** Every package
  that renders text ships one `zh-CN` bundle and one `en-US` bundle
  with identical leaf key sets under its own namespace, registered
  through `@speed/i18n` — registration validates parity before any
  mutation, and a missing key renders as the key itself, never another
  language's text. The workspace's `speed/no-literal-text` ESLint rule
  refuses inline text in package `src`. This is the frontend mirror of
  the backend rule that new text ships with both languages.

The same discipline keeps the packages free of product semantics: no
component fetches or stores data, no package decides a tenant, and no
package below the session layer knows what an access token is.

## Relationship to the other guides

The [frontend-building](/docs/user-guide/domains/frontend-building/)
domain guide tells the same story from the product side — which
layers exist and the four composition steps that assemble a host.
The [identity](/docs/user-guide/modules/identity/) group's Go modules
are the backend these packages talk to through `api-sdk`, and the
reference app's web host (`examples/reference-app/web`) is the
mandatory-first-consumer composition of them all, served by the app
itself under `APP_WEB_DIST`.
