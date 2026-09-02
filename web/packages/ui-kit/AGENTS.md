# AGENTS.md — @speed/ui-kit

## What this package is

The first DOM-rendering package: the `createAppTheme` theme factory that
maps the merged token tree onto an MUI v9 theme, `AppThemeProvider`
(theme + MUI locale linkage + CssBaseline), and six controlled core
components (`PageHeader`, `EmptyState`, `ConfirmDialog`, `FormField`,
`FormLayout`, `DataTable`) that render only the state hosts give them.
Built-in user-facing strings live in the bilingual `ui-kit` namespace;
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
- **Host-content props are fallbacks over namespace defaults, or pure
  host content — never required translations.** `EmptyState` /
  `ConfirmDialog` ship namespace defaults with overridable
  `title`/`message`/label props; `PageHeader`, column headers and cell
  renderers are entirely the host's translation surface. No component
  ever calls `t()` on a host-provided string.
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
