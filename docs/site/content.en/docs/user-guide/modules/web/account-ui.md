---
title: account-ui
weight: 9
description: "The signed-in account management family — sessions with per-session and bulk revocation, sign-in history, social bindings with the binding callback, and step-up-gated TOTP and recovery-code setup."
---

# account-ui

`@speed/account-ui` is the account-management component family of a
frontend built on speed: the signed-in half of the account story,
where the [auth-ui](/docs/user-guide/modules/web/auth-ui/) family
ends. Four surfaces compose a host account page — the sessions list
with per-session and bulk revocation, the sign-in history, the social
bindings surface (plus the callback handler that completes a binding
at the host's callback route), and the step-up-gated TOTP /
recovery-code setup — all rendering against the backend
[authn](/docs/user-guide/modules/identity/authn/) module.

## What it is for

The package ships `SessionsSection`, `LoginHistorySection`,
`SocialBindingsSection` (with `SocialProvider` /
`SocialProviderConfig`), `BindingCallbackHandler`, `MfaSection`, and
the `ACCOUNT_UI_NAMESPACE` / `accountUiResources` pair. The surfaces
are deliberately **sections, not a routed screen**: the page that owns
them, the headings above them and every surface outside this package
(profile fields, password settings — none of which the authn spec
ships) are host content.

The tier is one step above auth-ui's, on the generated-hooks side of
the api-sdk contract: **reads go through the `@tanstack/react-query`
hooks generated into `@speed/api-sdk` over the host's QueryClient**,
and writes through the generated mutations over the same
`bindRequestFn` seam — nothing here reads storage, attaches a session,
navigates or touches the network directly. Two sections take the
`@speed/auth-core` session as a prop, each for exactly one session
operation (`SocialBindingsSection` for the add area's authorize-URL
request, `MfaSection` for the step-up challenge's `verifyStepUp`); the
other two take no props at all. Built-in strings render from the
bilingual `account-ui` namespace; the danger dialogs and empty/error
states compose ui-kit's `ConfirmDialog` / `EmptyState`.

## When to choose it

A signed-in account page needs a security section: which sessions are
live and how to revoke them, the sign-in history, the bound social
identities, and two-factor setup. The family presumes the host's
sign-in ran first and the memory store holds a live token; a host that
never signs users in has nothing for it to render.

## Wiring

```tsx
const store = createMemoryAccessTokenStore()   // token planted by sign-in
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 0 } } })
// <QueryClientProvider client={queryClient}> wraps the account page,
// which composes the four sections under the host's own headings.
```

React-query retry and caching policy is the host's own, and so is the
binding turn: the add area reports an authorize URL through
`onAuthorizeUrl`, the host routes the provider redirect to the
callback route the URL's `redirect_uri` names, `BindingCallbackHandler`
completes the exchange there, and its `onBound` navigates back.

## Core concepts and API essentials

- **`SessionsSection`** (no props) — every session the authn module
  holds for the account, the request's own marked with a current
  badge. A session that is neither current nor revoked has a row-end
  sign-out button with no second confirmation; the section-top *sign
  out other devices* sits behind ui-kit's double-confirmed danger
  `ConfirmDialog`, and the server's `revoked_count` answer surfaces in
  a success notice. Every successful revoke invalidates the list
  query.
- **`LoginHistorySection`** (no props) — the newest page of the
  server's login-attempt list, frozen at 20 rows and never paginated;
  method and failure-reason tokens render only when on the
  component's known-token lists.
- **`SocialBindingsSection`** — `session`, `providers`, and
  `onAuthorizeUrl` props. Each bound identity lists its provider and
  email; an unbind action sits behind the danger `ConfirmDialog`, and
  a refused unbind (e.g. `authn.last_login_method`) stays on the page
  with its code text. The add area renders one button per configured
  provider that is not already bound. The provider vocabulary is
  deliberately copied, not imported from auth-ui — same-layer
  packages never import each other; the authn spec is the shared
  source of truth.
- **`BindingCallbackHandler`** — completes a binding at the host's
  callback route through a plain generated call: binding adds an
  identity to the caller's own account, it signs nobody in. A
  binding-shaped answer invalidates the identities list through the
  exported query-key builders and fires `onBound` once; a login-shaped
  answer (the caller's sign-in had died) renders the signed-elsewhere
  panel and fires nothing; a failed exchange stays retryable for the
  same `(code, state)` pair.
- **`MfaSection`** — the `session` prop exists for the challenge
  dialog alone; enroll, confirm and regenerate are plain generated
  mutations. The spec ships no factor-status and no disable operation,
  so the section never declares enabled or disabled — state is
  discovered through actions, every server-gated action answering 403
  `authn.step_up_required` when the access token carries no fresh
  second-factor proof, which opens the step-up dialog driving
  `session.verifyStepUp`: success settles a fresh access token with
  the factor in its `amr` and retries exactly the gated operation. The
  confirm answer's recovery codes open the show-once panel — the only
  place the codes ever appear; nothing caches or re-fetches them.

Every failure path resolves through the reachable-code whitelist —
the session-lifecycle family, the social-binding answers, the MFA and
step-up answers, `authn.rate_limited` and the `client.*` transport
codes — into one `role="alert"` banner, with an `errors.unknown`
fallback that never shows a raw key; codes whose failure context
matches the sign-in surface reuse the auth-ui bundle's text verbatim.

## Boundaries and pitfalls

- **No factor-status, no disable, no sign-in gating** — only the
  spec's actions exist: set up or replace an authenticator, confirm
  it, regenerate recovery codes.
- **Recovery codes appear exactly once**, in plaintext; leaving the
  show-once panel discards them, and the only way to see them again is
  to regenerate. Enrollment is manual-entry — no QR or clipboard
  dependency.
- **The session list is the server's list** — revoked sessions stay
  listed, greyed, and the current session is never revocable from the
  list; signing out of the device in front of you is the host's
  sign-out action.
- **Step-up elevation outlives exactly one access token**, and session
  state does not survive a page load — auth-core's inherited
  limitations, as with every package of the session family.
- **The account page's other halves are host content** — the authn
  spec ships no change-password or profile operations, so no section
  covers them.

## Source

- The frontend layers: [Building the frontend](/docs/user-guide/domains/frontend-building/)
- The backend surface: the [authn](/docs/user-guide/modules/identity/authn/) module page; error codes under [authn](/docs/user-guide/error-codes/#authn)
- Related package pages: [auth-ui](/docs/user-guide/modules/web/auth-ui/), [tenancy-ui](/docs/user-guide/modules/web/tenancy-ui/)
- Sibling packages `@speed/auth-core`, `@speed/ui-kit` and `@speed/api-sdk` have their own pages in this group
