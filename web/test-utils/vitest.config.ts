/**
 * Test configuration for the workspace's shared test-support package.
 *
 * Node environment: the package's own suite drives the session harness,
 * which touches no DOM. The helpers that do need a DOM (axe.ts,
 * render.tsx, matchMedia.ts) are exercised by the consuming packages'
 * suites, which run under their own jsdom configs -- this package
 * carries no second, duplicative suite for them.
 *
 * The aliases come from the workspace's single package map
 * (web/scripts/speed-aliases.mjs), the same derivation every consuming
 * package's config uses, so the @speed specifiers the harness imports
 * resolve onto the siblings' live src here exactly as they do there.
 *
 * No coverage thresholds, deliberately: this package's src is driven by
 * the consumers' suites (each of which enforces the workspace's 80%
 * floor over its own src, the src these helpers are imported into),
 * while the package's own suite pins the harness regression below. A
 * threshold over src/** here would demand a second suite for helpers
 * the consumers already drive.
 */
import { defineConfig } from 'vitest/config'
import { speedAliases } from '../scripts/speed-aliases.mjs'

export default defineConfig({
  resolve: {
    alias: speedAliases(),
  },
  test: {
    environment: 'node',
    coverage: {
      provider: 'v8',
      include: ['src/**'],
      reporter: ['text-summary', 'json-summary'],
    },
  },
})
