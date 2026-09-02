# AGENTS.md — @speed/tokens

## What this package is

Design tokens as pure data: `defaultTokens` (one tree), `deepMerge` (the
only mutation-free way to override it), and the types. It is the
dependency floor of the frontend -- every UI package reads values from
here, and `ui-kit`'s theme factory consumes the merged result. There are
no React bindings and no stylesheet output in this package, by design.

## Non-negotiable rules

- **Zero runtime dependencies, forever.** A dependency added here lands in
  the bundle of every consuming project. If a merge of behavior needs a
  helper, write it here (small, tested) rather than pulling one in.
- **No CSS-in-JS, no theme objects, no design-system opinions.** This
  package stores values and the merge mechanism. How tokens become a MUI
  theme (shadow-array interpolation, grey-ramp mapping, typography slot
  assignment) is an adapter decision owned by `ui-kit`; do not preempt it
  by adding adapter-shaped exports.
- **MUI-parity rows are contracts.** `breakpoints.values` and
  `zIndex.values` intentionally equal MUI's defaults, and tests pin them.
  When an MUI major changes those defaults, the test failure here tells the
  adapter round exactly what to re-decide. Never "fix" the test by
  loosening it; update the parity decision deliberately.
- **Color literals live in one place per section.** `defaultTokens`
  assembles from the section modules; the hex values are spelled only in
  `color.ts` (and friends). Duplicated literals drift.
- **Overrides type-check.** `TokensOverride` derives from `SpeedTokens`;
  unknown sections, wrong value kinds and object-for-string overrides are
  compile errors, pinned by `@ts-expect-error` lines in the tests. Keep the
  DeepPartial machinery in sync whenever `SpeedTokens` grows a section.

## Changing tokens

1. Edit the section module + `SpeedTokens` type together (types first).
2. Update `defaultTokens.ts` assembly and the acceptance test if the change
   is visible there.
3. `deepMerge` semantics do not change without a `merge.test.ts` paragraph
   documenting the new rule first.
4. Run `pnpm lint`, `pnpm typecheck`, `pnpm test`, `pnpm build` -- all
   clean. The build proves `dist/` still emits declarations for every
   export.
5. Anything that mirrors an MUI default goes through the parity table in
   the README (update the row) and a pinned test.

## CJK scanner

This package ships no user-facing text and no locales; if a fixture ever
needs language content, put it under a `locales/`-named directory (repo
rule) and import it -- never inline the text in `.ts`.
