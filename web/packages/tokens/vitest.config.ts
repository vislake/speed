/**
 * tokens test configuration: coverage only -- the design tokens are
 * dependency-free pure data and their tests run in the default node
 * environment with no aliases, since src/ imports only relative files.
 */
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    // Coverage: v8 over src/** only, so aliased sibling sources never
    // count toward this package's numbers; text-summary for local runs,
    // json-summary for the per-package coverage gate.
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      reporter: ['text-summary', 'json-summary'],
    },
  },
})
