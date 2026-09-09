/**
 * api-client test configuration: coverage only -- the core suites run
 * in the default node environment over scripted fetch, and the react
 * subpath's DOM suites opt in per file with the @vitest-environment
 * jsdom pragma. src/ imports only relative files, so no aliases are
 * needed.
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
