---
title: "@speed/tokens"
weight: 1
description: "The design-token tree as pure, dependency-free data — the typed defaultTokens assembly and the copy-on-write deepMerge override mechanism that @speed/ui-kit's createAppTheme maps onto an MUI v9 theme."
---

# @speed/tokens

The platform's design tokens as pure, dependency-free data: types, the
`defaultTokens` tree, and the `deepMerge` override mechanism. Zero
runtime dependencies, no React, no CSS-in-JS — consumers (the ui-kit
theme factory first among them, but also an app that needs raw values)
import data and types only.

## What it is for

`@speed/tokens` owns the single source of visual truth every speed
frontend starts from:

- **Semantic palette** — six roles, each with
  `main`/`light`/`dark`/`contrastText`, plus a neutral slate ramp from
  50 to 950 and the text/background/divider surfaces.
- **Typography** — Latin-first font stacks that end in CJK-capable
  fallbacks, sizes from 12 to 48px, weights, line heights and letter
  spacing.
- **Spacing** — one unit, 8px.
- **Shape** — border radius, 8px.
- **Breakpoints and z-index** — the `xs`..`xl` values and the eight
  MUI z-index slot names with their MUI-default values.
- **Shadows** — layered rgba shadows for the six elevation slots
  (1/2/4/8/16/24).

The assembled `defaultTokens` tree is immutable by convention *and* by
runtime: the sections are `readonly` in the types, and the tree is
deep-frozen at module load. Overrides never happen by mutation — they
happen by `deepMerge`, the copy-on-write mechanism the package also
owns, whose result shares untouched branches with the default tree by
identity and rebuilds the touched ones.

It is **not** a theme package: nothing here knows MUI, React or CSS.
It carries no user-facing text and ships no locales — it is data, not
UI.

## When to choose it

Any host that builds on the speed frontend uses it, whether directly
or through `@speed/ui-kit`, whose `createAppTheme` consumes
`defaultTokens` as its base layer. Use it directly when you need raw
token values in non-MUI code, or when your product needs a project- or
tenant-specific visual identity: each override layer is a `deepMerge`
diff over the defaults, so a white-label tenant override can be a
handful of tokens rather than a theme fork.

## Wiring and minimal use

```ts
import { defaultTokens, deepMerge, type TokensOverride } from '@speed/tokens'

const override: TokensOverride = {
  color: { semantic: { primary: { main: '#0F766E' } } },
  zIndex: { values: { drawer: 1400 } },
}
const tokens = deepMerge(defaultTokens, override)
```

`TokensOverride` is `DeepPartial<SpeedTokens>`, so shape drift — an
unknown section, a string where a hex belongs — is a **compile-time**
error, never a runtime surprise. Overriding a whole branch or one
token is the same call; layers compose because later overrides win.

## Core API and usage essentials

- **`defaultTokens`** — the assembled tree, deep-frozen at assembly.
  A write through the tree itself — or through any branch a `deepMerge`
  result shares with it by identity — throws in strict mode instead of
  silently polluting the base every override starts from.
- **`deepMerge(base, ...overrides)`** — the override mechanism, with
  semantics pinned by the package's tests: no input mutation (copy-on-
  write: untouched branches keep their identity, touched branches are
  rebuilt); `undefined` override values are skipped, so a partial
  override can never blank a token; plain objects merge recursively
  while arrays and every other value replace wholesale; later
  overrides win; a hostile `__proto__` key lands as a non-enumerable
  own property, invisible to every copy surface, so neither the
  result's prototype nor any downstream spread copy can be polluted.
- **MUI-parity rows pinned by tests.** The token rows that must agree
  with MUI — `breakpoints.values` and `zIndex.values` (the spacing
  unit, 8, is MUI's own too) — are tested against the installed MUI
  theme's real `createTheme` defaults (a dev-only dependency, so the
  package itself stays zero-dependency at runtime). An MUI major that
  changes one of those defaults fails here, in the token package,
  before any theme adapter ships silently-wrong chrome.
- **One recorded deviation.** `shape.borderRadius` ships 8 where MUI
  defaults to 4 — a deliberate product decision, and its test fails if
  MUI's default ever converges on the token value.

## Boundaries and pitfalls

- Tokens are data; the **adapter decisions** — how the tree maps onto
  an MUI theme (neutral ramp to `palette.grey` aliasing, typography
  roles, the floored 25-slot shadow ramp) — live in
  [ui-kit](/docs/user-guide/modules/web/ui-kit/)'s `createAppTheme`,
  documented and pinned there. Tone 950 of the neutral ramp has no MUI
  grey slot and is deliberately not mapped.
- Do not mutate a merged result's shared branches: the shared branches
  are the frozen default tree's own nodes, and writing through them
  throws (in strict mode) rather than corrupting the base. Rebuild via
  a fresh override instead.
- `deepMerge` is defined **over** `SpeedTokens`; for merging data of
  your own shape, use your own merge — the hostile-key and
  no-mutation guarantees here are specific to this tree's assembly.
- No i18n resources ship with this package; there is no namespace to
  register and no text to translate.

## Source

- [web/packages/tokens/README.md](https://github.com/vislake/speed/blob/main/web/packages/tokens/README.md) — the authoritative document (tree sections, parity table, merge semantics)
- [web/packages/tokens/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tokens/AGENTS.md) — package rules and recorded decisions
- Related: the theme factory and components that consume this tree, [ui-kit](/docs/user-guide/modules/web/ui-kit/), and the frontend narrative in [Building the frontend](/docs/user-guide/domains/frontend-building/)
