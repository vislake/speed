# AGENTS.md for `@speed/auth-ui`

The sign-in component family of a speed frontend, over an
`@speed/auth-core` session: the password and SMS-code channels, the
registration form, the social section and the callback handler that
completes its exchange, assembled in `SignInScreen` behind a channel
tab strip, plus `SignOutButton` and the `SessionEndedScreen`
placeholder. The package sits at the same auth-agnostic tier as
`@speed/ui-kit` and `@speed/layout-kit`: it depends on nothing
auth- or routing-shaped beyond auth-core's session and hooks, and it
never decides what happens after a sign-in.

## What this package is

- **Controlled components over a session prop.** Every component takes
  the session it drives as a required prop. Nothing here consumes the
  auth-core hooks, attaches or persists a session, reads storage,
  navigates, or touches HTTP. A successful sign-in fires the host's
  `onSignedIn` callback exactly once; the host observes session
  transitions through its own auth-core hooks and navigates.
- **Headless logic lives in `@speed/auth-core`.** `loginWithPassword`,
  `requestSMSCode`, `loginWithSMSCode`, `register`,
  `socialAuthorizeUrl`, `completeSocialLogin`, `logout`, `refresh` —
  the session contract this family renders — are auth-core's. If an
  operation does not exist on the session, the right home for it is
  auth-core's contract, not a fetch in this package.
- **Every built-in string is bilingual and bundled.** All text renders
  from the `auth-ui` namespace (`AUTH_UI_NAMESPACE`, one
  `zh-CN.json` and one `en-US.json` under `src/locales/`, identical
  leaf key sets), registered by the host exactly once through
  `@speed/i18n` alongside the `ui-kit` namespace.
- **The error surface is a code whitelist.** Every submit path
  resolves its failure to one error code and renders that code's text
  in one `role="alert"` banner; codes outside the whitelist render
  `errors.unknown`, so the bundle can never show a raw key.

## Rules that are load-bearing here

1. **Components stay controlled and hook-free.** A component in `src/`
   must not consume `useAuthState`/`useCurrentTenant`/`usePermission`,
   must not call `attachSession`, and must not navigate. Read state
   changes only through props, and report results only through the
   documented callbacks (`onSignedIn`, `onRegistered`, `onAuthorizeUrl`,
   `onSignIn`), each fired at most once per outcome.
2. **No text outside the namespace, and no key set drift.** User-facing
   strings are added to both locale files in the same commit, under
   their component's section; the workspace `speed/no-literal-text`
   rule refuses inline text in `src/`. A host rewording a string
   re-registers the whole namespace with an identical-key bundle pair —
   never by editing component text here.
3. **New reachable error codes join the whitelist and both locale
   files in one commit.** `src/internal/error-text.ts`'s
   `ERROR_TEXT_CODES` is the reachable-subset whitelist this family
   renders (the 25 authn/session-lifecycle/client codes the README's
   Text and i18n section tables, plus the `errors.unknown` fallback for
   anything else). When a code becomes reachable through a submit path
   of this family, it is added there and to both `errors.*` sections at
   once. Codes deliberately not whitelisted: any `client.http.*` answer
   and `client.unknown` (the classifier's landing slot for
   non-`ApiError` throws) — those render `errors.unknown` by design.
4. **No direct HTTP in `src/`.** Every request flows through the
   session's generated operations over the `bindRequestFn` seam; this
   package is not on the `speed/no-direct-http` whitelist. Tests bind
   their doubles through the same seam (`bindRequestFn` from
   `@speed/api-sdk/runtime`) — never by mocking a module.
5. **The public surface is the `index.ts` exports.** Helpers shared
   between components live in `src/internal/` and are deliberately not
   exported; a component that cannot be built from public pieces does
   not exist. `RegisterFormProps.onRegistered` carries the generated
   `AuthnUser` of `@speed/api-sdk`, so `@speed/api-sdk` is declared a
   **dependency** (type-only today) — consumers resolve the published
   `.d.ts` under pnpm's strict node_modules, and a public type
   reference demands a dependency entry, never a dev one. Do not move
   it back. `@speed/api-client` stays a devDependency: no public type
   references it; only the test-utils rigs bind a real client.
6. **The README's quick start is executed, not aspirational.**
   `src/usage-example.test.tsx` compiles and runs the documented
   composition (real api-client, genuine `Response` objects from a
   scripted fetch, real session, real i18n instance, the host gate) in
   the order the README shows. Any README composition change must keep
   that true; any component behaviour change ships with its README
   section updated in the same commit.
7. **Register/`sign-in` and screens stay separate.** `RegisterForm`
   never signs in (`register` is not a session operation) and
   `SessionEndedScreen` never signs in either — both hand back to the
   host. Screens that exist in this package are `SignInScreen` and
   `SessionEndedScreen`; everything else is a section, form or action
   the host composes.

## Public surface

Everything in `src/index.ts` is public: `SignInScreen` (with
`SignInChannel` and `SocialSignInOptions`), the three channel
components (`PasswordSignInForm`, `SMSSignInForm`, `RegisterForm`),
the social pair (`SocialSignInSection` with `SocialProvider`/
`SocialProviderConfig`, `SocialCallbackHandler`), `SignOutButton`,
`SessionEndedScreen`, and the resource pair (`AUTH_UI_NAMESPACE`,
`authUiResources`). Prop tables and behaviour live in the README;
`tsc -p tsconfig.json` type-checks every consumer-visible signature.

## Testing

Vitest + jsdom, one test file per source file under `src/`, shared
helpers only in `test-utils/`; the vitest config aliases the `@speed/*`
specifiers onto sibling packages' `src` entries, so no test imports
another package's `dist/`. Three helper layers:

- `test-utils/render.tsx` — `renderWithProviders` mounts a unit under
  the real host tree (fresh bilingual instance per call with both
  namespaces registered; `I18nextProvider` around `AppThemeProvider`).
  Bilingual assertions import the shipped locale files, never inline a
  language literal.
- `test-utils/session-harness.ts` — `makeHarness`, the scripted fake
  `RequestFn` bound through `bindRequestFn`, for tests that must script
  raw `ApiError`s or inspect request bodies.
- `test-utils/real-client.ts` + `test-utils/session-gate.tsx` — the
  journey rig: a real `@speed/api-client` over a scripted fetch
  answering genuine `Response` objects (so the 401-refresh leg runs in
  the client machinery itself), and the host gate miniature reading the
  snapshot of the attached session.

`src/usage-example.test.tsx` executes the README quick start end to end
over the real-client rig (six requests in a pinned order, asserted
verbatim); `src/session-journey.test.tsx` drives the journeys beyond
it (SMS channel, explicit sign-out, the social callback route). Every
component test asserts axe (`expectNoAxeViolations`) and the bilingual
text of its states.

## Known limitations

Recorded, with the current disposition, in the README's Known
limitations and Deferrals sections. The load-bearing ones for a
contributor: a registration is not a login (the new account signs in
through the sign-in surface); binding/MFA/enterprise-SSO surfaces are
not shipped (`SocialCallbackHandler` treats its route as a sign-in
surface and refuses the bound-identity answer with `client.protocol`);
the server exposes no channel discovery, so the family renders exactly
the channels the host composes; session state does not survive a page
load and the refresh token is JavaScript-visible in memory (auth-core's
limits, inherited); and the runtime consumer proof is discharged in
form at the package level — the browser + real-server leg lands with
the reference-app shells.
