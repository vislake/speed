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
