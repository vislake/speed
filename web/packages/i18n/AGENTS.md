# AGENTS.md — @speed/i18n

## What this package is

The single i18n entry point for web packages: instance creation with
negotiated start language (`createI18n`), the manual language switch
(`switchLanguage`), per-namespace resource registration
(`registerNamespace`), the instance's own supported-language read
(`readSupportedLanguages`, exported for surfaces that render a language
picker from the instance's real set rather than the package default), and
the MUI localization bridge (`./mui-locale`).
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
- **Empty translations are refused at registration.** An `""` leaf
  renders as silence and never fires the missing-key discipline -- at
  runtime it is indistinguishable from a dropped key. The Go catalog
  refuses empty translations the same way; ship real text or remove the
  key. (Whitespace-only strings are not refused, matching the Go twin's
  exact-empty check.)
- **Plural forms are suffixed leaves that must cover every supported
  language's count categories.** A family (`key_one`, `key_other`, ...)
  counts as ordinary keys for parity -- identical leaf sets across
  languages -- and registration additionally validates, through the same
  `Intl.PluralRules` the renderer resolves counts with, that the family
  carries every category each supported language can select: a count
  whose form is absent would render the raw key. Consequence of the
  parity rule: a form one language selects ships in every language's
  bundle even where it is never selected (zh-CN's `_one` forms exist for
  en-US), and adding a language with richer plural categories (ru-RU's
  few/many) requires extending every existing plural family in every
  bundle. Extend `PLURAL_CATEGORIES`'s sibling validation in register.ts
  only with a test proving the renderer's own resolution agrees.
- **No CJK in sources or tests.** Fixtures live under
  `test-utils/locales/<namespace>/<lang>.json`; every language-text
  assertion imports those fixtures. Never inline a language literal, and
  keep fixture languages' key sets identical.
- **The default language is `en-US`** -- the platform default every locale
  chain in the stack terminates at (backend content chains included), so
  the frontend and the backend land unknown-language requests on the same
  language. `DEFAULT_SUPPORTED_LANGUAGES` still ships the zh-CN/en-US
  pair; the default is a member of the supported set by construction
  (createI18n refuses otherwise).
- **The supported-language set is the contract.** Adding a language
  touches: `DEFAULT_SUPPORTED_LANGUAGES`/`DEFAULT_LANGUAGE` choices,
  the `muiLocaleFor` mapping (MUI localization must exist), fixture pairs
  for every namespace, the plural coverage of every existing plural
  family in every bundle, and the parity/coverage tests. A language ships
  only when every namespace can cover it -- partial support is refused at
  registration by design.
- **Server-resolved profile locales are applied without persisting.**
  `switchLanguage(i18n, locale, null)` applies a language to a live
  instance without writing the manual-choice slot. The persisting default
  belongs to the manual language-switch UI only: the stored slot is the
  manual-choice tier of the negotiation chain and outranks the profile
  tier on later visits, so persisting a profile application (the usual
  shape: the profile resolves from `/me` after `createI18n`) would
  permanently shadow later profile changes in that browser. The recipe
  lives in the README's "Applying the profile language after creation".
- **Do not widen the DOM dependence.** All browser reads (location,
  localStorage, navigator) are guarded and injectable; tests run
  deterministically in Node. New browser touches go through the same
  inject-or-guard pattern.
- **Instance options are internal to createI18n, and only some are pinned
  by tests.** i18next's option surface is an implementation detail here
  (v26 internals that shaped the code: `initAsync` defaults true and is
  set false for synchronous readiness; supportedLngs gains an internal
  `cimode` entry that readSupportedLanguages filters; `missingKeyHandler`
  dispatches only with `saveMissing`). The "pins the discipline options"
  test in create.test.ts asserts exactly what createI18n must keep for
  the no-fallback discipline: `fallbackLng === false`, `load ===
  'currentOnly'`, `saveMissing === true`, supportedLngs extended with
  `cimode`, and a `missingKeyHandler` installed as a function.
  `initAsync: false` is asserted nowhere -- only its observable
  consequence, an instance that is synchronously ready when createI18n
  returns, is what tests and SSR-rendered first paint rely on. Consuming
  code never sets any of these; when the wrapper version moves, re-verify
  each against the runtime before changing tests, and pin `initAsync`
  while there.

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
   `locales/`-named directory with **identical nested structure**, no
   empty-string values, and every plural family carrying both `_one` and
   `_other` forms (the categories zh-CN and en-US can select; zh-CN's
   `_one` copies are the parity rule's cost, not a typo).
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
