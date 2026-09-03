# @speed/account-ui

The account-management component family of a frontend built on speed:
the signed-in half of the account story, where the `@speed/auth-ui`
family ends. Four surfaces compose a host account page -- the
sessions-and-devices list with per-session and bulk revocation, the
sign-in-history list, the social bindings surface (with the callback
handler that completes a binding at the host's callback route), and the
step-up-gated two-factor (TOTP authenticator + recovery codes) setup.
They are deliberately sections, not a routed screen: the page that owns
them, the headings above them and every surface that lives outside this
package (profile fields, password settings) are host content.

The tier is one step above `auth-ui`'s: reads go through the
@tanstack/react-query hooks generated into `@speed/api-sdk` over the
host's QueryClient -- the one provider an account-ui host supplies
beyond the auth-ui tree -- and writes go through the generated
mutations over the same `bindRequestFn` seam; nothing here reads
storage, attaches a session, navigates or touches the network directly.
Two of the four sections take the `@speed/auth-core` session as a prop,
each for exactly one session operation (the authorize-URL request of
the add area; the step-up verification of the two-factor challenge
dialog); the other two take no props at all -- whose sessions or
history they show comes from the caller's bound client and its access
token. Every built-in string renders from the bilingual `account-ui`
namespace registered through `@speed/i18n`, and the dialogs and
empty/error states render through `ui-kit`'s `ConfirmDialog`/
`EmptyState`, the same discipline the other packages established.

## What ships

| Module | Exports |
|---|---|
| `SessionsSection.tsx` | `SessionsSection` |
| `LoginHistorySection.tsx` | `LoginHistorySection` |
| `SocialBindingsSection.tsx` | `SocialBindingsSection`, `SocialBindingsSectionProps`, `SocialProvider`, `SocialProviderConfig` |
| `BindingCallbackHandler.tsx` | `BindingCallbackHandler`, `BindingCallbackHandlerProps` |
| `MfaSection.tsx` | `MfaSection`, `MfaSectionProps` |
| `resources.ts` | `ACCOUNT_UI_NAMESPACE`, `accountUiResources` |

Everything else (`src/internal/`) is shared plumbing -- the code-to-text
error resolver, the `InlineError` banner, the `StepUpChallenge` dialog
and the translation hook -- and is deliberately not exported.

## Quick start

The account page presumes a signed-in viewer: the host's sign-in
surface (the `auth-ui` family) ran first, and the memory access-token
store holds a token whose session the account surfaces read. The
bootstrap has the four parts of every auth-aware host -- the bilingual
i18n instance with both namespaces a rendered surface can read, the
client and session over one store bound into the api-sdk runtime, the
QueryClient (react-query retries and caching policy are the host's
own), and the host's account page composing the four sections -- plus
the host-owned binding turn: the add area reports an authorize URL
upward, the host routes the provider redirect to the callback route the
URL's `redirect_uri` names, and the handler's `onBound` navigates back.

```tsx
import { useState } from 'react'
import type { ReactElement } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession, type AuthSession } from '@speed/auth-core'
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  ACCOUNT_UI_NAMESPACE,
  accountUiResources,
  BindingCallbackHandler,
  LoginHistorySection,
  MfaSection,
  SessionsSection,
  SocialBindingsSection,
  type SocialProviderConfig,
} from '@speed/account-ui'

// 1. The session and the client share one memory access-token store.
//    The host's sign-in surface ran before this page mounted; the store
//    holds the access token whose session the account surfaces read.
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    fetch: fetchImpl, // the host's fetch implementation
    accessTokenStore: store,
    refreshAccessToken: () => session.refresh(),
  }),
)

// 2. The bilingual instance, both namespaces registered exactly once:
//    the account-ui namespace for this package's strings, and the
//    ui-kit namespace because the confirm dialogs and empty states
//    speak ui-kit-namespace keys.
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  storage: null,
  urlParameterName: null,
  navigatorLanguages: [],
})
registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

// 3. The account surfaces read through the generated react-query hooks,
//    so the page renders under the host's QueryClient.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 0 } },
})

// 4. The channels the account may bind, with the callback route each
//    provider's redirect must land on.
const PROVIDERS: readonly SocialProviderConfig[] = [
  { provider: 'github', redirectUri: 'https://app.example.com/callback/social/github' },
  { provider: 'wechat', redirectUri: 'https://app.example.com/callback/social/wechat' },
]

// The host's account page in miniature: the four sections, and the
// host-owned binding turn -- the add area's authorize URL is reported
// upward, the host "navigates" to the callback route its redirect_uri
// names, BindingCallbackHandler completes the exchange there, and
// onBound navigates back (the sections unmount and remount, exactly as
// they would behind a router; their refetches converge on the server).
function AccountPage({ session }: { session: AuthSession }): ReactElement {
  const [callbackUrl, setCallbackUrl] = useState<string | null>(null)

  if (callbackUrl !== null) {
    const redirectUri = new URL(callbackUrl).searchParams.get('redirect_uri')
    const config = PROVIDERS.find((c) => c.redirectUri === redirectUri)
    const state = new URL(callbackUrl).searchParams.get('state')
    if (config === undefined) {
      throw new Error(`no configured callback route for ${callbackUrl}`)
    }
    // The code arrives in the provider's redirect to this route.
    return (
      <BindingCallbackHandler
        provider={config.provider}
        code={codeFromRedirect}
        state={state ?? ''}
        onBound={() => setCallbackUrl(null)}
      />
    )
  }

  return (
    <div>
      <SessionsSection />
      <LoginHistorySection />
      <SocialBindingsSection
        session={session}
        providers={PROVIDERS}
        onAuthorizeUrl={(url) => setCallbackUrl(url)}
      />
      <MfaSection session={session} />
    </div>
  )
}

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <QueryClientProvider client={queryClient}>
          <AccountPage session={session} />
        </QueryClientProvider>
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` validates before it mutates (identical leaf key
sets across both languages) and must run exactly once per instance,
same as every other speed namespace. This exact composition is
compiled and executed by the package suite (`src/usage-example.test.tsx`)
over a real `@speed/api-client`: its fetch stand-in answers with
genuine `Response` objects, and the journey signs the demo account in
through a real social exchange (the journey's turn zero, credential-
less -- everything after rides the issued token), then walks the page
through the surfaces' stories: one session revoked alone (its row's
icon button, no second confirmation), the rest behind the double-
confirmed revoke-others dialog whose `revoked_count` surfaces in a
success notice; a GitHub unbind refused with `authn.last_login_method`
(the demo account's one sign-in method cannot be shed) that stays on
the page with its code text; a step-up-gated authenticator replacement
-- the first enroll is refused with `authn.step_up_required`, the
challenge dialog's verification rotates the access token, and the
retried enroll opens the replacement wizard whose confirm answers the
show-once recovery codes; and a WeChat binding that walks the
authorize URL to the host's callback route and back, the remounted
page's refetches converging on the bound row. A final `switchLanguage`
leg re-renders the same page in the other supported language. The
whole exchange -- eighteen requests -- is pinned in order at the end,
with each request's authorization header asserted (turn zero
credential-less, `access-1` until the step-up rotates it to
`access-2`). The documented usage cannot drift from the API.

## SessionsSection

The sessions-and-devices surface: every session the authn module holds
for the signed-in account, the session the request's own token belongs
to marked with a current badge. Rows show what the server answers --
the device string (or an unknown-device label when the answer carries
none), the raw user agent as a muted detail line when it differs, the
IP, created and last-seen times rendered through `Intl` in the
surface's current language, AMR values as chips (AMR tokens are opaque
authentication-method references -- server vocabulary, not text to
translate -- so they render as-is) and a status badge telling an active
session from a revoked one. A revoked session stays listed, greyed
out; the section renders the server's list rather than a filtered
view.

Actions: a session that is neither current nor revoked carries a
row-end sign-out button that revokes exactly that session, without a
second confirmation -- a signed-out session is low-loss (its owner can
simply sign in again) and the row itself is never the current one. The
section-top action, sign out other devices, revokes every other
session at once and is the heavier gesture: it sits behind the ui-kit
danger `ConfirmDialog` with double confirm (the first click on the
danger confirm arms it, only the second revokes), and the server's own
`revoked_count` answer surfaces in a success notice. After every
successful revoke the list query is invalidated, so the rows converge
on the server's answer; a refused revoke renders its code text above
the list (`authn.session_not_found` when the session died elsewhere,
the session-lifecycle family, `authn.rate_limited`, `client.*`).

The section takes no props: whose sessions these are, and the right to
revoke them, come from the caller's bound client and its access token.
Empty and failure states hide the section header entirely and render
one ui-kit `EmptyState` (empty / error variant with a retry button), so
the heading order never skips a level.

## LoginHistorySection

The sign-in-history surface: the account's login attempts, newest page
of the server's answer first, read through the generated list hook with
the page size frozen at 20 -- within the spec's 1..200 limit, sized to
render a page of history without scrolling machinery, and deliberately
not paginated: login history is a longest-tail surface and the section
is a read-only account page, not a search tool. Rows show the method
(password, SMS code, social account or enterprise SSO -- values outside
that set, a future channel, render an other label, never a raw value),
the outcome and, when the server recorded them, the time and IP.

Outcome text is guarded server vocabulary, the same discipline as the
error-code whitelist elsewhere in the package: a successful attempt
renders the success label; a failed one resolves its `failure_reason`
token (a bare token like `bad_password`, not an `errors.<code>` style
code -- the authn login surface records bare reason tokens) through the
`history.reason` bundle keys, but only tokens on the known list are
ever passed to `t()` -- a reason outside the list, or an absent one,
renders the generic sign-in-failed label. No raw token and no API
message field can ever reach the row.

The section takes no props. Empty and failure states hide the section
header entirely and render one ui-kit `EmptyState` (empty / error
variant with a retry button), so the heading order never skips a level.

## SocialBindingsSection

The social-account bindings surface: every external identity the authn
module holds bound to the signed-in account, read through the generated
list hook. Each row names the provider (mapped through
`bindings.provider.<provider>` for the five providers the spec hosts --
a value outside that set, a future channel, renders the generic other
label) and the provider account's email when the answer carries one. A
row whose identity carries an id has a row-end unbind action that sits
behind the ui-kit danger `ConfirmDialog`: unbinding is irreversible
(the binding is re-established only by walking the provider's OAuth
flow again), so the row asks once before the DELETE goes out. After a
successful unbind the list query is invalidated; a refused unbind
renders its code text above the list -- `authn.last_login_method` when
the account would be left with no sign-in method (the server decides by
its own login-method count, never computable client-side),
`authn.identity_not_found` when the row was already unbound elsewhere
(a race; the list refetches so the stale row disappears), anything else
through the whitelist.

The add area renders one button per configured provider that is not
already bound; when every configured provider is already bound it does
not render. Clicking a provider asks the session for that channel's
authorization URL -- a pure request, never a navigation: the URL is
reported upward through `onAuthorizeUrl` and the host decides what it
is for (a redirect in the host's router, a popup, a new tab). The host
completes the flow at its callback route with
`BindingCallbackHandler`, below. While one request is in flight its
button disables; a failed answer (`authn.provider_unknown`,
`authn.redirect_uri_not_allowed`, `client.*`) renders through the one
banner.

The provider vocabulary is deliberately not imported from
`@speed/auth-ui` (same-layer packages never import each other):
`SocialProvider` and `SocialProviderConfig` are copied here, shaped
identically to auth-ui's own definitions, and must be kept in sync with
them -- the authn spec is the shared source of truth for the provider
set, and a social channel added to the spec lands in both packages'
copies in the same round.

```ts
export type SocialProvider =
  | 'google'
  | 'github'
  | 'wechat'
  | 'dingtalk'
  | 'feishu'

export interface SocialProviderConfig {
  readonly provider: SocialProvider
  readonly redirectUri: string
}
```

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session that builds authorization URLs for the add area |
| `providers` | `readonly SocialProviderConfig[]` (required) | the channels the host offers for binding, in the order wanted |
| `onAuthorizeUrl` | `(url: string) => void` | receives each channel's authorization URL once built; the host navigates. Omit to run the section without a follow-up |

Empty and failure states render one ui-kit `EmptyState` (empty / error
variant with a retry button) with the section header hidden, so the
heading order never skips a level. The one exception is a first-run
account with an unbound configured provider: there the empty list is
exactly the add area's cue, so the header stays and the add area is the
whole of the content.

## BindingCallbackHandler

Completes a social-account binding at the host's callback route. The
host renders it with the provider and the `code`/`state` the provider
redirected back after its authorize step; an effect -- keyed on the
`(code, state)` pair, so the double effect invocation of StrictMode
development starts exactly one exchange -- posts the pair to the
callback endpoint of the authn spec's social surface (the same
operation the add area's authorize URLs feed). The exchange is a plain
generated call, never a session operation: binding adds an identity to
the caller's own account, it does not sign anyone in.

The answer shape dispatches the outcome. A binding-shaped answer (no
tokens) invalidates the identities list query -- the bound row appears
for any observer of that cache -- and fires `onBound`, the host's cue
to navigate back to the account surface. A login-shaped answer (tokens
present: the caller's sign-in had died and the exchange turned into a
login, signing some account in) renders the dedicated signed-elsewhere
panel and fires nothing: the code is consumed, the other account is
signed in, and this handler must not tell the account surface anything
happened. A failed exchange (`authn.oauth_state_invalid`,
`authn.social_exchange_failed`, `authn.identity_requires_binding`)
renders its code text in the banner under a retry button that re-runs
the exchange for the same pair. Nothing here navigates: the props come
from the host's own route and the callbacks are the host's.

| Prop | Type | Notes |
|---|---|---|
| `provider` | `SocialProvider` (required) | the channel the redirect came back on, a path segment of the callback endpoint, threaded through to the exchange verbatim |
| `code` | `string` (required) | the authorization response the provider redirected back |
| `state` | `string` (required) | the state the authorize URL carried; the server validates it |
| `onBound` | `() => void` | fired once after a binding-shaped answer commits (and the identities list refetched); the host navigates back to the account surface. A handler without one still completes the exchange and refetches the list -- the bound row lands for any observer -- and stays on the pending notice until the host reacts |

## MfaSection

The two-step-verification (TOTP authenticator + recovery codes) surface
of the account page. The authn module ships no factor-status operation
and no disable operation, and its sign-in path is deliberately not
gated on a factor -- verification is a pure step-up mechanism, used to
prove the caller is themselves before security-sensitive operations.
The section therefore never declares enabled or disabled: state is
discovered through actions, and every action that the server gates
behind a step-up answers 403 `authn.step_up_required` when the
caller's current access token carries no fresh second-factor proof.

Two entry actions run through the same discover-by-acting machine:

- Set up an authenticator. A 200 answer means no active factor existed
  and the enrollment is pending: the wizard opens showing the secret
  and the provisioning URI -- both rendered as text, because the
  package ships no QR dependency and no clipboard mechanism, so manual
  entry is the supported path -- then a six-digit confirm makes the
  factor active and the confirm answer's recovery codes open the
  show-once panel. A 403 means an active factor does exist: the step-up
  dialog opens, and only its success path re-runs the enrollment --
  that 403 is the one reliable signal an active factor exists, so the
  replacement warning (this replaces your existing authenticator)
  renders only when the wizard was reached through the step-up, never
  on a first setup.
- Regenerate recovery codes. The handler gates this unconditionally, so
  an unelevated caller gets 403 whether or not a factor exists: the
  step-up dialog opens, and its success path re-runs the regeneration.
  The retry then answers either 200 (the show-once codes panel opens)
  or -- when no active factor actually backs the account, a race the
  step-up cannot rule out -- 404 `authn.mfa_not_enrolled`, which
  renders its guide text and points at the setup entry above.

Confirm answers that end the wizard (409 `authn.mfa_already_enrolled`
when another session confirmed the pending factor first, 404 when it
vanished) close the wizard and render their code text; a wrong code is
a field-level error and stays retryable; everything else (the shared
rate limiter, the session-lifecycle family, `client.*`) renders its
code text banner. The show-once recovery-codes panel is the only place
the codes ever appear (they are served in plaintext exactly once): it
shows the ten codes and a single I-have-saved-them exit, and leaving it
resets all state -- nothing in this package caches or re-fetches codes,
so the only way to see codes again is to regenerate.

Orchestration with the challenge dialog lives entirely inside this
section: the internal step-up dialog asks one thing -- a single code
that verifies the caller is themselves right now, accepting either a
six-digit TOTP code or one recovery code (the authn module's step-up
operation shape-dispatches between them, so the component validates
nothing about the value client-side) -- and drives `session.verifyStepUp`,
the session operation that owns the token lifecycle: a successful
verification settles a fresh access token whose `amr` carries the
just-verified factor, and the pending action is remembered so the
success retries exactly the gated operation and a cancel retries
nothing. The elevation lives only in that access token's lifetime; the
dialog never promises that verification will not be asked again.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session that verifies the step-up challenge. The enroll/confirm/regenerate calls themselves are plain generated mutations travelling on the access token in the shared store -- the session prop exists for the challenge dialog alone |

## Text and i18n

All built-in strings live in the bilingual `account-ui` namespace
(`src/locales/zh-CN.json` and `en-US.json`, 106 keys each with
identical leaf key sets, enforced by registration and by
`tools/check_i18n_keys.py` in CI):

| Section | Purpose |
|---|---|
| `sessions.*` | the session list: title, the current/revoked badges, the unknown-device label, the row-end revoke aria-label, the signed-in/last-seen time templates, the revoke-others action (label, danger-confirm texts and the `done_one`/`done_other` success notice the server's count resolves through), empty and error states |
| `history.*` | the sign-in history: title, one name per recorded method and reason token (`history.method.<method>`, `history.reason.<token>`, each with the other fallback), the success label, empty and error states |
| `bindings.*` | the social bindings: title, one name per provider (`bindings.provider.<provider>` plus `other`), the unbind action and its confirm texts, the add-section title, empty and error states |
| `bindingCallback.*` | the callback handler: the pending notice, the retry action, the signed-elsewhere panel |
| `mfa.*` | the two-factor surface: the authenticator wizard (labels, notices, the replacement warning), the recovery-codes panel, the step-up dialog |
| `errors.*` | code-to-text answers (see below) |

Hosts reword the kit by registering their own identical-key bundle
pair under `ACCOUNT_UI_NAMESPACE` at bootstrap -- never by editing
component text. Two built-in strings a rendered surface can show are
not this package's: the confirm-again label of the armed danger dialog
and the `EmptyState`'s own texts speak `ui-kit`-namespace keys, which
is why the quick start registers both namespaces. `MUI` components
themselves carry no speed text.

### Error text: the reachable-code whitelist

Every failure path of the family resolves its failure to one error code
and renders it through the same `role="alert"` banner (`InlineError`),
never per-field error prose (the one exception is a wrong MFA code,
which is a field-level error). The resolver maps exactly these codes to
their `errors.*` keys -- the reachable answers of the signed-in account
surface, plus the transport codes of the `@speed/api-client` contract:

| Area | Codes with dedicated text |
|---|---|
| Session lifecycle (a revoke or a read of one's own sessions can answer with these; a host renders them for its own protected operations too) | `authn.session_not_found`, `authn.session_revoked`, `authn.token_expired`, `authn.refresh_token_invalid`, `authn.refresh_token_reused` |
| Social binding (the authorize request, the callback exchange, the unbind) | `authn.provider_unknown`, `authn.redirect_uri_not_allowed`, `authn.oauth_state_invalid`, `authn.social_exchange_failed`, `authn.identity_requires_binding`, `authn.identity_already_bound`, `authn.identity_not_found`, `authn.last_login_method` |
| Two-factor and step-up | `authn.step_up_required`, `authn.mfa_not_enrolled`, `authn.mfa_already_enrolled`, `authn.mfa_invalid_code` |
| Shared rate limiting | `authn.rate_limited` |
| Transport (the api-client contract) | `client.network`, `client.timeout`, `client.protocol` |

Anything outside the whitelist -- a future authn code, a
`client.http.<status>` answer, a non-`ApiError` throw -- renders the
`errors.unknown` fallback, so the bundle can never show a raw key, and
a missing translation never leaks another language's text or an
English fallback. The family's classifier (`errorCodeOf`) collapses
non-`ApiError`-shaped throws to a code that is deliberately not
whitelisted, so the fallback is where they land: an operation that
throws at all always has a code to show. Wording policy: the codes
whose failure context is identical to the sign-in surface's (the
session-lifecycle family, `authn.rate_limited`,
`authn.identity_already_bound`, `authn.identity_requires_binding`)
reuse the auth-ui bundle's text verbatim, so the same server answer
reads the same on both surfaces. The whitelist-to-bundle pairing is
pinned in both languages by the suite (`src/internal/error-text.test.ts`).

## Accessibility

Component tests run axe over the rendered document and fail on any
violation, with the same two recorded exceptions shared with
`ui-kit`/`auth-ui`: `color-contrast` is disabled because jsdom computes
no layout or color (a contrast result there is neither trustworthy nor
actionable -- the theme owns contrast and is verified browser-side),
and `region` is disabled because the units under test are components,
not full app pages. The interaction semantics are asserted by the tests
rather than left to axe: pending lists announce through `role="status"`
skeleton containers (`aria-busy`, a per-surface `aria-label`, and the
decorative spinners `aria-hidden` so the announcement is not read
twice), the revoke-others success notice is a `role="status"` alert,
every failure renders in one `role="alert"`, the row-end revoke and
unbind actions are labelled buttons (the revoke is icon-only, so its
`aria-label` is the whole of its name), and the empty and error states
of the list surfaces hide the section header so the `EmptyState`'s own
title stands in for it and the heading order never skips a level.
Dialogs are MUI `Dialog`s (focus managed, Escape and backdrop cancel);
the danger confirmations use `ui-kit`'s `ConfirmDialog`, whose armed
state the suite drives through the ui-kit-namespace confirm-again
label. The MFA confirm field carries `autoComplete="one-time-code"`,
and every in-flight request disables its controls, so an answer cannot
be double-submitted or abandoned mid-rotation.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`
(`SessionsSection.test.tsx`, `LoginHistorySection.test.tsx`,
`SocialBindingsSection.test.tsx`, `BindingCallbackHandler.test.tsx`,
`MfaSection.test.tsx`, plus the internal `error-text.test.ts` and
`step-up-challenge.test.tsx`), shared helpers only in `test-utils/`.
The vitest config aliases the `@speed/*` specifiers onto the sibling
packages' `src` entries, so tests run against live sources -- no test
file imports another package's `dist/`. Two helper layers, both
carrying one provider auth-ui's harness does not: the
`QueryClientProvider`, because the surfaces read through react-query:

- `render.tsx` -- `renderWithProviders` mounts a unit under the tree a
  real host builds (`I18nextProvider` around `AppThemeProvider` around
  `QueryClientProvider`), with a fresh bilingual instance per call (the
  double-registration guard never fires across tests) and both
  namespaces a rendered surface can read registered;
  `createAccountUiI18n` and `createTestQueryClient` are the shared
  factories, the test client retrying nothing so an operation the test
  scripts to fail surfaces on the first attempt.
- `real-client.ts` -- `makeRealClientRig` builds the journey rig: a
  real `@speed/api-client` `createClient` whose fetch stand-in answers
  from a script with genuine `Response` objects, the memory
  access-token store, and the session bound through the same
  `bindRequestFn` seam a host's real client binds -- recording every
  request's method, path, query string and authorization header.
  Because the transport is real api-client machinery, the 401-refresh
  leg is exercised by the client itself rather than scripted around.
  This rig is the whole of the package's test story: every component
  suite drives its surface through it, and every answer a surface can
  render -- including the error codes -- is scriptable as a genuine
  `Response` carrying the API envelope, so no scripted-request-function
  harness (auth-ui's other half) has a counterpart here.

`src/usage-example.test.tsx` compiles and executes the Quick start
composition above end to end over the rig (eighteen requests in a
pinned order, authorization headers asserted per request), and every
component test asserts axe (`expectNoAxeViolations`) and the bilingual
text of its states -- importing the shipped locale files, never
inlining a language literal.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree |
| `@mui/material` | peer (required, ^9) | the `Button`/`Chip`/`TextField`/`Dialog` primitives and the ambient theme |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements |
| `@tanstack/react-query` | peer (required, ^5) | every read is a generated hook over the host's QueryClient -- the shared-QueryClient contract of docs/internal/21, the same peer `@speed/api-sdk` declares |
| `@speed/api-sdk` | dependency | the generated hooks every surface reads through, the exported query-key builders the invalidations use, and `authnSocialCallback`, the plain generated call the binding handler drives -- a runtime dependency, unlike auth-ui's type-only one |
| `@speed/auth-core` | dependency | the `AuthSession` type two public prop tables reference (`SocialBindingsSectionProps`, `MfaSectionProps`), so a consumer resolving the published `.d.ts` needs the specifier declared as a dependency, never a dev one -- the same rule auth-ui applies to the packages its public types reference |
| `@speed/i18n` | dependency | the namespace registration and translation hook every surface renders through |
| `@speed/ui-kit` | dependency | the `ConfirmDialog` and `EmptyState` every surface composes |

`@speed/api-client` stays a devDependency: only the test-utils rig
binds a real client, and no public type of this package references it.
No direct HTTP exists anywhere in `src/` -- every request goes through
the generated operations over the shared seam, and the workspace's
`speed/no-direct-http` rule enforces that (this package is simply
absent from the rule's one whitelist, `packages/api-client`); the
`speed/no-literal-text` rule enforces the namespace discipline over
`src/` the same way it does in every package.

## Known limitations

- **No factor-status, no disable, no sign-in gating.** The authn
  module ships no factor-status operation and no disable operation,
  and its sign-in path is not gated on a factor. `MfaSection` never
  declares enabled or disabled -- state is discovered through actions,
  and only actions the spec actually ships exist: set up or replace an
  authenticator, confirm it, regenerate recovery codes. Turning a
  factor off is not possible through this section, and no operation
  here can make future sign-ins require verification.
- **No change-password surface, and no profile fields.** The authn
  spec ships no change-password operation, so the password-change
  cascade (revoking the other sessions) remains missing end to end,
  and email/phone/display-name editing belongs to the profile round.
  The account page this family composes is the security section of a
  larger host page; the rest of it is host content or later packages.
- **Enrollment is manual-entry.** The package ships no QR rendering
  and no clipboard mechanism, so the wizard shows the secret and the
  provisioning URI as text; a viewer with a camera-equipped phone
  types or copies them by hand.
- **Recovery codes appear exactly once.** They are served in
  plaintext only by the enroll-confirm and regenerate answers, shown
  in the show-once panel, and never cached or re-fetchable; leaving
  the panel discards them, and the only way to see codes again is to
  regenerate. A host that must render a QR code or persist codes for
  its own flows composes outside this package.
- **The session list is the server's list.** Revoked sessions stay
  listed (greyed out), and the current session can never be revoked
  from the list -- signing out of the device in front of you is the
  host's sign-out action, not a row button.
- **Login history is one frozen page.** The section reads the newest
  twenty attempts and never paginates -- deliberate for a read-only
  account page, and the page size is pinned to the spec's limit range
  in the component (`PAGE_SIZE`), not configurable by prop.
- **Server vocabulary renders through whitelists.** Session `amr`
  values render as-is (opaque method references, deliberately
  untranslated), and the login history's method and failure-reason
  tokens render only when they are on the known-token lists; a future
  channel or reason renders its generic other label until the round
  that adds the token ships both bundle keys.
- **The provider vocabulary is copied, not imported.** `SocialProvider`
  and `SocialProviderConfig` are this package's own definitions, kept
  in sync with `@speed/auth-ui`'s by hand (same-layer packages never
  import each other); the authn spec is the shared source of truth,
  and both copies change in the same round as the spec.
- **Session state does not survive a page load, and step-up elevation
  is single-token.** The memory-only session and refresh token are
  auth-core's known limitations, inherited by any host of this family.
  Step-up verification outlives exactly one access token -- the
  surfaces never promise that a gated operation will not ask again
  after the token rotates.

## Deferrals and recorded decisions

- **Reference-app consumer**: `examples/reference-app` has no frontend
  directory yet (Go-only today). The runtime end-to-end consumption of
  the account surface of authn is discharged in form at the package
  level -- `src/usage-example.test.tsx` drives the composed family over
  a real `@speed/api-client` bound through the same seam a host binds,
  with a scripted fetch answering genuine `Response` objects, the same
  honesty standard every package here used before a shell consumer
  existed. The remaining leg, a browser driving a real server, lands
  with the reference-app shells; the M4 e2e pipeline covers the full
  stack.
- **Reads deliberately go through generated react-query hooks, not
  session operations.** The account surfaces read lists (sessions,
  history, identities) that are cacheable shared state and invalidate
  after their own mutations -- the generated-hooks tier of the
  api-sdk contract (docs/internal/21), the tier auth-ui's components
  deliberately do not consume because a sign-in form's answers are
  one-shot, not a cache. This package is the second consumer of the
  generated surface, after auth-core.
- **`auth-ui` stays unimported.** Same-layer packages never import
  each other: the provider vocabulary is copied (see Known
  limitations), the callback endpoints are per-provider path segments
  in the authn spec rather than an auth-ui type, and a host composes
  both families over one session without either package knowing the
  other exists.
- **Storybook / browser-side visual verification**: no preview-harness
  round exists yet, same deferral `ui-kit`, `layout-kit` and `auth-ui`
  carry; `color-contrast` stays axe-disabled for the same jsdom reason
  and is verified browser-side in a later round.

## Development

From `web/packages/account-ui`: `pnpm lint`, `pnpm typecheck`, `pnpm
test`, `pnpm build`. The build compiles `@speed/api-sdk`,
`@speed/auth-core`, `@speed/ui-kit` and `@speed/i18n` first (their
sources are what the aliases and the published `.d.ts` files reference)
and then emits this package's `dist/`; `pnpm build` from the `web/`
root does the same for every package. Shared test helpers live in
`test-utils/`, bilingual fixtures are the shipped locale files under
`src/locales/`, and `src/usage-example.test.tsx` compiles and executes
the Quick start composition above, so the documented usage cannot drift
from the API.
