/**
 * reference-app-web test configuration: jsdom DOM environment with the
 * shared jest-dom matcher setup, plus the workspace source aliases.
 *
 * The aliases come from the workspace's single package map
 * (web/scripts/speed-aliases.mjs), the same map the vite config
 * imports and the tsconfig paths section mirrors
 * (src/alias-manifest.test.ts pins that third leg): every @speed/*
 * specifier -- subpaths included -- resolves onto its sibling's live
 * src entry, because a sibling's dist/ is never committed and not
 * guaranteed to exist when tests run, and the map is the one place a
 * specifier is added or moved, so this list cannot drift. The map
 * keeps subpath entries before their prefixes, which alias matching
 * (first match wins) requires; api-sdk's runtime subpath and
 * product-shell's bootstrap subpath are the host-facing ones.
 */
import { defineConfig } from 'vitest/config'
import { speedAliases } from '../../../web/scripts/speed-aliases.mjs'

export default defineConfig({
  resolve: {
    alias: speedAliases(),
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/test-utils/setup.ts'],
    // The e2e/ directory holds the Playwright suite (playwright.config.ts),
    // whose specs import @playwright/test and drive a real browser against
    // a real server. Vitest's default include pattern would otherwise
    // collect them as unit tests -- the two tiers stay physically apart by
    // convention (.claude/skills/frontend-coding-standards/SKILL.md §12),
    // and this exclusion is what keeps `pnpm test` honest about which tier
    // it ran. Vitest's own defaults (node_modules, dist) are restated here
    // because naming `exclude` replaces them rather than extending them.
    exclude: ['e2e/**', '**/node_modules/**', '**/dist/**'],
    // Coverage: v8 over the host src/** only, so aliased sibling sources
    // never count toward this app's numbers; text-summary for local
    // runs, json-summary for the per-package coverage gate, whose floor
    // is 80% statements and 80% branches: the CI test leg runs with
    // --coverage, so dropping below either fails this app's job. The one
    // file left out of src/** is src/app-api/index.ts, the orval-
    // generated app-owned SDK output (its DO-NOT-EDIT header), which
    // @speed/api-sdk's own coverage config excludes the same way for its
    // generated src/index.ts -- the floor measures hand-written code, so
    // the hand-written src/app-api/runtime.ts seam stays measured.
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      exclude: ['src/app-api/index.ts'],
      reporter: ['text-summary', 'json-summary'],
      thresholds: {
        statements: 80,
        branches: 80,
      },
    },
  },
})
