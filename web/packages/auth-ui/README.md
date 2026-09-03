# @speed/auth-ui

The sign-in component family of a frontend built on speed: the
password, SMS-code and registration channels, the social sign-in
section and the callback handler that completes its exchange, plus the
sign-out action and the session-ended placeholder -- assembled in
`SignInScreen` behind a channel tab strip. Every piece is a controlled
component over an `@speed/auth-core` session passed in as a prop, at
the same tier as `ui-kit` and `layout-kit`: components never consume
the auth-core hooks, never read session state, never attach or
persist a session, never navigate, and never touch the network
directly -- a successful sign-in fires the host's `onSignedIn`
callback once, and everything that happens next is the host's. Every
built-in string renders from the bilingual `auth-ui` namespace
registered through `@speed/i18n`, and the form fields render through
`ui-kit`'s `FormField`/`FormLayout`, the same discipline the other
packages established.

## What ships

| Module | Exports |
|---|---|
| `SignInScreen.tsx` | `SignInScreen`, `SignInScreenProps`, `SignInChannel`, `SocialSignInOptions` |
| `PasswordSignInForm.tsx` | `PasswordSignInForm`, `PasswordSignInFormProps` |
| `SMSSignInForm.tsx` | `SMSSignInForm`, `SMSSignInFormProps` |
| `RegisterForm.tsx` | `RegisterForm`, `RegisterFormProps` |
| `SocialSignInSection.tsx` | `SocialSignInSection`, `SocialSignInSectionProps`, `SocialProvider`, `SocialProviderConfig` |
| `SocialCallbackHandler.tsx` | `SocialCallbackHandler`, `SocialCallbackHandlerProps` |
| `SignOutButton.tsx` | `SignOutButton`, `SignOutButtonProps` |
| `SessionEndedScreen.tsx` | `SessionEndedScreen`, `SessionEndedScreenProps` |
| `resources.ts` | `AUTH_UI_NAMESPACE`, `authUiResources` |

Everything else (`src/internal/`) is shared plumbing -- the
code-to-text error resolver, the whole-attempt failure banner, the
translation hook -- and is deliberately not exported.

## Quick start

A host composes the family over the session-and-client wiring
`@speed/auth-core`'s README documents. The bootstrap has four parts:
the bilingual i18n instance with both namespaces a rendered form can
read, the client and session over one memory access-token store, the
session attached to the hooks, and the host gate that switches between
the sign-in surface, the app and the session-ended placeholder on the
`useAuthState` snapshot.

```tsx
import { useEffect, useState } from 'react'
import {
  createClient,
  createMemoryAccessTokenStore,
} from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { authnGetMe } from '@speed/api-sdk'
import {
  createAuthSession,
  attachSession,
  useAuthState,
} from '@speed/auth-core'
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import {
  AppThemeProvider,
  UI_KIT_NAMESPACE,
  uiKitResources,
} from '@speed/ui-kit'
import {
  AUTH_UI_NAMESPACE,
  authUiResources,
  SessionEndedScreen,
  SignInScreen,
} from '@speed/auth-ui'

// 1. The session and the client share one memory access-token store.
//    refreshAccessToken is the session's silent refresh: an expired-token
//    401 on any request runs it once and retries with the fresh token.
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

// 2. The hooks read the attached session (last bind wins).
attachSession(session)

// 3. The bilingual instance, both namespaces registered exactly once:
//    the auth-ui namespace for this package's strings, and the ui-kit
//    namespace because FormField's validation text lives there.
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  storage: null,
  urlParameterName: null,
  navigatorLanguages: [],
})
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

// 4. The host's router in miniature: while the snapshot is authenticated
//    the app renders; an anonymous snapshot at a view that held an
//    authenticated one is the session-ended state; before the first
//    authentication it is the sign-in surface. The gate observes -- no
//    component in this family signals into the host's router.
function AppGate() {
  const snapshot = useAuthState()
  const [reachedApp, setReachedApp] = useState(false)
  useEffect(() => {
    if (snapshot.state === 'authenticated') {
      setReachedApp(true)
    }
  }, [snapshot.state])

  if (snapshot.state === 'authenticated') {
    return <ProtectedArea />
  }
  if (reachedApp) {
    return <SessionEndedScreen onSignIn={() => setReachedApp(false)} />
  }
  return <SignInScreen session={session} />
}

// The host's app view: protected content that reads the caller's identity
// through the generated /me operation, routed through the bound client.
function ProtectedArea() {
  const [userId, setUserId] = useState<string | undefined>(undefined)
  return (
    <div>
      <p>Signed in</p>
      <button type="button" onClick={() => void authnGetMe().then((me) => setUserId(me.user_id))}>
        Check session
      </button>
      {userId !== undefined ? <p>Session ok for {userId}</p> : null}
    </div>
  )
}

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <AppGate />
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
genuine `Response` objects, and the journey drives a password sign-in
(the store holds the issued access token), a protected request refused
with `authn.token_expired` that the client silently refreshes (the
refresh travels credential-less by declaration, the retried request
carries the fresh token), and a server-side session death whose own
refused refresh converges the snapshot to anonymous -- the gate's
anonymous snapshot at the app view is the session-ended screen.
Signing in again returns to the form, and a `switchLanguage` leg
re-renders the same surface in the other supported language. The
documented usage cannot drift from the API.

## SignInScreen

The assembled sign-in surface: a channel tab strip (password or SMS
code, labels from the bundle, password first) switches the form below
it, and when `social` is given the social section renders under the
form with its divider title between them. Switching channels unmounts
the previous form, so its half-typed state and whole-attempt error are
gone with it -- a deliberate reset: channel errors must not leak
across surfaces. A successful sign-in on any channel fires `onSignedIn`
once. The screen renders no heading of its own: the page above
(branding, the heading, the register link) is host content.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session every channel on the screen drives |
| `social` | `SocialSignInOptions` | the screen's social block; omitted to run without one |
| `defaultChannel` | `'password' \| 'sms'` | the channel selected on first render; defaults to `'password'` |
| `onSignedIn` | `() => void` | fired once after a sign-in commits on any channel |

`SocialSignInOptions` carries the channels in the order the host wants
them and the same `onAuthorizeUrl` seam `SocialSignInSection` takes:

```ts
export interface SocialSignInOptions {
  readonly providers: readonly SocialProviderConfig[]
  readonly onAuthorizeUrl?: (
    provider: SocialProvider,
    authorizeUrl: string,
  ) => void
}
```

## The channel forms

Each form is a controlled react-hook-form flow over one slice of the
session contract, rendered through `ui-kit`'s `FormLayout`/`FormField`
(the skeleton owns the `<form>`, validation-error rendering and the
actions row; the component owns the submission, the busy state and the
whole-attempt failure banner). Fields carry channel-appropriate
`autoComplete` values. A failed submit changes nothing on the session
and renders one `role="alert"` banner resolving the answer's error
code to current-language text (see Text and i18n below); a successful
one fires `onSignedIn` once and the host navigates. None of them reads
the session snapshot or renders a title.

### PasswordSignInForm

One identifier field -- email or phone, the backend decides which --
and a password field, over `session.loginWithPassword`.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session the password login drives |
| `onSignedIn` | `() => void` | fired once after a password login commits |

### SMSSignInForm

A two-step flow: the phone step requests a code
(`session.requestSMSCode`) -- whose 202 acceptance, and the
account-existence ambiguity the endpoint answers with, is the phone
step's terminal state -- then the code step completes the sign-in with
`session.loginWithSMSCode`. The sent notice announces the receiving
number (`role="status"`); resend repeats the request against the same
number; changing the phone returns to the first step. The request
step renders the code the server answers: `authn.invalid_phone` when
the number has no E.164 form (no leading '+' and country code) and
`authn.rate_limited` when the attempt trips the send policy, each
through the one banner.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session the SMS sign-in drives |
| `onSignedIn` | `() => void` | fired once after an SMS-code login commits |

### RegisterForm

Registration never signs in -- `register` is not a session operation.
One identifier field accepts an email or a phone number: the `'@'`
heuristic decides which slot the request carries, honouring the spec's
separated email/phone shape rather than a single ambiguous identifier.
The optional display name is trimmed and omitted when blank; the
locale the request declares is the session's current UI language, read
at submit time so a mid-flight language switch is honoured. Password
policy and identifier canonical form live on the backend, whose
code-level answers (`authn.password_too_short` and friends, and the
identifier-format refusals `authn.invalid_email` / `authn.invalid_phone`)
render through the one banner. The created user goes to `onRegistered` -- with a callback the
form stays quiet and the host navigates to its sign-in screen -- or,
without one, renders as a success panel in place of the form.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session the registration drives |
| `onRegistered` | `(user: AuthnUser) => void` | receives the created user once; the callback's type is the generated `AuthnUser` of `@speed/api-sdk` |

## Social sign-in

### SocialSignInSection

One outlined button per configured provider, each answering the
bundle's name for it (`social.provider.<provider>`). Clicking a
provider asks the session for that channel's authorization URL -- a
pure request, never a navigation: the URL is reported upward through
`onAuthorizeUrl` and the host decides what it is for (a redirect in
the host's router, a popup, a new tab). While one request is in flight
its button disables and the others stay live; a failed answer
(`authn.provider_unknown`, `authn.redirect_uri_not_allowed`) renders
through the one banner.

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
| `session` | `AuthSession` (required) | the session that builds authorization URLs |
| `providers` | `readonly SocialProviderConfig[]` (required) | the channels to render, in the order the host wants |
| `onAuthorizeUrl` | `(provider, authorizeUrl) => void` | receives each channel's authorization URL once built; the host navigates |

### SocialCallbackHandler

Completes a social sign-in at the host's callback route. The host
renders it with the provider and the `code`/`state` the provider
redirected back; an effect -- ref-keyed on the `(code, state)` pair,
so the double effect invocation of StrictMode development starts
exactly one exchange -- completes the login through
`session.completeSocialLogin` and fires `onSignedIn` once. A mount
that finds the session already authenticated is a re-entry to the
callback URL after a completed exchange: the single-use code is
consumed, so no second exchange is started -- the handler fires
`onSignedIn` again and keeps the pending notice up until the host
reacts. Without either an `onSignedIn` handler or a gate on the
authenticated snapshot the pending notice stays up by design: the
handler never paints the success itself, and the commit of an
exchange is observable through the auth-core hooks the host attaches.
While the exchange is in flight the handler shows the pending notice
(`role="status"`); a failed exchange (`authn.oauth_state_invalid`,
`authn.social_exchange_failed`, `authn.identity_requires_binding`)
renders its code text in the banner under a retry button that re-runs
the exchange for the same pair. The props come from the host's own
route; nothing here navigates.

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session that completes the exchange |
| `provider` | `SocialProvider` (required) | the channel the redirect came back on, threaded through to the exchange verbatim |
| `code` | `string` (required) | the authorization response the provider redirected back |
| `state` | `string` (required) | the state the authorize URL carried; the server validates it |
| `onSignedIn` | `() => void` | fired once after the exchange commits -- and once more on a later mount that finds the session already signed in (the re-entry case starts no exchange), so the host bounces the viewer onward |

## SignOutButton

The sign-out action over `session.logout()`. A click drives the
logout; while the request is in flight the button disables and shows
the busy label; a failed logout renders the answer's code text
(session-lifecycle codes like `authn.session_revoked` included) in one
banner and leaves the button ready to retry. A successful logout is
deliberately quiet: it clears the session and flips the snapshot to
anonymous, and the host's own auth-core hooks observe that flip -- the
component never navigates and fires no completion callback. It renders
whether or not the snapshot is authenticated; the host mounts it where
a sign-out action belongs (typically app chrome).

| Prop | Type | Notes |
|---|---|---|
| `session` | `AuthSession` (required) | the session the sign-out drives |

## SessionEndedScreen

The pure placeholder a host renders where protected content used to
be. The session lifecycle is observable, not imperative: there is no
recovery and no refresh cookie, so a page reload starts anonymous, and
a server-side session death converges through the api-client refresh
leg -- `refresh()` resolving `false` flips the snapshot to anonymous.
The host watches its own auth-core hooks and, at any view that just
lost its authenticated snapshot, mounts this screen; its action hands
the host back to its sign-in surface.

The screen is pure presentation: no session prop, no hooks, no
network. It renders `ui-kit`'s `EmptyState` in the `noPermission`
variant -- the lock icon, because the content is gated again until the
user signs in -- with every text slot overridden from the auth-ui
namespace, so nothing of `ui-kit`'s built-in texts can leak through.

| Prop | Type | Notes |
|---|---|---|
| `onSignIn` | `() => void` (required) | fired when the viewer asks to sign in again; the host navigates |

## Text and i18n

All built-in strings live in the bilingual `auth-ui` namespace
(`src/locales/zh-CN.json` and `en-US.json`, 59 keys each with
identical leaf key sets, enforced by registration and by
`tools/check_i18n_keys.py` in CI):

| Section | Purpose |
|---|---|
| `passwordSignIn.*` | the password channel: title, field labels, submit |
| `smsSignIn.*` | the SMS channel: title, phone/code labels, send/resubmit/change-phone actions, the sent notice |
| `register.*` | the registration channel: labels, submit, the no-callback success panel |
| `social.*` | the social section: the divider title and one name per provider (`social.provider.<provider>`) |
| `socialCallback.*` | the callback handler: the pending notice, the retry action |
| `signOut.*` | the sign-out button: label and busy state |
| `sessionEnded.*` | the session-ended screen: title, description, sign-in-again action |
| `errors.*` | code-to-text answers (see below) |

Hosts reword the kit by registering their own identical-key bundle
pair under `AUTH_UI_NAMESPACE` at bootstrap -- never by editing
component text. `MUI` components themselves carry no speed text.

### Error text: the reachable-code whitelist

Every submit path of the family resolves its failure to one error
code and renders it through the same whole-attempt `role="alert"`
banner (`InlineError`), never per-field error prose. The resolver
maps exactly these codes to their `errors.*` keys -- the reachable
answers of the authn surface this family drives, plus the transport
codes of the `@speed/api-client` contract:

| Area | Codes with dedicated text |
|---|---|
| Sign-in and register (identifier, canonical-format, credential, policy, attempt answers) | `authn.invalid_credentials`, `authn.tenant_membership_required`, `authn.account_locked`, `authn.rate_limited`, `authn.verification_code_invalid`, `authn.email_already_registered`, `authn.phone_already_registered`, `authn.identifier_required`, `authn.invalid_email`, `authn.invalid_phone`, `authn.password_too_short`, `authn.password_too_long`, `authn.password_too_weak` |
| Social endpoints | `authn.provider_unknown`, `authn.redirect_uri_not_allowed`, `authn.oauth_state_invalid`, `authn.social_exchange_failed`, `authn.identity_requires_binding`, `authn.identity_already_bound` |
| Session lifecycle (a sign-out call can answer with these; a host renders them for its own protected operations too) | `authn.session_not_found`, `authn.session_revoked`, `authn.refresh_token_invalid`, `authn.refresh_token_reused`, `authn.token_expired` |
| Transport (the api-client contract) | `client.network`, `client.timeout`, `client.protocol` |

Anything outside the whitelist -- a future authn code, a
`client.http.<status>` answer, a non-`ApiError` throw -- renders the
`errors.unknown` fallback, so the bundle can never show a raw key,
and a missing translation never leaks another language's text or an
English fallback. The family's classifier (`errorCodeOf`) collapses
non-`ApiError`-shaped throws to `client.unknown`, a code deliberately
not whitelisted, so the fallback is where they land: a submit that
throws at all always has a code to show.

## Accessibility

Component tests run axe over the rendered document and fail on any
violation, with two recorded exceptions shared with `ui-kit`: `color-
contrast` is disabled because jsdom computes no layout or color (a
contrast result there is neither trustworthy nor actionable -- the
theme owns contrast and is verified browser-side), and `region` is
disabled because the units under test are components, not full app
pages. The interaction semantics are asserted by the tests rather than
left to axe: sent notices and the social-callback pending state
announce through `role="status"` containers (the pending spinner is
`aria-hidden`, so the notice is not read twice), every failure renders
in one `role="alert"`, buttons disable while their request is in
flight, the channel strip is MUI `Tabs` (arrow-key navigation, labelled
by the channel titles), and each field carries the `autoComplete` value
matching its channel (`username`, `current-password`,
`new-password`, `tel`, `one-time-code`) plus `inputMode="numeric"`
for the SMS code.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/`. The vitest config aliases the
`@speed/*` specifiers onto the sibling packages' `src` entries, so
tests run against live sources -- no test file imports another
package's `dist/`. Three layers of helpers, mirroring `@speed/auth-core`'s:

- `render.tsx` -- `renderWithProviders` mounts a unit under the tree a
  real host builds (`I18nextProvider` around `AppThemeProvider`), with
  a fresh bilingual instance per call (the double-registration guard
  never fires across tests) and both namespaces a rendered form can
  read registered; `createAuthUiI18n` is the shared instance factory.
  Bilingual assertions import the shipped bundles
  (`../locales/zh-CN.json`, `en-US.json`), never inline a language
  literal.
- `session-harness.ts` -- `makeHarness` drives component tests'
  sessions: a scripted fake `RequestFn` bound through the same
  `bindRequestFn` seam a host's real client binds, a real session over
  a fresh memory store, and assertions on observable state only (store
  tokens, request bodies, raw `ApiError`s). The endpoint constants are
  the same literal keys auth-core's harness uses.
- `real-client.ts` + `session-gate.tsx` -- the journey rig:
  `makeRealClientRig` builds a real `@speed/api-client`
  `createClient` whose fetch stand-in answers from a script with
  genuine `Response` objects, recording every request's method, path
  and authorization header; because the transport is real api-client
  machinery, the 401-refresh leg is exercised by the client itself
  rather than scripted around. `SessionGate` is the host's router in
  miniature, reading the snapshot of the attached session and
  switching between host-provided app content, the sign-in surface and
  the session-ended screen.

`src/usage-example.test.tsx` compiles and executes the Quick start
composition above end to end over the real-client rig (six requests in
a pinned order: the password sign-in, a refused and refreshed
protected call, a refused refresh converging to the session-ended
screen, and the sign-in-again turn), and `src/session-journey.test.tsx`
drives the journeys beyond the example -- the SMS channel, an explicit
sign-out and the social callback route -- over the same rig and gate.
Every component test asserts axe and the bilingual text of its states.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree |
| `@mui/material` | peer (required, ^9) | the `TextField`/`Button`/`Tabs`/`Alert` primitives and the ambient theme |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements |
| `react-hook-form` | peer (required, ^7) | every form is a controlled react-hook-form flow; RHF is the host's single instance |
| `@speed/auth-core` | dependency | the session every component drives, and the hooks the host observes through |
| `@speed/i18n` | dependency | the namespace registration and translation hook every component renders through |
| `@speed/ui-kit` | dependency | the `FormLayout`/`FormField` form skeleton and `EmptyState` (the session-ended screen) |
| `@speed/api-sdk` | dependency (type-only today) | `RegisterForm`'s public callback carries the generated `AuthnUser`, so a consumer resolving the published `.d.ts` needs the specifier declared as a dependency, never a dev one -- the same rule `auth-core` applies to the packages its public types reference |

`@speed/api-client` stays a devDependency: only the suite binds a real
client, and no public type of this package references it. No direct
HTTP exists anywhere in `src/` -- every request goes through the
session's generated operations over the shared seam, and the
workspace's `speed/no-direct-http` rule enforces that (this package is
simply absent from the rule's one whitelist, `packages/api-client`);
the `speed/no-literal-text` rule enforces the namespace discipline over
`src/` the same way it does in every package.

## Known limitations

- **A registration is not a login.** `RegisterForm` never signs in --
  `register` is not a session operation, by spec and by auth-core's
  contract -- so after a successful registration the new account signs
  in through the sign-in surface the host navigates to (or, without an
  `onRegistered` callback, the form's success panel says so
  explicitly).
- **Account binding and step-up surfaces live in `@speed/account-ui`,
  not here.** This family is the sign-in surface: `SocialCallbackHandler`
  handles the callback of a sign-in exchange, and one answering with
  the bound-identity shape and no tokens -- the server's answer to an
  already-authenticated caller -- is refused with `client.protocol`,
  per auth-core's `completeSocialLogin` contract. The authenticated
  half of that answer is account management's own, and it shipped: an
  already-signed-in caller linking another channel to the account
  walks the add area of `@speed/account-ui`'s `SocialBindingsSection`
  (the authorize URL reported upward, the provider redirect completed
  by the package's `BindingCallbackHandler` at the host's callback
  route), and step-up-gated actions (`RequireStepUp` on the server)
  drive the challenge inside the package's `MfaSection`. Enterprise
  OIDC/SSO has no section in either family -- its discovery is
  per-tenant configuration.
- **No channel discovery.** The server exposes no endpoint answering
  "which sign-in channels may this tenant's users use", so the family
  renders exactly the channels the host composes: `SignInScreen`
  without the `social` prop renders no social block, and the register
  link that sits above a real sign-in page is host content, not a
  package export. When a discovery surface exists, hosts compose with
  it; nothing here changes.
- **Session state does not survive a page load, and the refresh token
  is JavaScript-visible in memory** -- auth-core's own known
  limitations, inherited by any host of this family: reloading starts
  anonymous and the user signs in again. See `@speed/auth-core`'s
  README for the full account.

## Deferrals and recorded decisions

- **Reference-app consumer**: `examples/reference-app` has no frontend
  directory yet (Go-only today). The runtime end-to-end consumption of
  the authn surface is discharged in form at the package level --
  `src/usage-example.test.tsx` drives the composed family over a real
  `@speed/api-client` bound through the same seam a host binds, with a
  scripted fetch answering genuine `Response` objects -- the same
  honesty standard every package here used before a shell consumer
  existed. The remaining leg, a browser driving a real server, lands
  with the reference-app shells (which will also be where generated
  TanStack Query hooks and tenant query-key namespacing first appear,
  and where `RouteGuard` -- its host-composition shape already proven
  in form by `product-shell`'s gated-journey suite, which mounts the
  gate inside the shell's children and feeds it a status the fixture
  host computes from the role lists it attached -- first wires behind
  real permission fetches and a router); the M4 e2e pipeline covers
  the full stack.
- **Auth-core hooks stay host-side by design.** Components in this
  family take the session as a prop and fire callbacks; the host
  observes session transitions with `useAuthState` and friends. A
  component that needed the hooks would have to be mounted under the
  host's `attachSession` anyway -- this family is the reason the host
  gate exists, not a consumer of it.
- **Storybook / browser-side visual verification**: no preview-harness
  round exists yet, same deferral `ui-kit` and `layout-kit` carry;
  `color-contrast` stays axe-disabled for the same jsdom reason and is
  verified browser-side in a later round.

## Development

From `web/packages/auth-ui`: `pnpm lint`, `pnpm typecheck`, `pnpm
test`, `pnpm build`. The build compiles `@speed/auth-core` and
`@speed/ui-kit` first (their sources are what the aliases and the
published `.d.ts` files reference) and then emits this package's
`dist/`; `pnpm build` from the `web/` root does the same for every
package. Shared test helpers live in `test-utils/`, bilingual fixtures
are the shipped locale files under `src/locales/`, and
`src/usage-example.test.tsx` compiles and executes the Quick start
composition above, so the documented usage cannot drift from the API.
