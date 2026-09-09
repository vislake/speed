/**
 * reference-app-web test configuration: jsdom DOM environment with the
 * shared jest-dom matcher setup, plus the workspace-sibling source
 * aliases that mirror tsconfig.json's paths.
 *
 * The aliases point @speed/* specifiers at the siblings' src entry files
 * so tests run against live sources (a sibling's dist/ is never
 * committed and not guaranteed to exist when tests run). Relative depth:
 * this app lives outside web/, so the sibling paths climb out of
 * examples/reference-app/web to the workspace root first. The app's src
 * composes every @speed package; the alias list covers the specifiers
 * the transitive graph can import -- ui-kit's own sources import
 * @speed/tokens and @speed/i18n/mui-locale, the generated api-sdk hooks
 * import @tanstack/react-query (resolved from api-sdk's own
 * node_modules), and hosts bind through @speed/api-sdk's runtime
 * subpath. Aliases match by prefix, so a subpath entry must come before
 * the entry that is its prefix: "@speed/api-sdk/runtime" before
 * "@speed/api-sdk", and "@speed/i18n/mui-locale" before "@speed/i18n".
 */
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

const sibling = (path: string): string =>
  fileURLToPath(new URL(path, import.meta.url))

export default defineConfig({
  resolve: {
    alias: [
      {
        find: '@speed/api-sdk/runtime',
        replacement: sibling('../../../web/packages/api-sdk/src/runtime.ts'),
      },
      {
        find: '@speed/api-sdk',
        replacement: sibling('../../../web/packages/api-sdk/src/index.ts'),
      },
      {
        find: '@speed/api-client/react',
        replacement: sibling('../../../web/packages/api-client/src/react.ts'),
      },
      {
        find: '@speed/api-client',
        replacement: sibling('../../../web/packages/api-client/src/index.ts'),
      },
      {
        find: '@speed/auth-core',
        replacement: sibling('../../../web/packages/auth-core/src/index.ts'),
      },
      {
        find: '@speed/auth-ui',
        replacement: sibling('../../../web/packages/auth-ui/src/index.ts'),
      },
      {
        find: '@speed/tenancy-ui',
        replacement: sibling('../../../web/packages/tenancy-ui/src/index.ts'),
      },
      {
        find: '@speed/account-ui',
        replacement: sibling('../../../web/packages/account-ui/src/index.ts'),
      },
      {
        find: '@speed/product-shell',
        replacement: sibling('../../../web/packages/product-shell/src/index.ts'),
      },
      {
        find: '@speed/layout-kit',
        replacement: sibling('../../../web/packages/layout-kit/src/index.ts'),
      },
      {
        find: '@speed/i18n/mui-locale',
        replacement: sibling('../../../web/packages/i18n/src/mui-locale.ts'),
      },
      {
        find: '@speed/i18n',
        replacement: sibling('../../../web/packages/i18n/src/index.ts'),
      },
      {
        find: '@speed/tokens',
        replacement: sibling('../../../web/packages/tokens/src/index.ts'),
      },
      {
        find: '@speed/ui-kit',
        replacement: sibling('../../../web/packages/ui-kit/src/index.ts'),
      },
    ],
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
