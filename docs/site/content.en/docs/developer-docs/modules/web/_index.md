---
title: Web packages — design
weight: 0
description: "The design guide for the web group — the twelve @speed packages in five layers: what each owns in one sentence, how the frontend consumes the backend's contracts, and the two design threads that run through every package."
bookCollapseSection: true
---

# Web packages — design

The twelve `@speed/*` packages under `web/packages` are the frontend
half of a speed-based product, and they repeat the backend's shape
decision: not one application but libraries under one lockstep version
that a product host composes into its own app. The
[frontend architecture](/docs/developer-docs/frontend-architecture/)
page draws the full dependency graph; this group guide divides the
labour in one sentence each, states how the group relates to the Go
side of the repository, and points at the two design threads every
package page below returns to. The division of labour, in layer order:

- **Foundation, auth-agnostic** — the floor every rendering package
  sits on. `@speed/tokens` — the design-token tree as dependency-free
  pure data, deep-frozen by default, overridable only through a typed
  copy-on-write diff. `@speed/i18n` — one react-i18next instance per
  host, a negotiated start language, and namespaces whose registration
  is validated for coverage and key-set parity before anything lands,
  with cross-language fallback made impossible. `@speed/ui-kit` — the
  theme factory that maps the merged token tree onto an MUI v9 theme,
  plus seven controlled components that render only the state hosts
  give them. `@speed/layout-kit` — the shared app chrome (`AppShell`,
  `RouteGuard`), where the gate's allow/deny/pending decision arrives
  as a host-injected value and the package carries no navigation or
  authentication logic of its own.
- **One HTTP client** — `@speed/api-client`, the single home of
  hand-written HTTP: injectable `fetch`, a memory-only access-token
  store, a silent single-flight 401 refresh, conservative transient
  retries, every failure normalized into one `ApiError`. And
  `@speed/api-sdk`, the generated typed surface of the merged API
  document, which performs no HTTP of its own — every call adapts
  through the one `bindRequestFn` seam onto that client.
- **Session and identity** — `@speed/auth-core`, the memory-only
  session state machine over the generated authn surface;
  `@speed/auth-ui`, the sign-in family; `@speed/account-ui`, the
  signed-in account pages; `@speed/tenancy-ui`, the tenant switcher.
- **Assembly** — `@speed/product-shell`, the three-branch view machine
  (signed out, signed in, dead session) composing the frame, the
  sign-in family and the session hooks.
- **Business read-only** — `@speed/billing-ui`, the billing-documents
  read surface over the generated billing operations — the pattern a
  domain read surface follows.

## How the web group relates to the Go side

The frontend never sees a Go module — it consumes the backend's
contracts, and only in two forms. Most traffic rides the merged OpenAPI
document: the ten platform modules' fragments joined into one spec,
from which `@speed/api-sdk` (platform operations) and each product's
own app-owned SDK (its own operations) are generated — the
[API contract](/docs/developer-docs/api-contract/) page explains the
split and the generation machinery. Where no fragment exists (config's
two pre-auth endpoints), typed hand-written wrappers carry the call.
Package to module: the authn module's operations back the session
family, config's endpoints back `fetchPublicConfig`/`useFeature`,
billing's read operations back `billing-ui` — the [frontend
architecture](/docs/developer-docs/frontend-architecture/) page maps
each surface.

Where no backend contract exists, the Go side's discipline is mirrored
rather than consumed: `go/pkgcore`'s bilingual message catalog — a
module whose language files disagree fails registration — is mirrored
by `@speed/i18n`'s namespace registration with its identical key-set
parity rule, checked by the same CI tool over the raw files. There is
no Go counterpart for the token tree or the theme; those layers are
pure frontend, and their pages say so.

## Two threads that run through the group

- **State flows in, events flow out.** Every component renders only
  the state hosts give it through props and reports changes up through
  callbacks; nothing in package code fetches, stores, navigates or
  decides a tenant. The reason is structural: routing, identity and
  data flow are the host's composition decisions, so package code
  cannot know them — rendering only given state keeps a component
  correct in every host and testable without a server.
- **Text is bilingual, or it is not in the code.** Every rendering
  package ships one `zh-CN` and one `en-US` bundle with identical leaf
  key sets under its own namespace, and the workspace's
  `speed/no-literal-text` ESLint rule refuses inline user-facing text
  in package `src`. The mirror of the backend rule that user-facing
  text never lives in code.

## Reading order

Read the foundation pages bottom-up — the dependency order is
[tokens](/docs/developer-docs/modules/web/tokens/),
[i18n](/docs/developer-docs/modules/web/i18n/),
[ui-kit](/docs/developer-docs/modules/web/ui-kit/),
[layout-kit](/docs/developer-docs/modules/web/layout-kit/) — then the
HTTP, session and assembly pages as they land. The
[user-guide counterpart](/docs/user-guide/modules/web/) shows the same
twelve from the consumer's side.
