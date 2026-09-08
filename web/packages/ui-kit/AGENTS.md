# AGENTS.md — @speed/ui-kit

## What this package is

The first DOM-rendering package: the `createAppTheme` theme factory that
maps the merged token tree onto an MUI v9 theme, `AppThemeProvider`
(theme + MUI locale linkage + CssBaseline), and seven controlled core
components (`PageHeader`, `EmptyState`, `ConfirmDialog`, `FormField`,
`FormLayout`, `DataTable`, `FileUploader`) that render only the state
hosts give them. `FileUploader` is no carve-out: the queue renders from
host-owned `rows` props, every pick, cancel, retry and remove reports up
through a callback (`onSelectFiles` / `onCancel` / `onRetry` /
`onRemove`), and the upload transport — one logical transfer per picked
file, pre-flight validation (size, type, count) and any concurrency
limit included — is the host's own code. That boundary is what keeps
the package free of HTTP: the widget never fetches, never holds a File
past the event handler that reported it, and keeps no record of rows
that are not in the current `rows` prop. Built-in user-facing strings
live in the bilingual `ui-kit` namespace;
the repo's text discipline (both languages, identical key sets, nothing
inline) is enforced over this package's `src` by the workspace's own
`speed/no-literal-text` ESLint rule. The public surface is `src/index.ts`
plus `resources.ts`'s exports; everything else under `src/internal/` is
plumbing and deliberately not exported.

## Non-negotiable rules

- **Text renders from the `ui-kit` namespace or from host props — never
  inline in code.** The `speed/no-literal-text` rule (workspace config at
  `web/eslint.config.mjs`, implementation and rule tests in
  `web/eslint-rules/`) enforces this over `src/`; package tests and
  `test-utils/` are exempt by config because fixture strings are data.
  Do not weaken the config for a new component — route its text through
  `useUiKitTranslation` instead.
- **The two locale files stay a bilingual pair.** A new built-in string
  adds one key to both `src/locales/zh-CN.json` and `en-US.json` in the
  same commit, with identical nested structure. Registration rejects
  drift at runtime (`registerNamespace` in the host and in
  `renderWithProviders`), and `tools/check_i18n_keys.py` checks the raw
  files in CI. Hosts reword the kit by registering their own identical-
  key bundle pair under `UI_KIT_NAMESPACE` at bootstrap — never by
  editing component text.
- **The namespace registers once, at host bootstrap — never inside the
  package.** Components and `AppThemeProvider` only consume the
  registered namespace; registering (or rendering `I18nextProvider`)
  from inside the package would make the double-registration guard and
  the provider tree a host's problem instead of a host's choice. The
  package never imports `@speed/i18n`'s `registerNamespace` outside
  test-utils.
- **Components are fully controlled.** No fetch, no data mutation, no
  business state, no implicit sorting, slicing or filtering of `rows` —
  every knob is a prop the host owns and a callback the component
  fires. Interaction-only state (a confirm's arming, a tooltip's open)
  is the allowed exception and must be provably interaction-local.
  `FileUploader` keeps that contract too: the queue is the host's
  `rows` state, interactions report up through its callbacks, and the
  upload transport is host code — never package code.
- **Host-content props are fallbacks over namespace defaults, or pure
  host content — never required translations.** `EmptyState` /
  `ConfirmDialog` ship namespace defaults with overridable
  `title`/`message`/label props; column headers and cell renderers are
  entirely the host's translation surface. `PageHeader`'s visible
  content (title, description, crumb labels, actions) is host content
  too; its only built-in strings are the breadcrumb nav landmark's
  accessible name and the collapse-expand button label
  (`pageHeader.*`), shipped through the namespace like any other. No
  component ever calls `t()` on a host-provided string.
- **Error messages follow the validation-error contract.** A message
  that is a ui-kit-namespace key renders as its translation; anything
  else renders verbatim. The form family resolves keys in the ui-kit
  namespace only and never guesses another namespace's codes —
  `src/internal/validation-error.ts` is the single resolution point.
  `REQUIRED_ERROR_KEY` is the exported key for hosts building their own
  rules.
- **Do not duplicate MUI chrome.** MUI controls already render labels,
  helper text and aria wiring; the form family's "uniform error display"
  is the render-state contract plus FormLayout's skeleton, not a second
  renderer. When a control is missing from MUI's v9 surface, build on
  its primitives — do not fork them.
- **Framework peers stay peers.** `react`, `react-dom`, `@mui/material`,
  `@emotion/*` and `react-hook-form` are peer (required) dependencies —
  single copies in the host tree are what make theme/context/type
  identity work; the package never depends on them directly. New
  dependencies on framework-adjacent packages need the single-copy
  argument stated in the same commit. `@speed/tokens` and `@speed/i18n`
  are regular dependencies; a new @speed consumer joins them.
- **Accessibility is asserted, with recorded exceptions.** Component
  tests run axe through `test-utils/axe.ts`; `color-contrast` and
  `region` are disabled with the rationale recorded there (jsdom cannot
  compute contrast — contrast lives in the theme tokens; landmark
  containment is a page concern). Do not disable further rules without
  the same kind of recorded rationale. Decorative icons stay
  `aria-hidden`; semantics come from real elements (`h1` headers, real
  table structure, dialog labelling).
- **The public API is frozen by convention.** Lockstep versioning makes
  an exported-signature change a breaking release; extend the surface
  only intentionally. A public change ships, in one commit: the code,
  its tests, this AGENTS.md, the README (contract prose and the
  resource table when keys change), and the compiled usage example when
  the documented composition changes. `FormLayout`'s `columns?: 1 | 2`
  is the template for how an additive prop stays additive: it defaults
  to `1`, exactly the unconditional single-column flow, so every
  existing consumer that omits it is byte-for-byte unaffected; only
  passing `columns={2}` opts into the new CSS Grid flow.
  `DataTableColumn`'s `priority?: 'high' | 'medium' | 'low'` follows
  the identical template: undefined by default, exactly the
  always-visible column, so an existing consumer that sets no column's
  priority is byte-for-byte unaffected; only a column that opts in
  gets the breakpoint-keyed hide/show.

## Testing

Unit tests are vitest + jsdom, one file per source file under `src/`,
shared helpers only in `test-utils/` (`renderWithProviders` mounts the
unit under the real host tree — fresh i18n instance per call so the
double-registration guard never fires across tests — and
`expectNoAxeViolations` runs axe; always `await` it: axe-core refuses
concurrent runs). Bilingual assertions import the shipped bundles
(`../locales/zh-CN.json`, `en-US.json`) — never inline a language
literal. `src/usage-example.test.tsx` compiles and executes the README's
Quick-start composition, so the documented usage cannot drift from the
API; when the README composition changes, that file changes with it.
The `FileUploader` suite reaches every interaction state through
host-owned rows and recorded callbacks — picks and drops reporting the
files in order, cancel/retry/remove keyed by row id, each row status
rendered as given, progress folding (indeterminate when absent,
determinate clamped when present, NaN and out-of-range fractions
included) and the settle announcements of the live region (mount quiet,
rows appended already settled announce without an uploading phase, a
retry clears so an identical later failure re-announces, and a second
same-name row reaching the same outcome as a standing announcement
clears and re-announces on the next tick) — and its a11y assertions run
through the same `expectNoAxeViolations` used everywhere here. The usage example's upload panel drives the documented host
composition — host-owned queue, host AbortControllers, host transport
— over a scripted fetch answering genuine `Response` objects; scripted
transports and their fixture URLs live in test files only, and the
workspace's `no-direct-http` rule keeps every fetch out of `src`.

**Responsive sx assertions**: a plain, non-breakpoint-gated sx value
(`flexWrap: 'wrap'`, a fixed
`width`) computes fine through jsdom's `getComputedStyle`, so those
stay ordinary `toHaveStyle` assertions (`FileUploader`'s row action
groups, `PageHeader`'s title/actions row). A breakpoint-keyed sx value
(`FormLayout`'s `gridTemplateColumns` under `columns={2}`) compiles to
a base rule plus an `@media` rule that jsdom cannot evaluate — there is
no real viewport for either side to be "active" at — so those assert
against `test-utils/emitted-css.ts`'s `emittedStyleText()` instead: the
actual generated CSS text, read across every `<style>` tag in the
document. Both forms are property/snapshot proofs that the intended
declaration was wired into the render; neither proves a layout looks
correct at a real viewport width, which this package's jsdom suite
cannot render at all (the README's Deferrals record the same
browser-side visual gap).

**DataTable column priority** reuses the same
`emittedStyleText()` technique for its own breakpoint-keyed `display`
value (see the README's Column priority subsection): one test per tier
asserts the tier's exact `@media (min-width:...)` breakpoint and the
`table-cell` restore value both appear in the generated CSS, plus a
separate test proving a prioritized column stays a genuine DOM column
rather than being conditionally rendered away (which the loading/empty
rows' `colSpan` depends on). No other `sx` in this component is
breakpoint-keyed, so those generated-CSS strings can only originate
from this feature — the same non-collision reasoning `FormLayout`'s
own test relies on. As with every `emittedStyleText()` assertion in
this package, this proves the right declarations were wired in, not
that a real viewport actually hides or shows the column.

## Deferrals (recorded, do not re-open silently)

- **Validation from generated types** (zod-from-generated-types) is
  not implemented; the validation-error contract is the seam it would
  plug into.
- **Error-code mapping** (which namespace turns which backend code into
  text) is not implemented; verbatim passthrough is the contract.
- **Storybook**: no preview harness exists; components are covered by
  jsdom tests + axe, and color-contrast is verified against the theme
  values, never a rendered browser.
- **The `no-literal-text` rule** catches the direct literal routes but
  not indirect ones (ternary branches, literals crossing component
  boundaries) — documented partial enforcement; hosts own their
  literals.
- **The storage frontend leg** — storage operations generated into
  `@speed/api-sdk` from the `go/storage` OpenAPI fragment, the natural
  transport for a host's upload code — is not implemented: orval runs
  over the merged document only, which carries no storage fragment, so
  no generated storage operation exists. The wire contract's authority
  is `go/storage/api/openapi.yaml` itself; hosts run their own
  transport and this package ships none. The deferral is recorded in
  `go/storage/AGENTS.md`'s deferral list and the Taskfile `api:gen`
  header comment as well, so no single doc owns the claim.
