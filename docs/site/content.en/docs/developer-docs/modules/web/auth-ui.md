---
title: auth-ui
weight: 8
description: "The sign-in component family over an auth-core session — why every component is controlled with the session passed as a prop, channel switching resets by unmounting, registration never signs in, the social callback survives StrictMode, and error answers resolve through a reachable-code whitelist."
---

# auth-ui

`@speed/auth-ui` is the sign-in family of a frontend built on speed:
the password, SMS-code and registration channels, the social sign-in
section and the callback handler that completes its exchange, the
sign-out action and the session-ended placeholder — assembled in
`SignInScreen` behind a channel tab strip. It is the frontend face of
the [authn module](/docs/developer-docs/modules/identity/authn/)'s
sign-in surface over the headless session of `@speed/auth-core`. This
page explains why the family is shaped as it is; the
[user-guide auth-ui page](/docs/user-guide/modules/web/auth-ui/) covers
the how.

## Responsibility and boundary

Every piece is a controlled component over an `@speed/auth-core`
session the host passes in as a prop, and the boundary is the same on
every side:

- **Nothing here consumes the auth-core hooks, attaches or reads
  session state, navigates, or touches the network.** Every request is
  a session operation over the host-bound client. A component that
  consumed the hooks would need the host's `attachSession` anyway —
  this family is the reason the host gate exists, not a consumer of
  it.
- **A successful sign-in fires the host's `onSignedIn` exactly once;
  everything that happens next is the host's.** Host callbacks run
  only after the operation's verdict settled, and a throwing callback
  is contained — it cannot re-classify the outcome, because the
  commit already happened.
- **The family renders exactly the channels the host composes.** No
  channel-discovery endpoint exists to query; page chrome — branding,
  the heading, the register link — is host content.
- **Registration is not login, and binding is not this family's.**
  `register` is not a session operation, so `RegisterForm` hands the
  created user to the host's `onRegistered` or shows a success panel.
  Linking channels to a signed-in account and step-up-gated two-factor
  live in `@speed/account-ui`; enterprise SSO is per-tenant
  configuration with no section anywhere. A page reload starts
  anonymous — session state is memory-only by auth-core's contract,
  observable through the snapshot, never commanded.

## Why the pieces are shaped as they are

**The tab strip resets by unmounting.** Switching channels unmounts
the previous form, so its half-typed state and whole-attempt error are
gone with it — channel errors must not leak across surfaces. The
channels offered are host-declared, because a deployment must not
offer a channel it cannot finish (SMS sign-in with nothing able to
deliver a code is a promise no phone will keep); one declared channel
renders its form directly, no tab strip, since a tablist with nothing
to switch between is not a choice.

**A lost race is not a failure.** A submit whose login lost a
concurrent sign-in race is answered with `OperationSupersededError`
and treated as the lost race it is: no banner, no `onSignedIn` — the
winning call fires its own exactly once. The SMS channel additionally
hears what the race cost it: a phone-login code is single-use
server-side, so the losing submit spent the very code it verified and
must not be re-submitted; only a fresh code can sign in.

**Registration never signs in.** `register` is not a session
operation, by spec and by auth-core's contract — the new account signs
in through the sign-in surface. The `'@'` heuristic splits the single
identifier field into the spec's separated email/phone shape rather
than sending one ambiguous value for the backend to guess at.

**Social sign-in is a pure request, completed by the host.** Clicking
a provider asks the session for that channel's authorization URL,
reported upward through `onAuthorizeUrl` — a package that navigated
could not live inside another host's router, popup or tab flow.
`SocialCallbackHandler` completes the exchange at the host's callback
route with its effect ref-keyed on the `(code, state)` pair, so
StrictMode's double effect invocation starts exactly one exchange. A
mount that finds the session already authenticated is a re-entry after
a completed exchange: the single-use code is consumed, no second
exchange starts, and the handler fires `onSignedIn` again, keeping its
pending notice up until the host reacts. Pending and sent states
announce through `role="status"`; failures render in one
`role="alert"` banner, never per-field prose.

**Session end is a pure placeholder.** `SessionEndedScreen` renders
`ui-kit`'s `EmptyState` in the `noPermission` variant — the content is
gated again until the user signs in — with every text slot overridden
from the auth-ui namespace and no session prop, hooks or network. Its
title defaults to `h1` because it replaces the whole authenticated
page: a whole-page placeholder continues nothing.

## Error answers: the reachable-code whitelist

Every submit path resolves its failure to one code and renders it
through the same whole-attempt banner. The resolver maps exactly the
reachable answers of this surface — the authn sign-in, registration,
social and session-lifecycle families plus the `client.*` transport
codes — to dedicated text; anything outside the whitelist, a future
authn code included, renders the `errors.unknown` fallback, so the
bundle never shows a raw key and a missing translation never leaks
another language's text. The classifier collapses non-`ApiError`
throws to a code deliberately not whitelisted, so an operation that
throws at all always has a code to show.

## The stable surface

The eight exported components and their prop tables (`session`
required wherever an operation exists), the
`SocialProvider`/`SocialProviderConfig` types mirroring the authn
spec's channel list, and the bilingual
`AUTH_UI_NAMESPACE`/`authUiResources` pair — hosts reword the kit by
registering their own identical-key bundle pair, never by editing
component text. `@speed/api-sdk` is a type-only dependency; no direct
HTTP exists in `src/`, enforced by the workspace's `no-direct-http`
rule.

## Related pages

- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — the package layers and the controlled-component contract
- [account-ui design](/docs/developer-docs/modules/web/account-ui/) — the signed-in half of the account story; [tenancy-ui design](/docs/developer-docs/modules/web/tenancy-ui/) — the same-tier neighbour; [product-shell design](/docs/developer-docs/modules/web/product-shell/) — the view machine whose sign-in branch this family fills
- User guide: [auth-ui module](/docs/user-guide/modules/web/auth-ui/), [authn module](/docs/user-guide/modules/identity/authn/), [identity and access domain](/docs/user-guide/domains/identity-access/), [building the frontend](/docs/user-guide/domains/frontend-building/)
