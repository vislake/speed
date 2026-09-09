# AGENTS.md for `@speed/billing-ui`

The billing-documents component family of a speed frontend, the read
surface of the billing module's channel-agnostic Invoice model:
`InvoicesSection` renders the caller's tenant's invoices, newest page
of the server's answer first, each row expandable into a document
region that re-reads the single invoice. The package sits on the
generated-hooks tier of the api-sdk contract: reads go through the
@tanstack/react-query hooks generated into `@speed/api-sdk`
(`useBillingListInvoices`, `useBillingGetInvoice`) over the host's
QueryClient, and nothing here consumes a session, reads storage,
navigates or touches HTTP.

## What this package is

- **A read surface over a host-owned QueryClient, with no props.** The
  section takes no props: whose invoices are shown comes from the
  caller's bound client and its access token. The billing operations
  carry no tenant concept -- no tenant header, no tenant parameter,
  and never a tenant prop -- because the tenant travels inside the
  access token.
- **The generated-hooks tier, consumed.** The invoice list and the
  single document are cacheable shared state keyed by the host's
  QueryClient (the account surfaces established the tier); the section
  consumes the generated hooks and never constructs its own
  QueryClient, never hand-writes a query key, and never invalidates --
  the billing HTTP surface ships no writes, so there is nothing here
  whose success could stale a query.
- **Every built-in string is bilingual and bundled.** All text renders
  from the `billing-ui` namespace (`BILLING_UI_NAMESPACE`, one
  `zh-CN.json` and one `en-US.json` under `src/locales/`, identical
  leaf keys per language), registered by the host exactly once through
  `@speed/i18n` alongside the `ui-kit` namespace -- the `EmptyState`
  texts are ui-kit-namespace keys.
- **The error surface is a code whitelist.** Every failure path
  resolves to one error code rendered in one `role="alert"` banner;
  codes outside the whitelist render `errors.unknown`, never a raw
  key. The whitelist is the reachable set of the two invoice reads
  this package performs: the billing document read's own 404
  (`billing.invoice_not_found`), the session-lifecycle family every
  protected speed surface whitelists,
  and the three `client.*` transport codes. The 400 pair and 500
  envelopes of the billing fragment are deliberately outside it --
  unreachable by construction from the fixed-parameter reads, or
  internal-error answers that render the unknown fallback like
  everywhere else.

## Rules that are load-bearing here

1. **The package renders exactly the operations that exist.** The
   billing fragment ships four reads and no writes — the two credit
   reads (`billing_getCreditBalance`, `billing_listCreditTransactions`)
   and the two invoice reads this package renders — and nothing here
   pretends to settle, void or create an invoice, and no payment
   surface exists anywhere in this package. A row and its document are
   the same `BillingInvoice` shape -- the detail read exists because
   the list row is the list query's snapshot and the document is the
   get's own fresh answer (a status settled after the list was fetched
   shows its current value in the document).
2. **No text outside the namespace, no key set drift, and no raw
   server vocabulary.** User-facing strings are added to both locale
   files in the same commit under their section; the
   `speed/no-literal-text` rule refuses inline text in `src/`. The
   status vocabulary is the spec's closed set of three, mapped through
   an exhaustive `Record`; a value outside the set renders no chip,
   never a raw value. A host rewording a string re-registers the whole
   namespace with an identical-key bundle pair -- never by editing
   component text here.
3. **New reachable error codes join the whitelist and both locale
   files in one commit.** `src/internal/error-text.ts`'s
   `ERROR_TEXT_CODES` is the reachable-subset whitelist this family
   renders, with `errors.unknown` for everything else. When a code
   becomes reachable through a path of this family, add it there and
   to both `errors.*` sections at once. Wording policy: codes whose
   answer text is shared with the sign-in family (the five
   session-lifecycle codes, the three `client.*` transport codes and
   the `errors.unknown` fallback) are verbatim copies of the auth-ui
   bundle's text -- never divergent copy for the same server answer --
   and the verbatim pin in `error-text.test.ts` imports the auth-ui
   bundles as test data (JSON data crosses no package boundary the
   same-tier rule draws). Deliberately not whitelisted:
   `billing.invalid_limit`, `billing.invalid_request`
   (unreachable by construction), `client.http.*` and `client.unknown`
   (the classifier's landing slot for non-`ApiError` throws) -- they
   render `errors.unknown` by design.
4. **No direct HTTP in `src/`, no react-query outside the host's
   client, no hand-formatted dates or money.** Every request flows
   through generated operations over the `bindRequestFn` seam; this
   package is not on the `speed/no-direct-http` whitelist. Money goes
   through `Intl.NumberFormat` with the invoice's own currency and
   dates through `Intl.DateTimeFormat` in the UTC calendar the server
   issued the document against (a document date is a fact of the
   record, not of the viewer's clock -- see
   `internal/invoice-format.ts`); a value Intl cannot format never
   reaches it. Tests bind their doubles through the same seam
   (`bindRequestFn` from `@speed/api-sdk/runtime`) -- never by mocking
   a module or by importing another package's `dist/`.
5. **The public surface is the `index.ts` exports; dependencies follow
   public types.** Helpers live in `src/internal/` and are deliberately
   not exported. Every read is a generated hook of `@speed/api-sdk`
   that executes here, and the namespace pair comes from
   `@speed/i18n`, so both are declared **dependencies**; `@speed/ui-kit`
   is a dependency because `EmptyState` composes into the settled
   empty/error states. Consumers resolve the published `.d.ts` under
   pnpm's strict node_modules, and a runtime import demands a
   dependency entry, never a dev one. `@speed/api-client` stays a
   devDependency: no public type references it; only the test-utils
   rig binds a real client.
6. **No session layer is imported.** The sign-in that preceded the
   billing page is the auth-core story, out of this package's scope:
   no `@speed/auth-core` dependency exists here, the store already
   holds the bearer token the reads ride, and a refused request
   surfaces its own code (a host whose session layer refreshes binds
   `refreshAccessToken` on its client, exactly as the account-ui
   quick start shows).
7. **The README's quick start is executed, not aspirational.**
   `src/usage-example.test.tsx` compiles and runs the documented
   composition (real api-client, genuine `Response` objects from a
   scripted fetch, real i18n, the host's QueryClient, two requests in
   a pinned order with authorization headers asserted). Any README
   composition change must keep that true; any component behaviour
   change ships with its README section updated in the same commit.
8. **The section stays a section; nothing navigates, nothing signs
   anyone in or out.** Expansion is row-local interaction state. Empty
   and failure states hide the section header and render a ui-kit
   `EmptyState` with `headingLevel="h2"` (the hidden header's own
   level), so heading order never skips a level: this depends on the
   caller supplying the page context correctly -- the component suites
   render under a real `h1`, the level the section's own `h2` header
   continues.

## Public surface

Everything in `src/index.ts` is public: the surface (`InvoicesSection`)
and the resource pair (`BILLING_UI_NAMESPACE`, `billingUiResources`).
Prop tables and behaviour live in the README; `tsc -p tsconfig.json`
type-checks every consumer-visible signature.

## Testing

Vitest + jsdom, one test file per source file under `src/`, shared
helpers only in `test-utils/`; the vitest config aliases the `@speed/*`
specifiers onto sibling packages' `src` entries, so no test imports
another package's `dist/`. One helper layer set, carrying the
`QueryClientProvider` (every read is a react-query hook):

- `test-utils/render.tsx` — `renderWithProviders` mounts a unit under
  the real host tree (`I18nextProvider` around `AppThemeProvider`
  around `QueryClientProvider`), with a fresh bilingual instance per
  call registering both namespaces a rendered surface can read, and
  `createTestQueryClient` retrying nothing so an operation the test
  scripts to fail surfaces on the first attempt.
- `test-utils/real-client.ts` — `makeRealClientRig`, the whole of this
  package's test story: a real `@speed/api-client` over a scripted
  fetch answering genuine `Response` objects, the memory access-token
  store (a test plants the bearer token the reads ride), bound through
  `bindRequestFn`, recording every request's method, path, query and
  authorization header. Every answer a surface can render -- error
  codes included -- is scriptable as a genuine `Response` carrying the
  API envelope.

`src/usage-example.test.tsx` executes the README quick start end to
end over the rig (list -> the three-status vocabulary -> one document
read -> collapse -> a language switch, two requests in a pinned
order); every component test asserts axe (`expectNoAxeViolations`) and
the bilingual text of its states. The whitelist-to-bundle pairing, the
verbatim copies to the auth-ui bundle and the unknown fallback are
pinned in both languages by `src/internal/error-text.test.ts`, and the
Intl formatters' choices by `src/internal/invoice-format.test.ts`.

## Known limitations

Recorded, with the current disposition, in the README's Known
limitations and Deferrals sections. The load-bearing ones for a
contributor: the billing HTTP surface ships no writes, so the family
can never invalidate a query after its own action; the list is one
frozen newest-first window of 50 (no keyset cursor exists in the
spec); document dates render in the UTC calendar; the status and error
vocabularies render only through their whitelists; and the runtime
consumer proof is discharged in form at the package level -- no
reference-app consumer shell renders this family, and no browser +
real-server leg exists for it.
