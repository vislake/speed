---
title: auth-ui
weight: 8
description: "The sign-in component family — SignInScreen with its password, SMS-code and registration channels, social sign-in, the sign-out action and the session-ended placeholder, all controlled components over an auth-core session."
---

# auth-ui

`@speed/auth-ui` is the sign-in component family of a frontend built
on speed: the password, SMS-code and registration channels, the social
sign-in section and the callback handler that completes its exchange,
the sign-out action and the session-ended placeholder — assembled in
`SignInScreen` behind a channel tab strip. It is the frontend face of
the [authn](/docs/user-guide/modules/identity/authn/) module's sign-in
surface: every component is **controlled**, driving an
`@speed/auth-core` session the host passes in as a prop.

## What it is for

The package renders the sign-in door of a tenant-facing application:
`SignInScreen` (plus `SignInChannel` and the `SocialSignInOptions`
shape), the channel forms `PasswordSignInForm`, `SMSSignInForm` and
`RegisterForm`, the social block (`SocialSignInSection`,
`SocialCallbackHandler` and the `SocialProvider`/
`SocialProviderConfig` types), `SignOutButton`, the pure
`SessionEndedScreen`, and the `AUTH_UI_NAMESPACE` / `authUiResources`
pair. The defining contract:

- **Nothing here consumes the auth-core hooks, reads or persists
  session state, navigates, or touches the network directly.** Every
  request is a session operation over the client the host bound into
  the shared seam, and a successful sign-in fires `onSignedIn` exactly
  once — everything that happens next is the host's, and host
  callbacks run only after the operation settled.
- **A failed submit changes nothing on the session** and renders one
  whole-attempt `role="alert"` banner resolving the error code through
  the reachable-code whitelist; anything outside it renders the
  `errors.unknown` fallback, so a raw key never appears.
- **Not shipped here**: registration that signs the user in (`register`
  is not a session operation), account binding and step-up (those live
  in [account-ui](/docs/user-guide/modules/web/account-ui/)), and
  channel discovery — the family renders exactly the channels the host
  declares.

## When to choose it

Any tenant-facing frontend whose users sign in with accounts. The
family presumes the host already wired the session layer
(`@speed/auth-core` over one `@speed/api-client`) — it is the reason
the host gate exists, not a consumer of it. The host owns the product
decisions the package cannot: the channel mix (never offer SMS
sign-in when nothing can deliver a code), the page above the form,
and where a successful sign-in leads.

## Wiring

```tsx
// One session and client over one memory access-token store; the
// silent-refresh leg is the session's own refresh().
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl, // the host's fetch implementation
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))

// The bilingual instance; both namespaces registered exactly once
// (ui-kit's, because FormField's validation text lives there).
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
attachSession(session) // the hooks read the attached session

// The host gate: authenticated → the app; an anonymous snapshot at a
// view that held an authenticated one → <SessionEndedScreen />; before
// the first authentication → <SignInScreen session={session} />.
```

The middle gate branch needs one piece of host memory — that the app
was reached — kept in component state, exactly as the
[product-shell](/docs/user-guide/modules/web/product-shell/) view
machine packages it.

## Core concepts and API essentials

- **`SignInScreen`** — `session` (required), `channels`
  (`readonly ('password' | 'sms')[]`; both by default), `social`,
  `defaultChannel`, `onSignedIn`. Switching channels unmounts the
  previous form, so its half-typed state and whole-attempt error reset
  with it; one declared channel renders its form with no tab strip at
  all, and the screen renders no heading of its own.
- **`PasswordSignInForm`** — one identifier field (email or phone;
  the backend decides which) plus the password, over
  `session.loginWithPassword`.
- **`SMSSignInForm`** — a two-step flow: the phone step requests a
  code (`session.requestSMSCode`), whose 202 acceptance is the step's
  terminal state; the code step signs in via `session.loginWithSMSCode`.
  A fresh code starts the code field empty, so a stale code never
  rides along. A submit that loses a concurrent sign-in race is
  answered with `OperationSupersededError` — no banner, no
  `onSignedIn` — and the channel renders the used-code notice, because
  the losing submit spent its single-use code.
- **`RegisterForm`** — registration never signs in: the `'@'`
  heuristic splits the one identifier field into the spec's separated
  email/phone slots, the locale is read at submit time, and the
  created user goes to `onRegistered` (typed with the generated
  `AuthnUser`) or, without a callback, a success panel.
- **Social sign-in** — `SocialSignInSection` renders one outlined
  button per configured provider; clicking asks the session for that
  channel's authorization URL, a pure request reported upward through
  `onAuthorizeUrl`, never a navigation. `SocialCallbackHandler`
  completes the exchange at the host's callback route, its effect
  keyed on the `(code, state)` pair so StrictMode starts exactly one
  exchange; a failed exchange stays retryable for the same pair.
- **`SignOutButton`** — drives `session.logout()`; a failed logout
  renders the answer's code text and stays retryable, a successful one
  is deliberately quiet (the host's hooks observe the flip).
- **`SessionEndedScreen`** — pure presentation, no session prop: the
  placeholder for a view whose authenticated snapshot just turned
  anonymous. It renders ui-kit's `EmptyState` in the `noPermission`
  variant with every text slot overridden from this package's
  namespace; `headingLevel` defaults to `h1`, the level that continues
  nothing.

## Boundaries and pitfalls

- **A registration is not a login** — `RegisterForm` never signs in,
  by spec and by auth-core's contract; the new account signs in
  through the sign-in surface afterwards.
- **Session state does not survive a page load** — auth-core's
  inherited limitation: the refresh token lives only in the session
  closure, so a reload starts anonymous.
- **No channel discovery** — the server answers no "which channels may
  this tenant use" question; the host composes the mix, and
  `SignInScreen` without `social` renders no social block.
- **Account binding and step-up are not here** — an
  already-authenticated caller linking a channel or performing a
  step-up-gated action belongs to
  [account-ui](/docs/user-guide/modules/web/account-ui/); enterprise
  SSO discovery is per-tenant server configuration and no section
  renders it.
- **Components signal nothing into the host's router** — the gate
  observes the snapshot and decides.

## Source

- Package README:
  [web/packages/auth-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/auth-ui/README.md) — the authoritative document (exports, props tables, error whitelist, accessibility, test rig)
- The frontend layers: [Building the frontend](/docs/user-guide/domains/frontend-building/)
- The backend surface: the [authn](/docs/user-guide/modules/identity/authn/) module page; error codes under [authn](/docs/user-guide/error-codes/#authn)
- Related package pages: [account-ui](/docs/user-guide/modules/web/account-ui/), [product-shell](/docs/user-guide/modules/web/product-shell/)
- Sibling packages `@speed/auth-core`, `@speed/ui-kit` and `@speed/layout-kit` have their own pages in this group
