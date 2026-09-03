/**
 * layout-kit test configuration: jsdom DOM environment with the shared
 * jest-dom matcher setup, plus the workspace-sibling source aliases that
 * mirror tsconfig.json's paths.
 *
 * The aliases point @speed/i18n and @speed/ui-kit specifiers at the
 * siblings' src entry files so tests run against live sources (a
 * sibling's dist/ is never committed and not guaranteed to exist when
 * tests run); @speed/tokens is aliased too since ui-kit's own theme
 * module imports it -- this package never imports @speed/tokens itself
 * (see tsconfig.json's paths comment). List order matters: the
 * "@speed/i18n/mui-locale" subpath entry must be tried before its
 * "@speed/i18n" prefix.
 */
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

const sibling = (path: string): string => fileURLToPath(new URL(path, import.meta.url))

export default defineConfig({
  resolve: {
    alias: [
      {
        find: '@speed/i18n/mui-locale',
        replacement: sibling('../i18n/src/mui-locale.ts'),
      },
      {
        find: '@speed/i18n',
        replacement: sibling('../i18n/src/index.ts'),
      },
      {
        find: '@speed/ui-kit',
        replacement: sibling('../ui-kit/src/index.ts'),
      },
      {
        find: '@speed/tokens',
        replacement: sibling('../tokens/src/index.ts'),
      },
    ],
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./test-utils/setup.ts'],
  },
})
