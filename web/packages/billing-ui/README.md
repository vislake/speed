# @speed/billing-ui

The billing-documents component family of a frontend built on speed:
the read surface of the module's channel-agnostic Invoice model,
rendered through the generated billing read operations. One section
composes a host's billing page -- `InvoicesSection`, the caller's
tenant's invoices newest first, each row expandable into the single
document's detail. It is deliberately a section, not a routed screen:
the page that owns it, the headings above it and every surface that
lives outside this package (subscription management, payment) are host
content.

The tier is the generated-hooks tier of the api-sdk contract: reads go
through the @tanstack/react-query hooks generated into
`@speed/api-sdk` (`useBillingListInvoices`,
`useBillingGetInvoice`) over the host's QueryClient -- the one
provider a billing-ui host supplies beyond the theme tree. The billing
operations carry no tenant concept: whose invoices a read returns is
decided by the caller's access token, so the section takes no props
and no tenant value is ever a prop or a header. Nothing here reads
storage, navigates or touches the network directly. Every built-in
string renders from the bilingual `billing-ui` namespace registered
through `@speed/i18n`, and the empty and error states render through
`ui-kit`'s `EmptyState`, the same discipline the other packages
established.

## What ships

| Module | Exports |
|---|---|
| `InvoicesSection.tsx` | `InvoicesSection` |
| `resources.ts` | `BILLING_UI_NAMESPACE`, `billingUiResources` |

Everything else (`src/internal/`) is shared plumbing -- the
code-to-text error resolver, the `InlineError` banner, the Intl
invoice formatters and the translation hook -- and is deliberately not
exported.

## Quick start

The billing page presumes a signed-in viewer: the host's sign-in
surface (the `auth-ui` family over an `auth-core` session) ran first,
and the memory access-token store holds the token whose tenant the
reads resolve. The bootstrap has the three parts of every host of this
package -- the bilingual i18n instance with both namespaces a rendered
surface can read, the client bound into the api-sdk runtime over one
store, and the QueryClient (react-query retries and caching policy are
the host's own) -- plus the host's billing page composing the section.

```tsx
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  BILLING_UI_NAMESPACE,
  billingUiResources,
  InvoicesSection,
} from '@speed/billing-ui'

// 1. The store holds the bearer token of the signed-in viewer: the
//    host's sign-in flow planted it before this page mounted. The
//    client reads it on every send -- tenant context travels inside
//    the token, never in a header or prop. No refresh seam is bound
//    here; a host whose session layer refreshes passes its refresh
//    through refreshAccessToken, exactly as the account-ui quick
//    start shows.
const store = createMemoryAccessTokenStore() // token planted by sign-in
bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    fetch: fetchImpl, // the host's fetch implementation
    accessTokenStore: store,
  }),
)

// 2. The bilingual instance, both namespaces registered exactly once:
//    the billing-ui namespace for this package's strings, and the
//    ui-kit namespace because the empty and error states speak
//    ui-kit-namespace keys.
const i18n = createI18n({
  supportedLanguages: ['zh-CN', 'en-US'],
  defaultLanguage: 'zh-CN',
  storage: null,
  urlParameterName: null,
  navigatorLanguages: [],
})
registerNamespace(i18n, BILLING_UI_NAMESPACE, billingUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)

// 3. The surface reads through the generated react-query hooks, so
//    the page renders under the host's QueryClient.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 0 } },
})

export function App() {
  return (
    <I18nextProvider i18n={i18n}>
      <AppThemeProvider i18n={i18n}>
        <QueryClientProvider client={queryClient}>
          <main>
            <h1>Billing</h1>
            <InvoicesSection />
          </main>
        </QueryClientProvider>
      </AppThemeProvider>
    </I18nextProvider>
  )
}
```

`registerNamespace` validates before it mutates (identical leaf key
sets across both languages) and must run exactly once per instance,
same as every other speed namespace. This exact composition is
compiled and executed by the package suite
(`src/usage-example.test.tsx`) over a real `@speed/api-client`: its
fetch stand-in answers with genuine `Response` objects, and the
journey mounts the page and walks the surface's story -- the
newest-first list rendering the three lifecycle states, one July
document expanded into its detail (whose single-document read answers
an invoice that settled after the list was fetched, so the document
reads paid with a last-update row while the collapsed row keeps the
list's open snapshot), the row collapsed again, and a final
`switchLanguage` leg re-rendering the same page in the other
supported language. The whole exchange -- two requests -- is pinned in
order at the end, with each request's authorization header asserted.
The documented usage cannot drift from the API.

## InvoicesSection

The billing-documents surface: every invoice the billing module holds
for the caller's tenant, newest page of the server's answer first,
read through the generated list hook with the page size frozen at 50
-- within the spec's 1..100 limit, sized to render the recent
documents without scrolling machinery, and deliberately not paginated:
the billing read surface serves one newest-first window (a
keyset-paginated read over a whole history is not part of the spec).
Rows show the document's status through the lifecycle's closed
vocabulary -- open (awaiting payment), paid (settled in full), void
(canceled before payment) -- as a chip whose label is the status's own
text (color never carries the meaning alone), the billing cycle it
covers, the amount in its own currency, and the issue date. Amounts,
dates and periods render through `Intl` in the surface's current
language, never hand-formatted -- and in the UTC calendar the server
issued the document against, so an invoice issued on July 31 stays
July 31 for a viewer in any timezone. A cycle whose bounds lie in one
calendar month reads as that month ("July 2026"); bounds that cross a
month render as the full date range.

Rows are expandable: the row-end control (a labelled button carrying
`aria-expanded`) mounts the row's detail, a named region that
re-reads the single document through `billing_getInvoice` and renders
the answer as a document -- status, amount, both cycle bounds, issue
and last-update times (the latter only when the row was touched after
issue), and the subscription and invoice ids. The list row stays the
list query's snapshot; the document is the get's own fresh answer, so
a status settled after the list was fetched shows its current value in
the document. The region announces its loading, renders a refused
read as the code-level banner with a retry, and collapses again with
the row.

The section takes no props: whose invoices these are, and the right to
read them, come from the caller's bound client and its access token.
An unresolved load -- the first load in flight, or parked by
react-query's default `networkMode: 'online'` while the device is
offline -- keeps the loading branch, header included. Only a settled
query leaves it: an answer listing zero invoices hides the header and
renders the ui-kit `EmptyState` empty variant, while a load that
failed renders the error variant with a retry button. In every settled
state the `EmptyState` title stands in for the hidden `h2` header at
its own level, so the heading order never skips a level and the
no-invoices text is only ever asserted for an answer that genuinely
listed none.

## Text and i18n

All built-in strings live in the bilingual `billing-ui` namespace
(`src/locales/zh-CN.json` and `en-US.json`, identical leaf key sets,
enforced by registration and by `tools/check_i18n_keys.py` in CI):

| Section | Purpose |
|---|---|
| `invoices.*` | the invoices surface: title, the loading announcement, one label per lifecycle status (`invoices.status.<status>`), the issued-date template, the row expand/collapse aria-labels and the detail region's name (each interpolating the row's cycle label), the document field labels, empty and error states |
| `errors.*` | code-to-text answers (see below) |

Hosts reword the kit by registering their own identical-key bundle
pair under `BILLING_UI_NAMESPACE` at bootstrap -- never by editing
component text. Two built-in strings a rendered surface can show are
not this package's: the `EmptyState`'s own texts speak
`ui-kit`-namespace keys, which is why the quick start registers both
namespaces. `MUI` components themselves carry no speed text.

### Error text: the reachable-code whitelist

Every failure path of the family resolves its failure to one error
code and renders it through the same `role="alert"` banner
(`InlineError`), never per-field error prose. The resolver maps
exactly these codes to their `errors.*` keys -- the reachable answers
of the billing read surface, plus the transport codes of the
`@speed/api-client` contract:

| Area | Codes with dedicated text |
|---|---|
| Billing (the document read's own 404: an id that names no invoice of the caller's tenant) | `billing.invoice_not_found` |
| Session lifecycle (a protected read answers with these when the caller's session dies mid-flight; the shared family every protected speed surface whitelists) | `authn.session_not_found`, `authn.session_revoked`, `authn.token_expired`, `authn.refresh_token_invalid`, `authn.refresh_token_reused` |
| Transport (the api-client contract) | `client.network`, `client.timeout`, `client.protocol` |

Anything outside the whitelist -- the billing 400 pair
(`billing.invalid_limit`, `billing.invalid_request`), unreachable from
this package by construction because the list hook always sends the
frozen in-range limit; the 500 envelopes (`billing.internal_error`);
a future billing code; a `client.http.<status>` answer; a
non-`ApiError` throw -- renders the `errors.unknown` fallback, so the
bundle can never show a raw key, and a missing translation never leaks
another language's text or an English fallback. The family's
classifier (`errorCodeOf`) collapses non-`ApiError`-shaped throws to a
code that is deliberately not whitelisted, so the fallback is where
they land: an operation that throws at all always has a code to show.
Wording policy: every code whose answer text this package shares with
the sign-in family (the five session-lifecycle codes, the three
`client.*` transport codes and the `errors.unknown` fallback) is a
verbatim copy of the auth-ui bundle's text, so the same server answer
reads the same on every surface. The whitelist-to-bundle pairing is
pinned in both languages by the suite
(`src/internal/error-text.test.ts`).

## Accessibility

Component tests run axe over the rendered document and fail on any
violation, with the same two recorded exceptions shared with
`ui-kit`/`auth-ui`/`account-ui`: `color-contrast` is disabled because
jsdom computes no layout or color (a contrast result there is neither
trustworthy nor actionable -- the theme owns contrast and is verified
browser-side), and `region` is disabled because the units under test
are components, not full app pages. The interaction semantics are
asserted by the tests rather than left to axe: the pending list
announces through a `role="status"` skeleton container (`aria-busy`, a
per-surface `aria-label`, and the decorative skeletons `aria-hidden`),
the document region's loading announces through its own
`role="status"` line, every failure renders in one `role="alert"`, and
the row's expand control is a labelled icon button carrying
`aria-expanded` and `aria-controls` pointing at the named detail
region it toggles. The empty and error states of the surface hide the
section header and pass `EmptyState` `headingLevel="h2"` so its title
stands in at the hidden header's own level rather than skipping to
`EmptyState`'s `h6` default; component tests scan an isolated tree
with a preceding `h1` supplied by the harness, so the level chain
`h1` -> `h2` never skips.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`
(`InvoicesSection.test.tsx`, plus the internal `error-text.test.ts`
and `invoice-format.test.ts`), shared helpers only in `test-utils/`.
The vitest config aliases the `@speed/*` specifiers onto the sibling
packages' `src` entries, so tests run against live sources -- no test
file imports another package's `dist/`. The harness carries the one
provider a host of this package supplies beyond the theme tree, the
`QueryClientProvider`:

- `render.tsx` -- `renderWithProviders` mounts a unit under the tree a
  real host builds (`I18nextProvider` around `AppThemeProvider` around
  `QueryClientProvider`), with a fresh bilingual instance per call (the
  double-registration guard never fires across tests) and both
  namespaces a rendered surface can read registered;
  `createBillingUiI18n` and `createTestQueryClient` are the shared
  factories, the test client retrying nothing so an operation the test
  scripts to fail surfaces on the first attempt.
- `real-client.ts` -- `makeRealClientRig` builds the journey rig: a
  real `@speed/api-client` `createClient` whose fetch stand-in answers
  from a script with genuine `Response` objects, the memory
  access-token store, bound through the same `bindRequestFn` seam a
  host's real client binds -- recording every request's method, path,
  query string and authorization header. The billing surface has no
  session layer of its own, so the rig's store starts empty and a test
  plants the bearer token the reads ride; no refresh seam is bound, so
  a 401 answer surfaces its own code. This rig is the whole of the
  package's test story: every answer the surface can render --
  including the error codes -- is scriptable as a genuine `Response`
  carrying the API envelope.

`src/usage-example.test.tsx` compiles and executes the Quick start
composition above end to end over the rig (the list window and the one
document read, in a pinned order with authorization headers asserted),
and every component test asserts axe (`expectNoAxeViolations`) and the
bilingual text of its states -- importing the shipped locale files,
never inlining a language literal.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react`, `react-dom` | peer (required, ^18 or ^19) | the host owns the React tree |
| `@mui/material` | peer (required, ^9) | the `Chip`/`Skeleton`/`Button` primitives and the ambient theme |
| `@emotion/react`, `@emotion/styled` | peer (required, ^11) | MUI's own runtime requirements |
| `@tanstack/react-query` | peer (required, ^5) | every read is a generated hook over the host's QueryClient -- the shared-QueryClient contract, the same peer `@speed/api-sdk` declares |
| `@speed/api-sdk` | dependency | the generated billing read hooks every surface reads through -- a runtime dependency, unlike auth-ui's type-only use of the package |
| `@speed/i18n` | dependency | the namespace registration and translation hook every surface renders through |
| `@speed/ui-kit` | dependency | the `EmptyState` every settled empty/error state composes |

`@speed/api-client` stays a devDependency: only the test-utils rig
binds a real client, and no public type of this package references it.
No direct HTTP exists anywhere in `src/` -- every request goes through
the generated operations over the shared seam, and the workspace's
`speed/no-direct-http` rule enforces that (this package is simply
absent from the rule's one whitelist, `packages/api-client`); the
`speed/no-literal-text` rule enforces the namespace discipline over
`src/` the same way it does in every package.

## Known limitations

- **The read surface is the whole surface.** The billing HTTP
  fragment ships two read operations (list, get) and no writes:
  nothing here settles, voids or creates an invoice, and nothing
  invalidates a query after a mutation -- a document whose status
  changed elsewhere converges on the list through the host's own
  refetch policy.
- **One newest-first window, no pagination.** The section reads the
  newest fifty documents and never paginates -- deliberate for a
  read-only billing page, and the page size is pinned to the spec's
  limit range in the component (`INVOICE_LIST_LIMIT`), not
  configurable by prop. The billing read surface serves no keyset
  cursor.
- **Document dates render in the UTC calendar.** The model stores UTC
  instants and the family renders them in UTC, so a document date is
  the same date for every viewer; a viewer who wants their own
  calendar's reading of a session-style timestamp will not find it
  here.
- **A row whose period does not parse names itself by its invoice
  id.** A malformed answer never reaches `Intl`; the cycle label
  falls back to the document id, which is always true.
- **The status vocabulary is the spec's closed set.** The three
  lifecycle states have labels; a status value outside the set (a
  future state an answer already carries) renders no chip, never a
  raw value.
- **Whitelisted error text only.** Failure answers outside the
  whitelist render the `errors.unknown` fallback (see the Error text
  section); a host that wants its own copy for a code registers its
  own bundle pair under the namespace.

## Deferrals and recorded decisions

- **No reference-app consumer shell yet.** The package proves itself
  in form at the package level -- `src/usage-example.test.tsx` drives
  the composed family over a real `@speed/api-client` bound through
  the same seam a host binds, with a scripted fetch answering genuine
  `Response` objects. The reference-app web host does not render this
  family, and there is no browser/real-server leg for it yet.
- **Reads deliberately go through generated react-query hooks.** The
  invoice list and document are cacheable shared state keyed by the
  host's QueryClient -- the generated-hooks tier of the api-sdk
  contract, the tier the account surfaces established; the section
  consumes it and never constructs its own client or hand-writes a
  query key.
- **The document read exists because the list is a snapshot.** Rows
  and document are the same `BillingInvoice` shape; expanding a row
  re-reads the single document so a status settled after the list was
  fetched shows its current value, and the document renders fields the
  row does not (both cycle bounds, last-update time, the two ids).
- **Storybook / browser-side visual verification**: no preview-harness
  round exists yet, same deferral `ui-kit`, `layout-kit`, `auth-ui`
  and `account-ui` carry; `color-contrast` stays axe-disabled for the
  same jsdom reason and is verified browser-side in a later round.

## Development

From `web/packages/billing-ui`: `pnpm lint`, `pnpm typecheck`, `pnpm
test`, `pnpm build`. The build compiles `@speed/api-sdk`,
`@speed/ui-kit` and `@speed/i18n` first (their sources are what the
aliases and the published `.d.ts` files reference) and then emits this
package's `dist/`; `pnpm build` from the `web/` root runs the same for
every package.
