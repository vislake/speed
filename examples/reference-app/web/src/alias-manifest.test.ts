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
 *  - every package tsconfig paths section (the same cannot-import leg at
 *    package depth) must map each entry through the map's projection for
 *    that package, keep the map's order, cover every workspace specifier
 *    the package's own compiled sources import (discovered through the
 *    TypeScript parser), and hold no entry outside the map -- so a
 *    target a move invalidated, a stale specifier, a broken subpath
 *    order or a forgotten entry fails here instead of in a package's
 *    standalone typecheck;
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

/**
 * The packages whose tsconfig declares a paths section. Same vacuity
 * pin as ALIAS_CONSUMERS: the sweep below resolves the list from the
 * tsconfigs themselves, so a package that starts or stops declaring
 * paths changes this list in the same commit.
 */
const TSCONFIG_PATHS_PACKAGES = [
  'account-ui',
  'api-sdk',
  'auth-core',
  'auth-ui',
  'billing-ui',
  'layout-kit',
  'product-shell',
  'tenancy-ui',
  'ui-kit',
]

/**
 * Every `@speed/*` module specifier a package's own compiled sources
 * import, over the tsconfig's include set (src/ and test-utils/),
 * through the TypeScript parser rather than pattern matching -- a
 * specifier named only in a comment never counts.
 */
function importedSpeedSpecifiers(packageDir: string): Set<string> {
  const specifiers = new Set<string>()
  const walk = (dir: string): void => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name)
      if (entry.isDirectory()) {
        walk(path)
        continue
      }
      if (!entry.name.endsWith('.ts') && !entry.name.endsWith('.tsx')) {
        continue
      }
      const parsed = ts.preProcessFile(readFileSync(path, 'utf8'))
      for (const imported of parsed.importedFiles) {
        if (imported.fileName.startsWith('@speed/')) {
          specifiers.add(imported.fileName)
        }
      }
    }
  }
  for (const root of ['src', 'test-utils']) {
    const dir = join(packageDir, root)
    if (existsSync(dir)) {
      walk(dir)
    }
  }
  return specifiers
}

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

describe('every package tsconfig paths section mirrors the map', () => {
  it('maps each entry through the map, in map order, covering the package imports', () => {
    const swept: string[] = []
    const mapOrder = SPEED_PACKAGE_MAP.map((item) => item.specifier)
    for (const entry of readdirSync(packagesDir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue
      const packageDir = join(packagesDir, entry.name)
      const tsconfigPath = join(packageDir, 'tsconfig.json')
      if (!existsSync(tsconfigPath)) continue
      const parsed = ts.parseConfigFileTextToJson(
        tsconfigPath,
        readFileSync(tsconfigPath, 'utf8'),
      )
      const paths = (
        parsed.config as
          | { compilerOptions?: { paths?: Record<string, string[]> } }
          | undefined
      )?.compilerOptions?.paths
      if (paths === undefined) continue
      swept.push(entry.name)

      // Every entry's target is the map's projection for this package's
      // depth, and every entry names a map specifier -- a target a move
      // invalidated, or a specifier the map does not know, fails here.
      const projected = speedTsconfigPaths(packageDir)
      for (const [specifier, targets] of Object.entries(paths)) {
        expect(targets, `${entry.name}: ${specifier}`).toEqual(
          projected[specifier],
        )
      }

      // The entries keep the map's relative order: paths resolution is
      // first-match-wins, so a subpath below its prefix could never
      // resolve to its own file.
      const positions = Object.keys(paths).map((specifier) =>
        mapOrder.indexOf(specifier),
      )
      for (const position of positions) {
        expect(position, `${entry.name}: unknown specifier`).toBeGreaterThanOrEqual(0)
      }
      expect(positions, `${entry.name}: order`).toEqual(
        [...positions].sort((a, b) => a - b),
      )

      // Every specifier the package's own sources import has an entry:
      // tsconfig cannot import the map, so a newly imported sibling
      // without its entry would otherwise fail only that package's
      // standalone typecheck (or resolve against an unbuilt dist/).
      for (const imported of importedSpeedSpecifiers(packageDir)) {
        expect(
          Object.hasOwn(paths, imported),
          `${entry.name} imports ${imported} without a paths entry`,
        ).toBe(true)
      }
    }
    expect(swept).toEqual(TSCONFIG_PATHS_PACKAGES)
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
