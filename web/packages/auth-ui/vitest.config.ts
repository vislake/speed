/**
 * auth-ui test configuration: jsdom DOM environment with the shared
 * jest-dom matcher setup, plus the workspace-sibling source aliases that
 * mirror tsconfig.json's paths.
 *
 * The aliases point @speed/* specifiers at the siblings' src entry files
 * so tests run against live sources (a sibling's dist/ is never committed
 * and not guaranteed to exist when tests run). src/ resolves
 * @speed/auth-core and @speed/i18n; test-utils/ additionally imports the
 * ui-kit theme providers -- whose own sources import @speed/tokens and
 * @speed/i18n/mui-locale, aliased below for the same reason -- and
 * drives sessions through the api-client and api-sdk seam. Aliases match
 * by prefix, so a subpath entry must come before the entry that is its
 * prefix: "@speed/i18n/mui-locale" before "@speed/i18n", and
 * "@speed/api-sdk/runtime" before "@speed/api-sdk".
 */
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

const sibling = (path: string): string =>
  fileURLToPath(new URL(path, import.meta.url))

export default defineConfig({
  resolve: {
    alias: [
      {
        find: '@speed/auth-core',
        replacement: sibling('../auth-core/src/index.ts'),
      },
      {
        find: '@speed/i18n/mui-locale',
        replacement: sibling('../i18n/src/mui-locale.ts'),
      },
      {
        find: '@speed/i18n',
        replacement: sibling('../i18n/src/index.ts'),
      },
      {
        find: '@speed/tokens',
        replacement: sibling('../tokens/src/index.ts'),
      },
      {
        find: '@speed/ui-kit',
        replacement: sibling('../ui-kit/src/index.ts'),
      },
      {
        find: '@speed/api-client',
        replacement: sibling('../api-client/src/index.ts'),
      },
      {
        find: '@speed/api-sdk/runtime',
        replacement: sibling('../api-sdk/src/runtime.ts'),
      },
      {
        find: '@speed/api-sdk',
        replacement: sibling('../api-sdk/src/index.ts'),
      },
    ],
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./test-utils/setup.ts'],
  },
})
