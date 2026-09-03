/**
 * layout-kit test configuration: jsdom DOM environment with the shared
 * jest-dom matcher setup, plus the workspace-sibling source alias that
 * mirrors tsconfig.json's paths.
 *
 * The alias points @speed/i18n specifiers at the sibling's src entry
 * files so tests run against live sources (a sibling's dist/ is never
 * committed and not guaranteed to exist when tests run). List order
 * matters: the "@speed/i18n/mui-locale" subpath entry must be tried
 * before its "@speed/i18n" prefix.
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
    ],
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./test-utils/setup.ts'],
  },
})
