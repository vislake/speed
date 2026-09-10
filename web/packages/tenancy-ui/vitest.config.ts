/**
 * tenancy-ui test configuration: jsdom DOM environment with the shared
 * jest-dom matcher setup, plus the workspace source aliases.
 *
 * The aliases come from the workspace's single package map
 * (web/scripts/speed-aliases.mjs) instead of a list hand-copied here:
 * every @speed/* specifier -- subpaths included -- resolves onto its
 * sibling's live src entry, because a sibling's dist/ is never
 * committed and not guaranteed to exist when tests run, and the map is
 * the one place a specifier is added or moved, so this list cannot
 * drift from what src/ and test-utils/ import. The map keeps subpath
 * entries before their prefixes, which both alias matching (first
 * match wins) and tsconfig paths resolution need.
 */
import { defineConfig } from 'vitest/config'
import { speedAliases } from '../../scripts/speed-aliases.mjs'

export default defineConfig({
  resolve: {
    alias: speedAliases(),
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./test-utils/setup.ts'],
    // Coverage: v8 over src/** only, so aliased sibling sources never
    // count toward this package's numbers; text-summary for local runs,
    // json-summary for the per-package coverage gate, whose floor is
    // 80% statements and 80% branches: the CI test leg runs with
    // --coverage, so dropping below either fails this package's job.
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      reporter: ['text-summary', 'json-summary'],
      thresholds: {
        statements: 80,
        branches: 80,
      },
    },
  },
})
