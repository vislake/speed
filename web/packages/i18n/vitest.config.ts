/**
 * i18n test configuration: coverage only -- the i18n host's tests run
 * in the default node environment (react-i18next instance creation
 * needs no DOM) and src/ imports only relative files, so no aliases
 * are needed.
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
