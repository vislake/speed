/**
 * auth-core test configuration: the plain node environment by default
 * (the session state machine is pure async logic -- no DOM); the DOM
 * suites opt in per file with the @vitest-environment jsdom pragma.
 * The workspace-sibling source aliases mirror tsconfig.json's paths.
 *
 * The aliases point @speed/api-client and @speed/api-sdk specifiers at
 * the siblings' src entry files so tests run against live sources (a
 * sibling's dist/ is never committed and not guaranteed to exist when
 * tests run). List order matters: the "@speed/api-sdk/runtime" subpath
 * entry must be tried before its "@speed/api-sdk" prefix.
 */
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vitest/config'

const sibling = (path: string): string =>
  fileURLToPath(new URL(path, import.meta.url))

export default defineConfig({
  resolve: {
    alias: [
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
})
