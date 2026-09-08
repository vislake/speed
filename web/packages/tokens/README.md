# @speed/tokens

The platform's design tokens as pure, dependency-free data: types, the
`defaultTokens` tree, and the `deepMerge` override mechanism. Zero runtime
dependencies, no React, no CSS-in-JS -- consumers (the `ui-kit`
theme factory, apps that need raw values) import data and types only.

## Contents

| Module | Section of the token tree |
|---|---|
| `color.ts` | Semantic palette (6 roles x main/light/dark/contrastText), neutral ramp (slate, 50..950), text/background/divider |
| `typography.ts` | Font families (Latin-first stacks ending in CJK-capable fallbacks), font sizes 12..48px, weights, line heights, letter spacing |
| `spacing.ts` | Spacing unit (8px base) |
| `shape.ts` | Border radius (single 8px value) |
| `breakpoints.ts` | xs..xl values |
| `z-index.ts` | The eight MUI z-index slot names, MUI-default values |
| `shadows.ts` | Layered rgba box shadows for elevation slots 1/2/4/8/16/24 |
| `types.ts` | `SpeedTokens`, `DeepPartial`, `TokensOverride` |
| `defaultTokens.ts` | The assembled tree, deep-frozen at assembly |
| `merge.ts` | `deepMerge(base, ...overrides)` |

The default tree is immutable by convention *and* by runtime: sections are
`readonly` in the types, and the assembled tree is deep-frozen at module
load. A write attempt through `defaultTokens` itself -- or through any
branch a `deepMerge` result shares with it by identity -- throws in strict
mode instead of silently polluting the tree every override starts from.
Tests deep-freeze inputs to prove `deepMerge` never mutates.

## Why the naming mirrors the MUI theme shape

The tokens are deliberately structured so the `createAppTheme`
adapter in `ui-kit` maps them onto the MUI theme **without contortions**:

| Token section | MUI theme slot | Parity today |
|---|---|---|
| `color.semantic.<role>.{main,light,dark,contrastText}` | `palette.<role>.{main,light,dark,contrastText}` | same shape |
| `color.neutral` | `palette.grey` equivalent | values chosen to read as the neutral ramp; adapter decision |
| `typography.fontFamily.sans/mono` | `typography.fontFamily` | stack carries CJK fallbacks |
| `typography.fontSize/lineHeight/letterSpacing` | font size/weight/line-height slots | adapter decision |
| `spacing.unit` | `theme.spacing` base | equal (8) |
| `shape.borderRadius` | `theme.shape.borderRadius` | **not equal**: tokens ship 8; MUI defaults to 4. Deliberate product deviation, pinned as one |
| `breakpoints.values` | `theme.breakpoints.values` | **equal by contract** (tests pin it) |
| `zIndex.values` | `theme.zIndex` | **slot names and values equal by contract** |
| `shadows` | `theme.shadows` | token decision is the layered shadow per elevation; interpolation to MUI's 25-slot array is an adapter decision |

The equal-by-contract rows (spacing unit, breakpoints, z-index) are pinned
against the installed MUI theme itself: the tests import MUI's real
`createTheme` (dev-only `@mui/material`, so the package keeps its zero
runtime dependencies) and compare the token rows to the live defaults. A
stale in-tree expectation could only agree with the tokens it was copied
from, so an MUI upgrade that changes a default now fails loudly in this
package -- before any adapter ships silently-wrong chrome. `shape.borderRadius`
is the recorded exception (8 where MUI ships 4), and the deviation test
fails if MUI's default ever converges on the token value.

## deepMerge lives here -- why

Merge is defined **over** `SpeedTokens` (`TokensOverride = DeepPartial<...>`)
and its only current consumer is a token factory, so `tokens` is its natural
home. Putting it in `ui-kit` later would force a tokens consumer to depend on
a React-bearing package just to override a palette. Semantics, all pinned in
`merge.test.ts`:

- no input mutation; copy-on-write -- untouched branches of the base keep
  their identity, touched branches are rebuilt. Because `defaultTokens` is
  deep-frozen at assembly, the shared branches of a merge over it are
  frozen nodes too: a write through one throws in strict mode instead of
  silently reaching the default tree, while rebuilt branches stay plain
  objects owned by that result alone;
- a hostile `__proto__` key lands as a non-enumerable own property,
  deep-merged but invisible to every copy surface, so neither the result's
  prototype nor any downstream `Object.assign`/spread copy can be
  polluted;
- the result is a faithful copy of the base -- every own key of `base`
  survives an override that omits it (`undefined` skipping applies to the
  override side only);
- `undefined` override values are skipped (a partial override can never
  blank a token);
- plain objects merge recursively; arrays and every other value (null,
  strings, numbers) replace wholesale;
- later overrides win, each merged onto the result of the earlier ones.

## Using the tokens

```ts
import { defaultTokens, deepMerge, type TokensOverride } from '@speed/tokens'

const override: TokensOverride = {
  color: { semantic: { primary: { main: '#0F766E' } } },
  zIndex: { values: { drawer: 1400 } },
}
const tokens = deepMerge(defaultTokens, override)
```

Shape drift (an unknown section, a string where a hex belongs) is a
**compile-time** error via `TokensOverride`; the acceptance test in
`defaultTokens.test.ts` pins that contract with `@ts-expect-error` lines.

## Development

From `web/packages/tokens`: `pnpm lint`, `pnpm typecheck`, `pnpm test`,
`pnpm build` (emits `dist/`). No locales: this package carries
no user-facing text.
