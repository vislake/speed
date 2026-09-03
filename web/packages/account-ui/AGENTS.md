# AGENTS.md for `@speed/account-ui`

The account-management component family of a speed frontend, the
signed-in half of the account story: four surfaces compose a host
account page — `SessionsSection` (the sessions-and-devices list with
per-session and bulk revocation), `LoginHistorySection` (the
sign-in-history list), `SocialBindingsSection` plus
`BindingCallbackHandler` (the social bindings surface and the callback
route that completes a binding), and `MfaSection` (the
step-up-gated TOTP authenticator + recovery-codes setup). The package
sits one step above `@speed/auth-ui`'s tier: reads go through the
@tanstack/react-query hooks generated into `@speed/api-sdk` over the
host's QueryClient (the shared-QueryClient contract of
docs/internal/21), and writes through the generated mutations over the
same `bindRequestFn` seam. Two surfaces take the `@speed/auth-core`
session as a required prop for exactly one session operation each (the
add area's authorize-URL request; the step-up challenge dialog's
verification); two take no props at all.

## What this package is

- **Controlled sections over a host-owned QueryClient and session.**
  Every read is a generated react-query hook (`useAuthnListSessions`,
  `useAuthnListLoginHistory`, `useAuthnListIdentities`) keyed on the
  caller's bound client; every write is a generated mutation or plain
  generated call (`authnSocialCallback`). Nothing here consumes the
  auth-core hooks, attaches or persists a session, reads storage,
  navigates, or touches HTTP. The session arrives as a prop only where
  a session operation exists that the generated surface cannot
  express — `session.socialAuthorizeUrl` in the bindings add area,
  `session.verifyStepUp` in the two-factor challenge dialog. A
  component with no session operation to drive takes no session prop.
- **The generated-hooks tier, consumed.** auth-ui's forms deliberately
  avoid react-query because a sign-in form's answers are one-shot, not
  a cache; account lists are cacheable shared state that invalidates
  after its own mutations, which is why the generated-hooks tier (and
  therefore the host's QueryClientProvider) exists. Invalidations use
  the exported query-key builders (`getAuthnListIdentitiesQueryKey`),
  never hand-written query keys.
- **Every built-in string is bilingual and bundled.** All text renders
  from the `account-ui` namespace (`ACCOUNT_UI_NAMESPACE`, one
  `zh-CN.json` and one `en-US.json` under `src/locales/`, 106 identical
  leaf keys per language), registered by the host exactly once through
  `@speed/i18n` alongside the `ui-kit` namespace — the confirm-again
  label of the armed danger dialog and the `EmptyState` texts are
  ui-kit-namespace keys.
- **The error surface is a code whitelist with guarded vocabulary.**
  Every failure path resolves to one error code rendered in one
  `role="alert"` banner; codes outside the whitelist render
  `errors.unknown`, never a raw key. The login-history method and
  reason tokens are guarded the same way: a token renders only when it
  is on the component's known-token list, anything else renders the
  generic other label.

## Rules that are load-bearing here

1. **State is discovered through the spec's own answers; the package
   renders exactly the operations that exist.** The authn spec ships
   no factor-status operation and no disable operation, so `MfaSection`
   never declares enabled or disabled — a 403 `authn.step_up_required`
   on an enroll attempt is the discovery that a factor exists, and the
   section opens its step-up dialog and remembers the gated action.
   The spec's delete-semantics shape the dialogs: revoking one's own
   session is low-loss (no second confirmation on the row), revoking
   the rest is a ui-kit danger `ConfirmDialog` with double confirm.
   Do not invent operations the spec does not ship, and do not let a
   section render a state the server never answers with.
2. **No text outside the namespace, no key set drift, and guarded
   server vocabulary.** User-facing strings are added to both locale
   files in the same commit under their component's section; the
   `speed/no-literal-text` rule refuses inline text in `src/`. Session
   `amr` values render as-is (opaque method references, deliberately
   untranslated); login-history method and failure-reason tokens pass
   through `t()` only when on the known-token lists. A host rewording
   a string re-registers the whole namespace with an identical-key
   bundle pair — never by editing component text here.
3. **New reachable error codes join the whitelist and both locale
   files in one commit.** `src/internal/error-text.ts`'s
   `ERROR_TEXT_CODES` is the reachable-subset whitelist this family
   renders (the session-lifecycle family, the social-binding family,
   the two-factor family, `authn.rate_limited`, the three
   `client.*` transport codes — the README's Text and i18n section
   tables them), with `errors.unknown` for everything else. When a
   code becomes reachable through a path of this family, add it there
   and to both `errors.*` sections at once. Codes whose failure
   context matches the sign-in surface's reuse the auth-ui bundle's
   text verbatim — never divergent copy for the same server answer.
   Deliberately not whitelisted: `client.http.*` and `client.unknown`
   (the classifier's landing slot for non-`ApiError` throws) — they
   render `errors.unknown` by design.
4. **No direct HTTP in `src/`, and no react-query outside the host's
   client.** Every request flows through generated operations over the
   `bindRequestFn` seam; this package is not on the
   `speed/no-direct-http` whitelist. Components consume the generated
   hooks and the `useQueryClient`-independent query-key builders; they
   never construct their own QueryClient (the host owns it) and never
   hand-write a query key. Tests bind their doubles through the same
   seam (`bindRequestFn` from `@speed/api-sdk/runtime`) — never by
   mocking a module or by importing another package's `dist/`.
5. **The public surface is the `index.ts` exports; dependencies follow
   public types.** Helpers live in `src/internal/` and are deliberately
   not exported; a component that cannot be built from public pieces
   does not exist. Two prop tables reference `AuthSession` of
   `@speed/auth-core`, and every read is a generated hook of
   `@speed/api-sdk` — so both are declared **dependencies** (api-sdk a
   runtime one: the hooks, the query-key builders and the
   `authnSocialCallback` call all execute here, unlike auth-ui's
   type-only use). Consumers resolve the published `.d.ts` under pnpm's
   strict node_modules, and a public type reference or a runtime import
   demands a dependency entry, never a dev one. Do not move them back.
   `@speed/api-client` stays a devDependency: no public type references
   it; only the test-utils rig binds a real client.
6. **`@speed/auth-ui` is never imported.** Same-layer packages do not
   import each other. `SocialProvider`/`SocialProviderConfig` are this
   package's own definitions, copied to match auth-ui's and kept in
   sync with them — the authn spec is the shared source of truth for
   the provider set, and a channel added to the spec lands in both
   copies in the same round. The social callback endpoints are
   per-provider path segments of the authn spec, not an auth-ui type;
   a host composes both families over one session without either
   package knowing the other exists.
7. **The README's quick start is executed, not aspirational.**
   `src/usage-example.test.tsx` compiles and runs the documented
   composition (real api-client, genuine `Response` objects from a
   scripted fetch, real session, real i18n, the host's QueryClient and
   its callback turn, eighteen requests in a pinned order with
   authorization headers asserted). Any README composition change must
   keep that true; any component behaviour change ships with its README
   section updated in the same commit.
8. **Sections stay sections; nothing navigates, nothing signs anyone
   in or out.** The four surfaces are account-page content under host
   headings — empty and failure states hide the section header and
   render a ui-kit `EmptyState` so heading order never skips a level.
   The bindings add area reports authorize URLs upward through
   `onAuthorizeUrl`; `BindingCallbackHandler` dispatches the exchange's
   answer shape (binding-shaped → refetch + `onBound`; login-shaped →
   the signed-elsewhere panel, no callback — the exchange signed some
   other account in and this handler must not claim the binding
   happened). The current session is never revocable from the list, and
   recovery codes appear exactly once, never cached or re-fetched.

## Public surface

Everything in `src/index.ts` is public: the four surfaces
(`SessionsSection`; `LoginHistorySection`; `SocialBindingsSection`
with `SocialBindingsSectionProps`, `SocialProvider` and
`SocialProviderConfig`; `MfaSection` with `MfaSectionProps`),
`BindingCallbackHandler` with `BindingCallbackHandlerProps`, and the
resource pair (`ACCOUNT_UI_NAMESPACE`, `accountUiResources`). Prop
tables and behaviour live in the README; `tsc -p tsconfig.json`
type-checks every consumer-visible signature.

## Testing

Vitest + jsdom, one test file per source file under `src/`, shared
helpers only in `test-utils/`; the vitest config aliases the `@speed/*`
specifiers onto sibling packages' `src` entries, so no test imports
another package's `dist/`. Two helper layers, carrying one provider
auth-ui's harness does not — the `QueryClientProvider`, because every
read is a react-query hook:

- `test-utils/render.tsx` — `renderWithProviders` mounts a unit under
  the real host tree (`I18nextProvider` around `AppThemeProvider`
  around `QueryClientProvider`), with a fresh bilingual instance per
  call registering both namespaces a rendered surface can read, and
  `createTestQueryClient` retrying nothing so an operation the test
  scripts to fail surfaces on the first attempt.
- `test-utils/real-client.ts` — `makeRealClientRig`, the whole of this
  package's test story: a real `@speed/api-client` over a scripted
  fetch answering genuine `Response` objects (so the 401-refresh leg
  runs in the client machinery itself), the memory access-token store,
  and the session bound through `bindRequestFn`, recording every
  request's method, path, query and authorization header. Every answer
  a surface can render — error codes included — is scriptable as a
  genuine `Response` carrying the API envelope, so auth-ui's
  scripted-request-function harness has no counterpart here.

`src/usage-example.test.tsx` executes the README quick start end to end
over the rig (the signed-in journey: a social exchange signs the demo
account in credential-less, then sessions, history, bindings, the
two-factor wizard and the binding callback each tell their story,
eighteen requests in a pinned order); every component test asserts axe
(`expectNoAxeViolations`) and the bilingual text of its states. The
whitelist-to-bundle pairing is pinned in both languages by
`src/internal/error-text.test.ts`.

## Known limitations

Recorded, with the current disposition, in the README's Known
limitations and Deferrals sections. The load-bearing ones for a
contributor: the spec ships no factor-status/disable/change-password
operations, so `MfaSection` discovers state through 403s and the
change-password cascade stays missing end to end; enrollment is
manual-entry (no QR or clipboard dependency); recovery codes are
show-once, never re-fetchable; the session list renders the server's
list (revoked rows stay, the current session is never revocable here)
and login history is one frozen 20-row page; server vocabulary
(`amr`, history tokens) renders only through its whitelists; session
state does not survive a page load, the refresh token is
JavaScript-visible in memory, and step-up elevation outlives exactly
one access token (auth-core's limits, inherited); and the runtime
consumer proof is discharged in form at the package level — the
browser + real-server leg lands with the reference-app shells.
