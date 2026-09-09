---
title: account-ui
weight: 9
description: "The signed-in account-management family — why its reads go through generated react-query hooks rather than session operations, why MFA state is discovered through actions and step-up elevation never outlives one access token, and why recovery codes appear exactly once."
---

# account-ui

`@speed/account-ui` is the account-management family of a frontend
built on speed: the signed-in half of the account story, where the
`@speed/auth-ui` family ends. Four sections compose a host account
page — sessions and devices with per-session and bulk revocation,
sign-in history, social bindings with the callback handler that
completes a binding at the host's callback route, and the
step-up-gated two-factor setup. The
[user-guide account-ui page](/docs/user-guide/modules/web/account-ui/)
covers the how; this page explains the why.

## Responsibility and boundary

The surfaces are sections, not a routed screen: the owning page and
everything outside this package are host content. The boundary follows
the sibling discipline:

- **Reads go through the generated react-query hooks of
  `@speed/api-sdk` over the host's QueryClient**; writes through the
  same seam's generated mutations. Nothing here attaches a session,
  navigates or touches the network directly.
- **The session arrives as a prop exactly where a session operation
  exists that the generated surface cannot express** — the
  authorize-URL request of the add area, the step-up verification of
  the challenge dialog. The two read-only sections take no props:
  their identity comes from the caller's bound client.
- **`@speed/auth-ui` stays unimported.** Same-layer packages never
  import each other: the provider vocabulary is copied, shaped
  identically, and kept in step by the authn spec.
- **The surface can only do what the spec ships** — there is no
  factor-status, disable or change-password operation.

## Why the reads ride the generated-hooks tier

The account surfaces read lists — sessions, history, identities —
that are **cacheable shared state**, invalidated after their own
mutations: exactly the generated-hooks tier of the api-sdk contract,
the deliberate counterpart of auth-ui's zero-hook forms — a sign-in
answer is one-shot, not a cache. The choice explains the rest
of the shape: the host tree gains a `QueryClientProvider`,
`@speed/api-sdk` is a runtime dependency here (not type-only), and
invalidations go through the exported query-key builders, never a
hand-written key.

## Why each section is shaped as it is

**The session list is the server's list.** `SessionsSection` renders
the authn module's answer as-is: the current session marked from the
server's own `is_current`, revoked rows staying listed and greyed. The
current session can never be revoked from the list. Revoking one
other session is low-loss and needs no second confirmation, while
"sign out other devices" is the heavier gesture, behind the
double-confirmed danger dialog whose `revoked_count` answer surfaces
in a `role="status"` notice.

**History renders server vocabulary only through whitelists.** The
newest-twenty window is frozen and deliberately unpaginated: the
section is a read-only account page, not a search tool. Method and
failure-reason values pass
through `t()` only when on the component's known-token lists —
anything unknown renders a generic label, never a raw value — and
session `amr` values render as opaque references, untranslated by
design.

**Unbinding is irreversible; binding is a pure request.** A row-end
unbind sits behind the danger `ConfirmDialog`; a refusal such as
`authn.last_login_method` stays on the page with its code text. The
add area asks the session for each channel's authorization URL —
reported upward through `onAuthorizeUrl`, never a navigation.
`BindingCallbackHandler` completes the flow with a plain generated
call, never a session operation: binding adds an identity to the
caller's own account, it does not sign anyone in. Its effect is keyed
on the `(code, state)` pair so StrictMode starts exactly one exchange,
and the answer shape dispatches the outcome: a binding-shaped answer
invalidates the identities list and fires `onBound` once; a
login-shaped answer means the caller's sign-in had died and the
exchange logged another account in — the handler renders the
signed-elsewhere panel and fires nothing.

**MFA has no state, only actions.** Because the spec ships no
factor-status operation, state is discovered through actions: a gated
action answers 403 `authn.step_up_required` when the caller's token
carries no fresh second-factor proof, and a 200 on set-up opens the
wizard. Whether that 200 is a first setup or a replacement is decided
by the caller's own elevation, never by the status code — so a warm
token still shows the replacement warning before a confirm that would
void the previous codes. The step-up dialog drives
`session.verifyStepUp`: success settles a fresh access token whose
`amr` carries the factor, and the elevation lives only in that token's
lifetime — the dialog never promises verification will not be asked
again. Recovery codes are
served in plaintext exactly once: the show-once panel is their only
appearance, and the only way to see them again is to regenerate. A
code reported as used — a lost race included, spent even though its
elevation never landed — is never re-submittable; a wrong code is a
field-level error and stays retryable. Enrollment is manual-entry by
recorded decision: the wizard shows the secret and provisioning URI
as text, with no QR or clipboard dependency.

## Error answers: the reachable-code whitelist

Every failure resolves to one code and renders through one
`role="alert"` banner (the exception: a wrong MFA code, a field-level
error). The whitelist covers the session-lifecycle, social-binding and
two-factor/step-up families, `authn.rate_limited` and the `client.*`
transport codes; everything else renders the `errors.unknown`
fallback, never a raw key. Codes whose failure context matches the
sign-in surface's reuse the auth-ui bundle's text verbatim.

## The stable surface

Five exported components with small prop tables — the two read-only
sections take none; `SocialBindingsSection` adds the provider list and
`onAuthorizeUrl`; `BindingCallbackHandler` adds the `code`/`state`
pair and `onBound`; `MfaSection` takes the session — plus the
`SocialProvider`/`SocialProviderConfig` copy of the spec's channel
list and the bilingual `ACCOUNT_UI_NAMESPACE`/`accountUiResources`
pair.

## Related pages

- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — the layers and the two data tiers
- [auth-ui design](/docs/developer-docs/modules/web/auth-ui/) — the sign-in half whose continuation this family is; [authn design](/docs/developer-docs/modules/identity/authn/) — the backend surface both drive
- User guide: [account-ui module](/docs/user-guide/modules/web/account-ui/), [authn module](/docs/user-guide/modules/identity/authn/)
