---
title: billing-ui
weight: 12
description: "The billing-documents read surface — one InvoicesSection rendering the caller's tenant invoices newest first, each expandable into the single document's detail."
---

# billing-ui

`@speed/billing-ui` is the billing-documents component family of a
frontend built on speed: the read surface of the backend
[billing](/docs/user-guide/modules/capabilities/billing/) module's
channel-agnostic `Invoice` model, rendered through the generated
billing read operations. One section — `InvoicesSection` — composes a
host's billing page: the caller's tenant's invoices newest first, each
row expandable into the single document's detail.

## What it is for

The package ships `InvoicesSection` plus the `BILLING_UI_NAMESPACE` /
`billingUiResources` bundle pair. The section is deliberately a
**section, not a routed screen**: the page that owns it, the headings
above it and every surface outside this package (subscription
management, payment) are host content.

The tier is the generated-hooks tier of the api-sdk contract: reads go
through the `@tanstack/react-query` hooks generated into
`@speed/api-sdk` (`useBillingListInvoices`, `useBillingGetInvoice`)
over the host's QueryClient. The billing operations carry no tenant
concept — whose invoices a read returns is decided by the caller's
access token — so **the section takes no props**, and no tenant value
is ever a prop or a header. There is no session prop and no session
operation either: the surface is read-only by spec, the billing HTTP
fragment shipping exactly the two read operations. Nothing here reads
storage, navigates or touches the network directly; built-in strings
render from the bilingual `billing-ui` namespace, and the settled
empty/error states compose ui-kit's `EmptyState`.

## When to choose it

A signed-in tenant-facing app needs to show its billing documents:
the lifecycle states of recent invoices, what each covers and for how
much. The family presumes the host's sign-in ran first and the memory
store holds a live token whose tenant the reads resolve. The fragment
ships no writes, so a host that must settle or void invoices has no
surface here to do it with.

## Wiring

```tsx
const store = createMemoryAccessTokenStore()  // token planted by sign-in
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  // refreshAccessToken: () => session.refresh()  // a session-layer host passes its own
}))
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, BILLING_UI_NAMESPACE, billingUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 0 } } })
// <QueryClientProvider client={queryClient}> around the billing page:
//   <main><h1>Billing</h1><InvoicesSection /></main>
```

React-query retry and caching policy is the host's own. The suite
compiles and executes this composition over a real client whose
scripted fetch answers genuine `Response` objects — two requests
pinned in order, authorization headers asserted — so the documented
usage cannot drift from the API.

## Core concepts and API essentials

`InvoicesSection` renders the newest page of the server's answer, the
page size frozen at 50 (within the spec's 1..100 limit) and never
paginated: the billing read surface serves one newest-first window, and
a keyset cursor over a whole history is not part of the spec. Rows
show:

- **Status through the lifecycle's closed vocabulary** — open
  (awaiting payment), paid (settled in full), void (canceled before
  payment) — as a chip whose label is the status's own text; color
  never carries the meaning alone, and a status value outside the set
  renders no chip, never a raw value.
- **The billing cycle** — within one calendar month it reads as that
  month ("July 2026"); bounds that cross a month render as the full
  date range.
- **The amount in its own currency and the issue date** — formatted
  through `Intl` in the surface's current language, never
  hand-formatted, and rendered in the UTC calendar the server issued
  the document against, so a document date is the same date for every
  viewer.

Rows are expandable: the row-end control mounts a named detail region
that **re-reads the single document** through `useBillingGetInvoice`
and renders the fresh answer — status, amount, both cycle bounds,
issue and last-update times (the latter only when the row was touched
after issue), and the two ids. The list row stays the list query's
snapshot while the document is the get's own answer, so a status
settled after the list was fetched shows its current value in the
document. The region announces its loading, renders a refused read as
the code-level banner with a retry, and collapses with the row.

Unresolved loads (first load in flight, or parked by react-query's
default `networkMode: 'online'` while offline) keep the loading branch,
header included; only a settled query leaves it — zero invoices hide
the header and render the empty `EmptyState`, a failed load the error
variant with a retry button, both at `headingLevel="h2"` so the
heading order never skips a level.

Error text covers the reachable answers only — the document read's own
404 (`billing.invoice_not_found`), the five session-lifecycle codes a
protected read answers with when the session dies mid-flight, and the
three `client.*` transport codes; everything else (the billing 400
pair, unreachable by construction since the list hook always sends
the frozen in-range limit; the 500 envelopes; a future code) renders
the `errors.unknown` fallback. Shared codes are verbatim copies of the
auth-ui bundle's text.

## Boundaries and pitfalls

- **The read surface is the whole surface** — no write exists in the
  billing fragment, so nothing invalidates a query after a mutation; a
  document whose status changed elsewhere converges on the list through
  the host's own refetch policy.
- **One newest-first window, no pagination** — the newest fifty
  documents, page size pinned in the component, not configurable by
  prop.
- **Document dates render in the UTC calendar** — the model stores UTC
  instants and the family renders them in UTC; a viewer wanting their
  own calendar's reading of a timestamp will not find it here.
- **A row whose period does not parse names itself by its invoice
  id** — a malformed answer never reaches `Intl`; the cycle label
  falls back to the document id, which is always true.
- **Whitelisted error text only** — a host that wants its own copy for
  a code registers its own bundle pair under the namespace.

## Related pages

- The frontend layers: [Building the frontend](/docs/user-guide/domains/frontend-building/)
- The backend surface: the [billing](/docs/user-guide/modules/capabilities/billing/) module page and the domain guide [Billing and metering](/docs/user-guide/domains/billing-metering/); error codes under [billing](/docs/user-guide/error-codes/#billing)
- Sibling packages `@speed/api-sdk` (the generated hooks), `@speed/ui-kit` and `@speed/i18n` have their own pages in this group
