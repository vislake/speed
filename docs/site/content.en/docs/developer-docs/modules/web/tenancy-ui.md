---
title: tenancy-ui
weight: 10
description: "The tenant-switch affordance — why TenantSwitcher is one fully controlled component over an auth-core session, why a context change must announce itself and never queue, why a lost switch race is never re-issued, and why its error texts copy auth-ui's except where the meaning differs."
---

# tenancy-ui

`@speed/tenancy-ui` is the tenant-switch affordance of a frontend built
on speed: one controlled component, `TenantSwitcher`, that renders the
host's current tenant as a trigger and the host's tenant list as a
menu, and switches the session to the picked tenant through
`session.switchTenant`. The
[user-guide tenancy-ui page](/docs/user-guide/modules/web/tenancy-ui/)
covers the how; this page explains the why.

## Responsibility and boundary

The component sits at the same tier as `auth-ui` — deliberately
auth-aware (it drives an `@speed/auth-core` session passed in as a
prop) but entirely controlled: it never consumes the auth-core hooks,
never reads session state beyond the single operation it drives, never
attaches or persists a session, never navigates and never touches the
network directly. The boundary around its data is just as deliberate:

- **The current tenant is the host's fact.** `currentTenantId` is a
  prop, typically fed from `useCurrentTenant`; the component exists to
  change that value and reports the change through `onSwitched`,
  which fires exactly once after a commit. A host that never updates
  the prop after a switch shows a stale trigger.
- **The tenant list is host data by contract.** No endpoint answers
  "which tenants may this principal switch between", so the list —
  and every entry's name, rendered verbatim and translated by nobody —
  is the host's.
- **Everything after a commit is the host's**: refetching the
  tenant's data, removing the previous tenant's query cache,
  re-attaching `/me`-derived permission lists. The switch itself is a
  session operation — the tenant travels in the request body, never a
  header, and the committed context lives in the fresh access token
  the switch mints.

## Why the switcher is shaped as it is

**A context change must say so out loud.** Switching tenants silently
alters which rows a host shows, so the affordance announces itself
through a `role="status"` live region: while the switch is in flight
the trigger is inert — `aria-disabled`, never the native `disabled`
attribute, because the menu closes onto the trigger the moment the
flight starts and the focus MUI restores on close can only land if the
trigger stays focusable — and one notice names the destination; once
the switch commits, the same region becomes the confirmation naming
the tenant the session now runs under, staying until the next switch
begins (no auto-dismiss timer, so its lifetime never races a screen
reader). A failed switch renders its code text in one `role="alert"`
banner and changes nothing locally: the store keeps its token, the
trigger stays enabled on the same tenant, and the next pick retries.

**The current row can never re-trigger a switch.** It renders disabled
— announced and skipped by assistive tech — and its click guard
returns before the session operation could start, so even a synthetic
click changes nothing. And one switch at a time: while a switch is in
flight the trigger is inert, so the affordance never queues a second
switch behind the first.

**A lost race stays lost — and is never re-issued.** Two switcher
instances (chrome plus a drawer copy) racing `switchTenant` to
different tenants both succeed server-side; auth-core rejects
whichever answer settles after a sibling's commit, supersession
decided by response settlement order, never send order. The superseded
call is a lost race, not a failure: nothing renders and no
`onSwitched` fires — the winning operation's own commit fired its own
exactly once, for the tenant the session genuinely runs under.
Re-issuing the lost request would be actively wrong: when the
earlier-sent request settles last, a re-issue would re-commit the
tenant the user already abandoned. Convergence is the host's
`currentTenantId` or, in the recorded residual settle-in-send-order
case, the next silent refresh, which mints for the server-stored
current tenant.

## Error answers: the reachable-code whitelist

The switch surface resolves a failure to text only for the codes it
can actually draw — thirteen: the switch endpoint's own three answers
(membership refused; membership unavailable, an unwired
`MembershipReader` failing closed; and the account-status
`authn.invalid_credentials`), the two token-verification answers a
switch can draw because it travels authenticated, the five-code
session-lifecycle family, and the three `client.*` transport codes.
Everything else renders the `errors.unknown` fallback, so a raw code
never appears on screen.

The error texts follow a copy discipline with a reason: where the
switch answer and the sign-in answer share one meaning, the texts are
verbatim copies of auth-ui's, because same-tier packages cannot import
one another's catalogs and two versions of one server code's text must
not drift apart in the product — the pairing pinned by the suite
against the auth-ui bundles imported as test data. Three texts are
authored here instead: `authn.invalid_credentials`, whose
switch-surface meaning (account not active) is not the sign-in
surface's (wrong password), and the two token-verification codes,
which a pre-auth sign-in surface cannot be answered with and auth-ui
therefore carries no text for.

## The stable surface

One component with four props (`session`, `tenants`,
`currentTenantId`, `onSwitched`), the `TenantOption` shape, and the
bilingual `TENANCY_UI_NAMESPACE`/`tenancyUiResources` pair. Its
dependency surface is the smallest of the auth-aware tier —
`@speed/auth-core` (the session type and the supersession guard) and
`@speed/i18n`; `ui-kit`'s theme provider appears only in the test
tree. The usage example is the `authn_switchTenant` operation's
in-form consumer proof: one login and three switch attempts over a
real `@speed/api-client`, pinned in order with each switch's bearer
token and `{tenant_id}` body asserted.

## Related pages

- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — the package layers and the no-tenant-header rule that makes switching a token operation
- [auth-ui design](/docs/developer-docs/modules/web/auth-ui/) — the same-tier family whose sign-in surface precedes this component; [product-shell design](/docs/developer-docs/modules/web/product-shell/) — the frame whose `userMenu` slot hosts the switcher
- User guide: [tenancy-ui module](/docs/user-guide/modules/web/tenancy-ui/), [authn module](/docs/user-guide/modules/identity/authn/), [building the frontend](/docs/user-guide/domains/frontend-building/)
