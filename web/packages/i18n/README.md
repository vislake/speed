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
3. **Profile language** -- the signed-in user's stored locale. The platform
   ships no profile feature yet: this slot exists for hosts that can
   resolve a profile locale, and it outranks the browser (the URL
   parameter and a persisted manual choice still outrank it, in the chain
   order above).
4. **Navigator languages**, in preference order.
5. **The default language** -- `zh-CN`.

### Applying the profile language after creation

`profileLanguage` is a creation-time input, but the profile locale's
usual source is a `/me` round trip that resolves only after the instance
exists. The one API that applies a language to a live instance is
`switchLanguage`, whose optional storage argument is three-state:
`undefined` uses the storage bound at creation, `null` persists nothing,
and a `StorageLike` persists to that store instead. Which state a caller
means is not a mechanics detail:

- the **manual language-switch UI** persists (the default): the stored
  slot is the manual-choice tier of the negotiation chain, and a manual
  choice is the most recent user intent;
- a host **applying a server-resolved profile locale** must use the
  non-persisting form -- `switchLanguage(i18n, locale, null)`. Persisting
  a profile application writes the manual slot, and the stored choice
  outranks the profile tier on every later visit by design: a subsequent
  profile change would then be shadowed in that browser until the manual
  slot is overwritten or cleared.

```ts
// host resolves the profile after creation (e.g. from /me):
await switchLanguage(i18n, profile.locale, null) // apply, never persist
```

Matching relaxes language subtags in both directions, never crossing
languages: an exact tag matches first, a subtagged request selects a
supported bare tag of the same language (`ja-JP` selects supported `ja`),
and a bare request selects the unique supported tag carrying it (`en`
selects `en-US` unless several supported tags share the primary subtag).

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

`switchLanguage` writes the choice to storage first and then switches the
instance; persistence is best-effort by design. A storage write failure
(quota, disabled storage, embedded contexts) never fails or aborts the
switch -- it is reported by a `[speed-i18n]` console warning and the
instance still switches, so the promise resolves exactly when the language
changed and a rejection always means the switch did not happen. Reads are
protected the same way: a throwing storage read at creation falls back to
"no stored choice", never to a failed creation.

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
- leaves must be **non-empty strings**; nesting is plain records only. An
  empty translation renders as silence and never fires the missing-key
  discipline, so at runtime it is indistinguishable from a dropped key --
  `registerNamespace` refuses `""` the same way the Go catalog refuses
  empty translations;
- **plural forms are suffixed leaves** (`key_one`, `key_other`, ...,
  resolved per count) and count as ordinary keys for the parity rule. A
  family of such leaves must cover every count category the instance's
  supported languages can select -- registration validates this through
  the same `Intl.PluralRules` resolution the renderer uses -- because a
  count whose form is absent would render the raw key. Since bundles
  carry identical leaf sets, the forms one language needs ship in every
  language's bundle: zh-CN carries `_one` forms that en-US counts of 1
  select and that zh-CN itself never does;
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
errors. A gap that appears after registration cannot be driven through
this package's API -- registerNamespace validates key-set parity and
refuses a second registration, and i18next's own removal is per
language-and-namespace, never per key -- so the tests prove the render
path by removing the whole en-US bundle of a registered namespace
(instance.removeResourceBundle) while zh-CN still holds the key, then
rendering in en-US: the key renders as itself, the zh-CN text is asserted
absent, and the missing-key warning fires. A genuine single-key deletion
would take that same render path, but no package API can express it.

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
the actionable fix inline: a non-canonical entry in the supported set at
creation names its canonical spelling (or says it is not a language tag);
an unsupported language names the supported set; a parity gap lists the
missing or extra leaf paths; an empty translation names its leaf path; an
incomplete plural family names the family, the languages whose counts
would spill the raw key and the missing `_<category>` forms; a switch to
an unsupported language lists the supported tags; registering on a bare
i18next instance (no pinned supported set) names `createI18n` as the fix.
Storage is deliberately best-effort, by design: a failing write warns
(`[speed-i18n]` console warning) without failing the switch, and a failing
read at creation silently means "no stored choice". The missing-key
handler is the other non-throwing surface (a production lookup must
degrade visibly, not crash).

## Development

From `web/packages/i18n`: `pnpm lint`, `pnpm typecheck`, `pnpm test`,
`pnpm build`. Bilingual fixtures live under
`test-utils/locales/` (repo CJK-scanner exemption); sources and tests
assert against imported fixtures. `test-utils/` is test-only and never
emitted into `dist/`.
