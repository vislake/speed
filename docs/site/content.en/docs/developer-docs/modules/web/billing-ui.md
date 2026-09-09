---
title: billing-ui
weight: 12
description: "The billing-documents read surface — why the family renders exactly the read operations the billing module offers, why one newest-first window is deliberate, why documents render in the UTC calendar, and why a row expands into a fresh single-document read."
---

# billing-ui

`@speed/billing-ui` is the billing-documents family of a frontend
built on speed: one section, `InvoicesSection`, renders the caller's
tenant's invoices — newest first, each row expandable into the single
document's detail — through the generated billing read operations. It
is the read surface of the [billing
module](/docs/developer-docs/modules/capabilities/billing/)'s
channel-agnostic Invoice model. The
[user-guide billing-ui page](/docs/user-guide/modules/web/billing-ui/)
covers the how; this page explains the why.

## Responsibility and boundary

The section is deliberately a section, not a routed screen: the page
that owns it, the headings above it and every surface outside this
package (subscription management, payment) are host content. The
boundary follows the sibling discipline:

- **The read surface is the whole surface.** The billing HTTP
  fragment ships exactly two read operations — list and get, no
  writes — so the family renders exactly the read surface the module
  offers. Nothing settles, voids or creates an invoice, and nothing
  invalidates a query after a mutation: a document whose status changed
  elsewhere converges through the host's refetch policy.
- **No props, no tenant value anywhere.** The billing operations
  carry no tenant concept: whose invoices a read returns is decided by
  the caller's access token, so the section takes no props and no
  tenant is ever a prop or a header.
- **No session layer of its own.** The store holds the bearer token
  the host's sign-in flow planted; a host with a session layer passes
  its refresh through the client's `refreshAccessToken` seam. A
  refused read surfaces its own code.
- **Generated hooks only.** Reads go through the react-query hooks
  generated into `@speed/api-sdk` over the host's QueryClient — the
  one provider a host supplies beyond the theme tree. The section
  never builds its own client and never hand-writes a query key.

## Why the section is shaped as it is

**One newest-first window, deliberately unpaginated.** The section
reads the newest fifty documents — frozen in-range within the spec's
1..100 limit, sized to render recent documents without scrolling
machinery. The spec serves no keyset cursor, so a paginated read over
a whole history is not part of the surface.

**A row expands into a fresh read, because the list is a snapshot.**
Rows and document are the same `BillingInvoice` shape, but expanding a
row re-reads the single document through `billing_getInvoice` and
renders the get's own fresh answer: a status settled after the list
was fetched shows its current value in the document while the
collapsed row keeps the list's snapshot, and the document renders
fields the row does not — both cycle bounds, the last-update time, the
two ids. The detail is a named region with `aria-expanded`/
`aria-controls` wiring, its loading announced, a refused read rendered
as the code-level banner with a retry.

**Documents render in the UTC calendar the server issued them
against.** The model stores UTC instants, so an invoice issued on
July 31 stays July 31 for a viewer in any timezone — a document date
is the same date for every viewer, the property a billing document
needs and a local-timezone reading would break. Amounts, dates and
periods render through `Intl` in the surface's current language,
never hand-formatted; a cycle whose bounds lie in one calendar month
reads as that month, bounds that cross a month render as the full
date range, and a malformed period falls back to the document id —
which is always true — rather than ever reaching `Intl`.

**The status vocabulary is the spec's closed set.** The three
lifecycle states (open, paid, void) render as chips whose labels are
the status's own text — color never carries the meaning alone — and a
status outside the set, a future state an answer already carries,
renders no chip, never a raw value.

**Loading, empty and error states respect the heading order.** An
unresolved load — the first load in flight, or parked by react-query's
offline mode — keeps the loading branch, header included; only a
settled query leaves it. A genuine zero-invoice answer hides the
section header and renders `ui-kit`'s `EmptyState` empty variant; a
failed load renders the error variant with a retry. In every settled
state the `EmptyState` title stands in for the hidden `h2` at its own
level, so the heading order never skips.

## Error answers: the reachable-code whitelist

The whitelist has nine codes: `billing.invoice_not_found` (the
document read's own 404 — an id that names no invoice of the caller's
tenant), the five-code session-lifecycle family a protected read draws
when the caller's session dies mid-flight, and the three `client.*`
transport codes. The billing 400 pair is unreachable by construction —
the list hook always sends the frozen in-range limit — and the 500
envelopes, a future billing code, a `client.http.<status>` answer and
a non-`ApiError` throw all render the `errors.unknown` fallback, never
a raw key. Every code whose answer text this package shares with the
sign-in family is a verbatim copy of the auth-ui bundle's text, so the
same server answer reads the same on every surface.

## The stable surface

`InvoicesSection` (no props), the bilingual
`BILLING_UI_NAMESPACE`/`billingUiResources` pair, and the two
generated hooks it consumes. Dependencies are the generated surface
(`@speed/api-sdk`, a runtime dependency), i18n and ui-kit's
`EmptyState`; `@speed/api-client` stays a devDependency, bound only by
the test rig. Its usage example — two requests pinned in order with
authorization headers asserted — is the read surface's in-form
consumer proof; no in-workspace consumer shell renders this family
yet, so the surface is proven at the package level, with no
browser-and-real-server leg, recorded as such.

## Source

- [web/packages/billing-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/billing-ui/README.md) — the contract, the error whitelist and Known limitations
- [docs/internal/12-frontend.md](https://github.com/vislake/speed/blob/main/docs/internal/12-frontend.md) — the frontend package layers (the generated-hooks read tier this package consumes)
- The [billing module design](/docs/developer-docs/modules/capabilities/billing/) — the Invoice model and read-only fragment behind the surface

## Related pages

- [Frontend architecture](/docs/developer-docs/frontend-architecture/) — the package layers and how packages map onto backend modules
- [account-ui design](/docs/developer-docs/modules/web/account-ui/) — the sibling that established the generated-hooks tier; [billing design](/docs/developer-docs/modules/capabilities/billing/) — the module whose read surface this family renders
- User guide: [billing-ui module](/docs/user-guide/modules/web/billing-ui/), [billing module](/docs/user-guide/modules/capabilities/billing/), [building the frontend](/docs/user-guide/domains/frontend-building/)
