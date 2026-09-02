# @speed/i18n

The platform's i18n layer for web packages: one react-i18next-wrapped
instance per host, a negotiated start language, per-package namespaces
registered with parity and coverage validation, and a missing-key
discipline with **no silent cross-language fallback** -- the frontend
counterpart of `go/pkgcore/i18n`'s catalog. The MUI locale helper lives at
`@speed/i18n/mui-locale`.

## Language negotiation

The start language is decided by a chain, first match wins, every source
validated against the instance's supported set and skipped when it matches
nothing:

1. **URL parameter** (`?lang=...`, override for share links; parameter name
   configurable, disable with `urlParameterName: null`).
2. **Persisted choice** -- the manual language switch writes through
   `switchLanguage`; stored under `SPEED_LOCALE_STORAGE_KEY` in the bound
   storage (browser localStorage by default, injectable, opt-out with
   `storage: null`).
3. **Profile language** -- the signed-in user's stored locale. M0 has no
   profile feature: this slot is the documented extension point the M1
   user-profile step feeds. Hosts that can resolve a profile locale pass
   it to `createI18n` and it outranks the browser.
4. **Navigator languages**, in preference order.
5. **The default language** -- `zh-CN`.

An unknown browser language resolving to the zh-CN default is a deliberate
negotiation default (this product's home language), documented and
overridable. It is a different rule from the missing-key rule below, which
never falls back.

```ts
import { createI18n, switchLanguage } from '@speed/i18n'

const i18n = createI18n() // negotiates, synchronous; react-ready instance
// language switch UI:
await switchLanguage(i18n, 'en-US') // persists the canonical choice
```

## React bindings

The react-i18next bindings are re-exported from the main entry, so hosts
and component packages consume the whole i18n surface through one
`@speed` package (lockstep single-version shipping pins the module
identity -- no host can end up with two react-i18next copies, which is
what makes `useTranslation` inside third-party components safe):

```tsx
import { I18nextProvider, useTranslation } from '@speed/i18n'

export function App() {
  return <I18nextProvider i18n={i18n}>{/* ... */}</I18nextProvider>
}

function Greeting() {
  const { t } = useTranslation('welcome')
  return <p>{t('greeting.hello')}</p>
}
```

## Namespaces and registration

One namespace per package, in bare, unscoped form: a name matches
`[A-Za-z][A-Za-z0-9_-]*` (`welcome`, `auth`...). Scoped npm-style names
are not namespaces: `registerNamespace` refuses `@speed/tokens` -- the
pattern allows no `@` or `/`, because a namespace is a short key hosts
write in `useTranslation('...')` and override, not a package identifier.
A package that ships locale resources registers under its base name.
Resources map canonical language tags to bundles; registration is atomic
and validated before anything lands:

- every language key must be a supported language, **every supported
  language must be present** (a namespace speaking fewer languages than the
  host forces the render-as-key path on users of the missing ones);
- all bundles must carry the **same leaf key set** (parity with a
  reference language; deterministic error messages) -- a key can never
  exist in one language and silently miss in another;
- leaves must be strings; nesting is plain records only;
- a namespace registers exactly once per instance (double registration
  usually means double init in tests or SSR).

```ts
import { registerNamespace } from '@speed/i18n'
import welcomeZh from './locales/welcome/zh-CN.json'
import welcomeEn from './locales/welcome/en-US.json'

registerNamespace(i18n, 'welcome', {
  'zh-CN': welcomeZh,
  'en-US': welcomeEn,
})
// components: useTranslation('welcome') / t('greeting.hello')
```

## Missing keys: never another language's text

`createI18n` pins the guarantees on the underlying instance:
`fallbackLng: false`, `load: 'currentOnly'` (cross-language fallback is
impossible), `saveMissing: true` (i18next v26 only dispatches its
missing-key handler when this is on) and a handler that is always
installed. A key missing in the loaded language therefore:

- renders as the key itself -- never as the same key's text from another
  language;
- fires the handler with structured details
  (`MissingKeyDetails`: languages, namespace, key) -- the package's
  default handler warns visibly (`[speed-i18n]` prefixed console warning),
  or the host supplies `onMissingKey` at creation.

The registration-time parity and coverage checks are the companion
guarantee: whole-language or whole-key gaps surface at registration as
errors, and gaps that appear later (a key deleted from one language) are
the visible render-as-key + warning path -- no silent zh text under an
en-US UI. Tests prove both: registration rejects imbalanced bundles, and
a key removed from the loaded language renders as the key while the other
language's fixture text is asserted absent.

## MUI locale linkage

`createTheme` needs the MUI localization matching the active language.
Isolated in its own subpath so the main entry never imports MUI:

```ts
import { createTheme } from '@mui/material/styles'
import { muiLocaleFor } from '@speed/i18n/mui-locale'

const theme = createTheme(baseTheme, muiLocaleFor(i18n.language))
```

`muiLocaleFor` throws on unknown tags rather than silently pairing a
Chinese UI with English locale text. It is identity-stable per language.

## Dependencies

| Package | Kind | Why |
|---|---|---|
| `react` | peer (required, ^18 or ^19) | the main entry installs `initReactI18next`, so the instance is the react-i18next instance hosts render with; react-i18next imports React at runtime. devDependency for the test suite. |
| `@mui/material` | peer (optional, ^9) | only `./mui-locale` imports `@mui/material/locale`. Optional-peer means consumers without MUI never install or resolve it; consumers importing the mui-locale subpath must have MUI in their own dependencies. devDependency pins the tested version. |
| `i18next` / `react-i18next` | dependencies | the engine; pinned ranges are the tested compatibility window. |

## Error index

All validation failures throw `Error` messages prefixed `[speed-i18n]` with
the actionable fix inline: an unsupported language names the supported
set; a parity gap lists the missing or extra leaf paths; a switch to an
unsupported language lists the supported tags; registering on a bare
i18next instance (no pinned supported set) names `createI18n` as the fix.
The missing-key handler is the only non-throwing surface, by design (a
production lookup must degrade visibly, not crash).

## Development

From `web/packages/i18n`: `pnpm lint`, `pnpm typecheck`, `pnpm test`
(63 tests), `pnpm build`. Bilingual fixtures live under
`test-utils/locales/` (repo CJK-scanner exemption); sources and tests
assert against imported fixtures. `test-utils/` is test-only and never
emitted into `dist/`.
