# AGENTS.md — @speed/i18n

## What this package is

The single i18n entry point for web packages: instance creation with
negotiated start language (`createI18n`), the manual language switch
(`switchLanguage`), per-namespace resource registration
(`registerNamespace`), and the MUI localization bridge (`./mui-locale`).
It wraps react-i18next/i18next and adds the platform's discipline on top:
pinned supported-language sets, per-language coverage, key-set parity, and
missing keys that warn and render as the key -- never another language's
text. That discipline mirrors `go/pkgcore/i18n` (see the Go module's
catalog) and is non-negotiable here.

## Non-negotiable rules

- **A missing key never falls back across languages.** This is the design
  invariant; the instance options that enforce it (`fallbackLng: false`,
  `load: 'currentOnly'`, `saveMissing: true` with the handler always
  installed) are pinned by tests. Do not "helpfully" turn on fallbackLng,
  and do not register resources through raw `addResourceBundle` in
  consuming packages -- `registerNamespace` exists so validation runs.
- **Registration is validated before it mutates, and covers the whole
  supported set with identical key sets.** Parity and coverage errors must
  keep listing actionable details (language tags, leaf paths).
- **No CJK in sources or tests.** Fixtures live under
  `test-utils/locales/<namespace>/<lang>.json`; every language-text
  assertion imports those fixtures. Never inline a language literal, and
  keep fixture languages' key sets identical.
- **The supported-language set is the contract.** Adding a language
  touches: `DEFAULT_SUPPORTED_LANGUAGES`/`DEFAULT_LANGUAGE` choices,
  the `muiLocaleFor` mapping (MUI localization must exist), fixture pairs
  for every namespace, and the parity/coverage tests. A language ships
  only when every namespace can cover it -- partial support is refused at
  registration by design.
- **Do not widen the DOM dependence.** All browser reads (location,
  localStorage, navigator) are guarded and injectable; tests run
  deterministically in Node. New browser touches go through the same
  inject-or-guard pattern.
- **Instance options are internal to createI18n.** i18next's option
  surface is an implementation detail here (v26 internals that shaped the
  code: `initAsync` defaults true and must be false for synchronous
  readiness; supportedLngs gains an internal `cimode` entry that
  readSupportedLanguages filters; `missingKeyHandler` only dispatches with
  `saveMissing`). Consuming code never sets these; when the wrapper
  version moves, re-verify them against the runtime before changing tests.

## React bindings

The main entry re-exports react-i18next's `I18nextProvider` and
`useTranslation` (plus the `UseTranslationResponse` type). Component
packages and hosts import them from here -- never from `react-i18next`
directly -- so lockstep single-version shipping can prove one
react-i18next copy, which is what makes `useTranslation` inside shipped
components safe. The exact-export test (`index.test.ts`) pins this
surface; do not grow it with new react-i18next names without the same
AGENTS/README/test update in one commit.

## Adding a namespace (consuming-package side)

1. Ship `zh-CN` + `en-US` JSON under the package's
   `locales/`-named directory with **identical nested structure**.
2. Register once at host bootstrap: `registerNamespace(i18n, name, {…})`.
3. Render via `useTranslation(name)` / `t(...)`; never hardcode
   user-facing text anywhere (repo rule).
4. Missing-key warnings in dev mean a real gap: fix the resources, do not
   silence the handler.

## Changing the public API

Exports are frozen by convention (lockstep versioning repo-wide). A change
to `createI18n` options, `registerNamespace` semantics, or the entry-point
surface updates: this AGENTS.md, the README (negotiation chain, error
index), `index.test.ts`'s exact-export list, and the feature tests in one
commit, with rationale.
