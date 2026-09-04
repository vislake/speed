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
  the documented composition changes.

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
a retry clears, an identical later failure re-announces) — and its a11y
assertions run through the same `expectNoAxeViolations` used everywhere
here. The usage example's upload panel drives the documented host
composition — host-owned queue, host AbortControllers, host transport
— over a scripted fetch answering genuine `Response` objects; scripted
transports and their fixture URLs live in test files only, and the
workspace's `no-direct-http` rule keeps every fetch out of `src`.

## Deferrals (recorded, do not re-open silently)

- **Validation from generated types** (zod-from-generated-types) is the
  form milestone's follow-up; the validation-error contract is the seam
  it plugs into.
- **Error-code mapping** (which namespace turns which backend code into
  text) is a later resolver round; verbatim passthrough is the current
  contract.
- **Storybook**: no preview harness round exists yet; components are
  covered by jsdom tests + axe, and color-contrast verification awaits
  a browser-side visual round.
- **The `no-literal-text` rule** catches the direct literal routes but
  not indirect ones (ternary branches, literals crossing component
  boundaries) — documented partial enforcement; hosts own their
  literals.
- **The storage frontend leg** — storage operations generated into
  `@speed/api-sdk` from the `go/storage` OpenAPI fragment, the natural
  transport for a host's upload code — waits for the merged document
  to gain that fragment: orval runs over the merged document only
  (notes and authn today), and the round that first extends it is the
  M1 `org-web` round joining org's fragment, storage's riding that
  same regeneration. Until then the wire contract's authority is
  `go/storage/api/openapi.yaml` itself; hosts run their own transport
  and this package ships none. The deferral is recorded three ways so
  no single doc owns the claim: `go/storage/AGENTS.md`'s deferral
  list, the Taskfile `api:gen` header comment, and this entry.
