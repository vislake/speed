/**
 * speed-aliases.mjs -- the workspace's single @speed package map, and
 * the two derivations every alias-consuming leg builds from it.
 *
 * Why a manifest: resolving @speed/* specifiers onto the workspace
 * siblings' live src (never their uncommitted dist/) needs the same
 * specifier -> source-file map in several places per consumer -- the
 * vite/vitest `resolve.alias` lists and the tsconfig `paths` -- and
 * those lists were hand-copied per consumer until each consumer read
 * them from here. A hand-copied list drifts silently (a new sibling
 * resolves only where someone remembered to add it), while a derived
 * one cannot: every consumer that imports this module sees the same
 * map, and the reference app's suite additionally pins its tsconfig
 * `paths` against `speedTsconfigPaths()` (tsconfig cannot import, so
 * that leg is checked rather than derived).
 *
 * Order is load-bearing and mirrors both resolution rules: an entry
 * whose specifier is a prefix of another must come after it, because
 * alias matching is first-match-wins ("@speed/api-sdk/runtime" before
 * "@speed/api-sdk", "@speed/i18n/mui-locale" before "@speed/i18n",
 * "@speed/product-shell/bootstrap" before "@speed/product-shell").
 * Never reorder this list; append new subpaths directly above their
 * bare package entry.
 *
 * Entry paths are workspace-root-relative (`packages/<name>/src/...`)
 * so one manifest serves every consumer depth: the packages under
 * web/packages/ and the reference app, which lives outside web/
 * exactly where a delivered consumer project lives.
 */
import { dirname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

/** Absolute path of the workspace root (web/), this module's own parent.
 * Path derivation stays on node:path rather than URL resolution: the
 * jsdom test environment's global URL constructor is not Node's, so a
 * relative URL resolution against a file: base is not reliable there
 * (the same reasoning the packages' manifest-drift tests document). */
const WORKSPACE_ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')

/**
 * The workspace's @speed specifiers and the src file each resolves to,
 * in resolution order (see the header: subpath entries before their
 * prefixes). The vite/vitest alias lists and the tsconfig `paths` maps
 * of every consumer derive from exactly this list.
 */
export const SPEED_PACKAGE_MAP = Object.freeze([
  {
    specifier: '@speed/api-sdk/runtime',
    entry: 'packages/api-sdk/src/runtime.ts',
  },
  { specifier: '@speed/api-sdk', entry: 'packages/api-sdk/src/index.ts' },
  {
    specifier: '@speed/api-client/react',
    entry: 'packages/api-client/src/react.ts',
  },
  { specifier: '@speed/api-client', entry: 'packages/api-client/src/index.ts' },
  { specifier: '@speed/auth-core', entry: 'packages/auth-core/src/index.ts' },
  { specifier: '@speed/auth-ui', entry: 'packages/auth-ui/src/index.ts' },
  { specifier: '@speed/billing-ui', entry: 'packages/billing-ui/src/index.ts' },
  { specifier: '@speed/tenancy-ui', entry: 'packages/tenancy-ui/src/index.ts' },
  { specifier: '@speed/account-ui', entry: 'packages/account-ui/src/index.ts' },
  {
    specifier: '@speed/product-shell/bootstrap',
    entry: 'packages/product-shell/src/bootstrap.tsx',
  },
  {
    specifier: '@speed/product-shell',
    entry: 'packages/product-shell/src/index.ts',
  },
  { specifier: '@speed/layout-kit', entry: 'packages/layout-kit/src/index.ts' },
  {
    specifier: '@speed/i18n/mui-locale',
    entry: 'packages/i18n/src/mui-locale.ts',
  },
  { specifier: '@speed/i18n', entry: 'packages/i18n/src/index.ts' },
  { specifier: '@speed/tokens', entry: 'packages/tokens/src/index.ts' },
  { specifier: '@speed/ui-kit', entry: 'packages/ui-kit/src/index.ts' },
])

/** Absolute path of one map entry's source file. */
function entryPath(entry) {
  return join(WORKSPACE_ROOT, entry)
}

/**
 * The map as vite/vitest `resolve.alias` entries: `{ find, replacement }`
 * with absolute replacements, order preserved. This is what a consumer's
 * vite.config.ts / vitest.config.ts drops into its `resolve.alias`.
 */
export function speedAliases(map = SPEED_PACKAGE_MAP) {
  return map.map(({ specifier, entry }) => ({
    find: specifier,
    replacement: entryPath(entry),
  }))
}

/**
 * The map as a tsconfig `paths` object with entries relative to the
 * consumer's own tsconfig directory (POSIX separators, leading './'
 * stripped the way tsc writes them -- relative() already yields a
 * leading '..' at every consumer depth here). tsconfig files cannot
 * import this module, so a consumer's `paths` is pinned against this
 * function by a drift test instead of derived at load time; a consumer
 * that generates its tsconfig can emit the return value verbatim.
 */
export function speedTsconfigPaths(consumerDir, map = SPEED_PACKAGE_MAP) {
  const paths = {}
  for (const { specifier, entry } of map) {
    const rel = relative(consumerDir, entryPath(entry)).split(sep).join('/')
    paths[specifier] = [rel.startsWith('.') ? rel : `./${rel}`]
  }
  return paths
}
