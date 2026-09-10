/**
 * alias-manifest.test.ts -- the workspace's alias wiring pinned against
 * its single package map (web/scripts/speed-aliases.mjs).
 *
 * The app's own vite and vitest configs, and the package vitest configs
 * that alias, import the map directly, so those legs cannot drift by
 * construction; this suite closes the remaining legs and the map's own
 * invariants:
 *
 *  - the app tsconfig paths section (tsconfig cannot import; it is
 *    checked, not derived) must equal the map's tsconfig projection for
 *    this app's depth, both directions;
 *  - every package vitest config that declares an alias list resolves
 *    exactly the map, loaded through the module graph rather than
 *    pattern-matched as text, so a config cannot keep a hand-copied
 *    subset that drifts from the map;
 *  - every package the workspace installs (web/packages/*) must be
 *    resolvable through the map, and every map entry must point at a
 *    file that exists -- a sibling added without a map entry, or an
 *    entry left behind by a move, fails here instead of surfacing as a
 *    resolution error in one consumer;
 *  - the resolution-order invariant: a specifier that is a subpath of
 *    another must come before it (first match wins in alias matching
 *    and in tsconfig paths);
 *  - the derivation itself is data-driven: appending an entry to the
 *    map shows up in the aliases, so the pipeline has no second,
 *    hidden source.
 */

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import ts from 'typescript'
import { describe, expect, it } from 'vitest'
import {
  SPEED_PACKAGE_MAP,
  speedAliases,
  speedTsconfigPaths,
} from '../../../../web/scripts/speed-aliases.mjs'
import type { SpeedPackageEntry } from '../../../../web/scripts/speed-aliases.mjs'

const appDir = join(dirname(fileURLToPath(import.meta.url)), '..')
const webRoot = join(appDir, '..', '..', '..', 'web')
const packagesDir = join(webRoot, 'packages')

/**
 * The packages whose vitest config declares an alias list. The sweep
 * below resolves this list from the configs themselves, so naming it
 * here is what keeps the sweep from passing vacuously: a package that
 * starts or stops aliasing changes this list in the same commit, and
 * every name on it is asserted to derive its list from the map.
 */
const ALIAS_CONSUMERS = [
  'account-ui',
  'auth-core',
  'auth-ui',
  'billing-ui',
  'layout-kit',
  'product-shell',
  'tenancy-ui',
  'ui-kit',
]

describe('the app tsconfig paths mirror the workspace package map', () => {
  it('equals the map projected for this app', () => {
    const tsconfigPath = join(appDir, 'tsconfig.json')
    const parsed = ts.parseConfigFileTextToJson(
      tsconfigPath,
      readFileSync(tsconfigPath, 'utf8'),
    )
    const paths = (
      parsed.config as
        | { compilerOptions?: { paths?: Record<string, string[]> } }
        | undefined
    )?.compilerOptions?.paths
    expect(paths).toEqual(speedTsconfigPaths(appDir))
  })
})

describe('every package alias list derives from the map', () => {
  it('holds exactly the map in each config that declares an alias list', async () => {
    const consumers: string[] = []
    for (const entry of readdirSync(packagesDir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue
      const configPath = join(packagesDir, entry.name, 'vitest.config.ts')
      if (!existsSync(configPath)) continue
      // The config is loaded through the module graph rather than
      // pattern-matched as text: the assertion is the alias list the
      // config itself produces, so it holds however the file is
      // formatted and fails on any list the map does not derive.
      const loaded = (await import(
        `../../../../web/packages/${entry.name}/vitest.config.ts`
      )) as {
        default?: { resolve?: { alias?: unknown } }
      }
      const alias = loaded.default?.resolve?.alias
      if (alias === undefined) continue
      consumers.push(entry.name)
      expect(alias, entry.name).toEqual(speedAliases())
    }
    // A config that stops declaring aliases leaves the sweep above with
    // one less leg to prove, so the set it must reach is pinned: adding
    // or dropping a consumer edits this list in the same commit.
    expect(consumers).toEqual(ALIAS_CONSUMERS)
  })
})

describe('the workspace package map is complete and well-ordered', () => {
  it('covers every package the workspace installs', () => {
    const names = readdirSync(packagesDir, { withFileTypes: true })
      .filter((entry) => entry.isDirectory())
      .map(
        (entry) =>
          (
            JSON.parse(
              readFileSync(join(packagesDir, entry.name, 'package.json'), 'utf8'),
            ) as { name: string }
          ).name,
      )
    const specifiers = SPEED_PACKAGE_MAP.map((item) => item.specifier)
    for (const name of names) {
      expect(specifiers).toContain(name)
    }
  })

  it('points every entry at a file that exists', () => {
    for (const { specifier, entry } of SPEED_PACKAGE_MAP) {
      expect(existsSync(join(webRoot, entry)), `${specifier} -> ${entry}`).toBe(
        true,
      )
    }
  })

  it('keeps a subpath entry before the entry that is its prefix', () => {
    // Alias matching is first-match-wins, so "@speed/x/sub" must come
    // before "@speed/x" or the subpath could never resolve to its own
    // file.
    const indexOf = new Map(
      SPEED_PACKAGE_MAP.map((item, index) => [item.specifier, index]),
    )
    for (const { specifier } of SPEED_PACKAGE_MAP) {
      const slash = specifier.lastIndexOf('/')
      const prefix = specifier.slice(0, slash)
      if (slash >= 0 && indexOf.has(prefix)) {
        expect(indexOf.get(specifier)).toBeLessThan(indexOf.get(prefix) ?? -1)
      }
    }
  })
})

describe('the alias derivation is data-driven', () => {
  it('an entry appended to the map appears in the alias list', () => {
    const scratch: SpeedPackageEntry = {
      specifier: '@speed/scratch-pkg',
      entry: 'packages/scratch-pkg/src/index.ts',
    }
    const aliases = speedAliases([...SPEED_PACKAGE_MAP, scratch])
    expect(aliases).toHaveLength(SPEED_PACKAGE_MAP.length + 1)
    expect(aliases.at(-1)).toEqual({
      find: '@speed/scratch-pkg',
      replacement: join(webRoot, 'packages/scratch-pkg/src/index.ts'),
    })
  })
})
