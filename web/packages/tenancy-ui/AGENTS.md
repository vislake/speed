# AGENTS.md -- @speed/tenancy-ui

Rules for working in this package and for consuming it. `@speed/tenancy-ui`
ships the tenant-switch affordance: one controlled `TenantSwitcher`
component, its props, the `tenancy-ui` i18n namespace resources, and
nothing else. It is installed into business projects via `npm install`,
so everything below that touches the public surface is contract, not
style.

## Public surface (frozen)

`src/index.ts` exports exactly five names -- `TENANCY_UI_NAMESPACE`,
`tenancyUiResources`, `TenantOption`, `TenantSwitcherProps`,
`TenantSwitcher` -- and nothing more. Every other `src/` file is
`internal/` plumbing: the code-to-text error resolver, the failure
banner, the translation hook. They exist to be tested, never to be
imported by a consumer; adding an export is a breaking change under
lockstep versioning. `TenantOption` is
`{ id, name }` with both fields readonly -- extending the shape, or
making the props' `session`/`tenants`/`currentTenantId` optional,
breaks delivered projects.

## Controlled discipline (non-negotiable in src/)

- The session arrives as a `session` prop; component code never calls
  `attachSession`, never consumes `useAuthState`/`useCurrentTenant`/
  `usePermission` (those are the host's observation points), and never
  calls the session on mount -- every request starts from a user
  gesture on a component.
- Nothing in `src/` navigates, fetches, or touches the network in any
  form. The one network path is `session.switchTenant`, the generated
  operation over the host-bound client; the workspace `speed/no-direct-http`
  rule enforces the rest.
- The current tenant is the host's fact (`currentTenantId` prop), never
  derived here. The component exists to change that fact and reports
  the change through `onSwitched` exactly once per commit.
- Permission-set re-attachment is the host's job. A switch commit drops
  the tenant-domain permission list by auth-core's survival-rule
  design; this package never re-attaches it, and hosts must, or the
  switched-to tenant renders with stale permissions.
- The menu must never offer a way to re-switch to the current tenant:
  its row stays disabled and its click guard returns before any session
  call can start.

## Same-tier boundary

This package sits beside `auth-ui` and `ui-kit` at the same layer. It
never imports `auth-ui` (same-layer dependency is prohibited) and never
imports `ui-kit` from `src/` -- the theme provider appears only in
`test-utils/` and devDependencies, because the test tree must render
under a real host composition. It depends on `@speed/auth-core` as a
runtime import, not a type-only one: `TenantSwitcherProps.session` is
the `AuthSession` type, and `src/TenantSwitcher.tsx` calls the runtime
helper `isOperationSuperseded` to tell a superseded switch (a sibling
operation committed to the session first) from a genuine failure. It
also depends on `@speed/i18n`.

## Error text

The failure banner resolves a switch answer to text only through the
reachable-code whitelist in `src/internal/error-text.ts`: the
membership and account-status codes the switch endpoint answers
(`authn.tenant_membership_required`, `authn.tenant_membership_unavailable`,
`authn.invalid_credentials`), the two token-verification codes of
authn's per-operation and per-route guards (`authn.authentication_required`,
`authn.token_invalid` -- the first written by `RequireAuthenticated` and
the handler's `requirePrincipal` for a credential-less request, the
second by the composed optional `authn.Middleware` for a presented
token that fails verification), the five
session-lifecycle codes (`authn.session_not_found`,
`authn.session_revoked`, `authn.refresh_token_invalid`,
`authn.refresh_token_reused`, `authn.token_expired`), and
`client.network`, `client.timeout`, `client.protocol` -- thirteen codes
plus `errors.unknown`, the fallback for everything else. A raw code
must never render.

Where the switch answer and the sign-in answer share one meaning, the
`errors.authn.*` and `errors.client.*` texts are deliberate verbatim
copies of `@speed/auth-ui`'s error texts for the same codes (same-tier
packages cannot import one another's catalogs; two versions of one
server code's text must not diverge). When auth-ui's texts change, copy
the change here. Three codes are NOT copies: `authn.invalid_credentials`
means the account is not active on the switch surface (never a wrong
password), and the two token-verification codes cannot be answered on
the pre-auth sign-in surface at all, so auth-ui carries no text to
copy -- their texts are authored here, each recorded in the error-text
suite's `SWITCH_AUTHORED_TEXTS` with the reason. The suite pins the pairing in
both directions inside this package -- a whitelist code without its two
bundle leaves fails, a bundle leaf without its whitelist code fails --
and pins the shared-answer copies to their source too: the error-text
suite imports the auth-ui bundles themselves as test data, so a drift
between the packages' texts fails here rather than reaching the
product.

## i18n

Every built-in string lives in the bilingual `tenancy-ui` namespace,
seventeen leaves per language (`tenantSwitcher` three, `errors`
fourteen: ten `authn`, three `client`, `unknown`). Keep
the leaf key sets of `zh-CN.json` and `en-US.json` identical --
`registerNamespace` refuses to register a namespace whose languages'
leaf key sets differ, so the failure surfaces at registration, before
any component can render. Tests must assert language text by importing
the shipped bundles (`src/locales/*.json`) and reading the leaf --
never by inlining a translation string into a test.

## Testing layout

One test file per source file (`TenantSwitcher.tsx` ->
`TenantSwitcher.test.tsx`), shared helpers only in `test-utils/`. Two
network rigs, used for different jobs:

- `session-harness.ts` (component tests): a scripted `RequestFn` bound
  through the same `bindRequestFn` seam a host's real client binds.
  Component tests use it when they must script raw `ApiError`s or
  assert the request contract (method, path, body) directly.
- `real-client.ts` (the journey): a real `@speed/api-client`
  `createClient` over a fetch stand-in answering genuine `Response`
  objects. `src/usage-example.test.tsx` is the one consumer -- it
  compiles and executes the README quick start so the documented
  usage cannot drift from the API.

Behaviour tests assert what the user sees and what observably changed
(store tokens, fired callbacks), never internal call counts of the
component under test.

## Host checklist

Consumers wiring `TenantSwitcher` must (all documented in the README
quick start): pass the current tenant from their own data source
(`currentTenantId`), react to `onSwitched` by refetching the switched
tenant's data and removing the previous tenant's query cache
(`queryClient.removeQueries` over the tenant-namespaced query keys),
and re-attach `/me`-derived permission
lists after a commit. None of that happens here, by design.

One behavioural note for hosts that mount more than one `TenantSwitcher`
on a session (chrome plus a drawer copy): two instances racing the same
session settle by response order, and whichever request answers after a
sibling committed is superseded -- a lost race, never re-issued (a
re-issue would actively re-commit a tenant the user abandoned when the
earlier-sent request settles last) and never reported. The winning
commit is the only one: its instance fires `onSwitched` exactly once
for the tenant the session genuinely runs under, and the host's own
`currentTenantId` converges the trigger. The lost-race rule's recorded
residual is that a session row keeping the lost request's tenant (the
send-order case) converges at the next silent refresh through the
session's own refresh path, with no `onSwitched`; the component header
carries the full reasoning.

## Language

Code, comments, TSDoc and this file are English. User-visible product
text is bilingual and lives only in the namespace resources.
